package server

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

	"pgit/internal/pgs"
	"pgit/internal/pgs/git"

	"golang.org/x/crypto/ssh"
)

type SSHHandler struct {
	HostKey    *rsa.PrivateKey
	PublicKey  *rsa.PublicKey
	Manager    *pgs.RepositoriesManager
	GitRoot    string
}

func NewSSHHandler(hostKeyPath string, gitRoot string, manager *pgs.RepositoriesManager) (*SSHHandler, error) {
	h := &SSHHandler{GitRoot: gitRoot, Manager: manager}
	if err := h.LoadPrivateKey(hostKeyPath); err != nil {
		return nil, err
	}
	return h, nil
}

func (s *SSHHandler) LoadPrivateKey(path string) error {
	if pgs.FileExist(path) {
		pkey, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		skey, _ := pem.Decode(pkey)
		s.HostKey, err = x509.ParsePKCS1PrivateKey(skey.Bytes)
		return err
	}

	log.Printf("SSH: host key not found, generating")
	key, err := pgs.GenerateKey()
	if err != nil {
		return err
	}
	s.HostKey = key
	pkey := pgs.KeyEncode(s.HostKey, true)
	return os.WriteFile(path, pkey, 0600)
}

func (s *SSHHandler) HandleConn(conn net.Conn) {
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(connMetadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
		PasswordCallback: func(connMetadata ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	signer, err := ssh.NewSignerFromKey(s.HostKey)
	if err != nil {
		log.Printf("SSH: NewSignerFromKey: %v", err)
		conn.Close()
		return
	}
	config.AddHostKey(signer)

	sConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		if err == io.EOF {
			log.Printf("SSH: handshaking terminated: %v", err)
		} else {
			log.Printf("SSH: handshaking error: %v", err)
		}
		return
	}
	log.Printf("SSH: connection from %s (%s)", sConn.RemoteAddr(), sConn.ClientVersion())
	go ssh.DiscardRequests(reqs)
	s.handleChannels(chans)
}

func (s *SSHHandler) handleChannels(chans <-chan ssh.NewChannel) {
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}
		ch, reqs, err := newChan.Accept()
		if err != nil {
			log.Printf("SSH: accept channel: %v", err)
			continue
		}
		go s.handleSession(ch, reqs)
	}
}

func (s *SSHHandler) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "env":
			log.Printf("SSH: env: %#v", string(req.Payload))
		case "exec":
			if len(req.Payload) < 5 {
				log.Printf("SSH: payload too short")
				return
			}
			payload := strings.SplitN(string(req.Payload[4:]), " ", 2)
			if len(payload) < 2 {
				log.Printf("SSH: invalid exec payload: %#v", payload)
				return
			}
			cmdName := payload[0]
			rawArg := strings.Trim(payload[1], "'")
			alias := strings.TrimSuffix(rawArg, ".git")

			repo, err := s.Manager.GetByAlias(alias)
			if err != nil {
				log.Printf("SSH: unknown repo alias %q: %v", alias, err)
				return
			}
			repoPath := filepath.Join(s.GitRoot, repo.Name+".git")
			log.Printf("SSH: exec %s %s", cmdName, repoPath)

			req.Reply(true, nil)
			if err := git.HandleSSHSession(cmdName, repoPath, ch); err != nil {
				log.Printf("SSH: session %s: %v", cmdName, err)
			}
			ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			return
		default:
			return
		}
	}
}

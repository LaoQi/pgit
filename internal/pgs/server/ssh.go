package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
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
	HostKey ssh.Signer
	Manager *pgs.RepositoriesManager
	GitRoot string
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
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		signer, err := parseHostKey(data)
		if err != nil {
			return err
		}
		s.HostKey = signer
		return nil
	}

	log.Printf("SSH: host key not found, generating ed25519 key")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return err
	}
	s.HostKey = signer

	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	return os.WriteFile(path, pemBytes, 0600)
}

// parseHostKey 解析 host key PEM，兼容：
//   - 标准 PEM 格式（RSA PRIVATE KEY / EC PRIVATE KEY / OPENSSH PRIVATE KEY / PKCS8 PRIVATE KEY）
//   - 旧版 pgit 生成的 PKCS1 RSA，但 PEM Type 标记为 "PRIVATE KEY"
func parseHostKey(data []byte) (ssh.Signer, error) {
	if signer, err := ssh.ParsePrivateKey(data); err == nil {
		return signer, nil
	}
	if block, _ := pem.Decode(data); block != nil {
		if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			return ssh.NewSignerFromKey(key)
		}
	}
	return nil, errors.New("ssh: unable to parse host key")
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
	config.AddHostKey(s.HostKey)

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
			// 明确拒绝 GIT_PROTOCOL（version=2），让客户端确定性降级 v0
			log.Printf("SSH: env: %#v", string(req.Payload))
			req.Reply(false, nil)
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
			alias := strings.TrimSuffix(strings.TrimPrefix(rawArg, "/"), ".git")

			repo, err := s.Manager.GetByAlias(alias)
			if err != nil {
				log.Printf("SSH: unknown repo alias %q: %v", alias, err)
				return
			}
			repoPath := filepath.Join(s.GitRoot, repo.Name+".git")
			log.Printf("SSH: exec %s %s", cmdName, repoPath)

			// mirror 仓库禁止 push：拒绝 git-receive-pack。
			if cmdName == "git-receive-pack" && repo.IsMirror() {
				log.Printf("SSH: receive-pack denied: mirror repo %q (alias %q)", repo.Name, alias)
				req.Reply(true, nil)
				_, _ = io.WriteString(ch.Stderr(), "fatal: mirror repository: push disabled\n")
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 1})
				return
			}

			req.Reply(true, nil)
			if err := git.HandleSSHSession(cmdName, repoPath, ch); err != nil {
				log.Printf("SSH: %s %s failed: %v", cmdName, alias, err)
			} else {
				log.Printf("SSH: %s %s ok", cmdName, alias)
			}
			ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			return
		default:
			return
		}
	}
}

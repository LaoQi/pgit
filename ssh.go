package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

var allowedCommands = map[string]bool{
	"git-upload-pack":    true,
	"git-upload-archive": true,
	"git-receive-pack":   true,
}

type SSHServer struct {
	HostKey *rsa.PrivateKey
}

func (s *SSHServer) GenerateKey() (*rsa.PrivateKey, error) {
	reader := rand.Reader
	bitSize := 2048

	key, err := rsa.GenerateKey(reader, bitSize)

	if err != nil {
		return nil, err
	}
	return key, err
}

func (s *SSHServer) SaveKey(path string, key *rsa.PrivateKey, isPrivate bool) error {

	outFile, err := os.Create(path)
	if err != nil {
		return err
	}
	defer outFile.Close()

	skey := &pem.Block{}

	if isPrivate {
		skey.Type = "PRIVATE KEY"
		skey.Bytes = x509.MarshalPKCS1PrivateKey(key)
	} else {
		skey.Type = "PUBLIC KEY"
		skey.Bytes = x509.MarshalPKCS1PublicKey(&key.PublicKey)
	}

	err = pem.Encode(outFile, skey)
	return err
}

func (s *SSHServer) LoadPrivateKey(path string) error {
	// var err error
	// s.HostKey, err = s.GenerateKey()
	pkey, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}

	skey, _ := pem.Decode(pkey)
	s.HostKey, err = x509.ParsePKCS1PrivateKey(skey.Bytes)
	return err
}

func handleChannels(chans <-chan ssh.NewChannel) {
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		ch, reqs, err := newChan.Accept()
		if err != nil {
			log.Printf("Error accepting channel: %v", err)
			continue
		}

		go func(in <-chan *ssh.Request) {
			defer ch.Close()
			for req := range in {
				log.Printf("SSH: Req.Type: '%#v'", req.Type)

				switch req.Type {
				case "env":
					log.Printf("SSH: Invalid env arguments: '%#v'", string(req.Payload))
				case "exec":
					if len(req.Payload) < 5 {
						log.Printf("SSH: Payload Empty: %v", req.Payload)
						return
					}
					payload := strings.SplitN(string(req.Payload[4:]), " ", 2)
					// cmdName := payload
					log.Printf("SSH: Payload: %v", payload)
					path := filepath.Join(GetSettings().GitRoot, strings.Trim(payload[1], "':"))

					log.Printf("SSH: Payload path: %v", path)
					cmd := exec.Command(payload[0], path)
					stdout, err := cmd.StdoutPipe()
					if err != nil {
						log.Printf("SSH: StdoutPipe: %v", err)
						return
					}
					stderr, err := cmd.StderrPipe()
					if err != nil {
						log.Printf("SSH: StderrPipe: %v", err)
						return
					}
					input, err := cmd.StdinPipe()
					if err != nil {
						log.Printf("SSH: StdinPipe: %v", err)
						return
					}

					// FIXME: check timeout
					if err = cmd.Start(); err != nil {
						log.Printf("SSH: Start: %v", err)
						return
					}

					req.Reply(true, nil)
					go io.Copy(input, ch)
					io.Copy(ch, stdout)
					io.Copy(ch.Stderr(), stderr)

					if err = cmd.Wait(); err != nil {
						log.Printf("SSH: Wait: %v", err)
						return
					}

					ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					return
				default:
					return
				}
			}
		}(reqs)
	}
}

func (s *SSHServer) ListenAndServe() error {
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			// @todo auth force-command
			// if conn.User() == "foo" {
			// 	pub := base64.StdEncoding.EncodeToString(key.Marshal())
			// 	result := strings.Compare(pub, pubkey)
			// 	if result == 0 {
			// 		return nil, nil
			// 	}
			// }
			// return &ssh.Permissions{Extensions: map[string]string{"key-id": ""}}, nil
			return nil, nil
		},
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	hostKey, err := ssh.NewSignerFromKey(s.HostKey)
	if err != nil {
		return err
	}
	config.AddHostKey(hostKey)

	listener, err := net.Listen("tcp", GetSettings().getSSHListenAddr())
	if err != nil {
		return err
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("SSH: Error accepting incoming connection: %v", err)
			continue
		}
		go func() {
			log.Printf("SSH: Handshaking for %s", conn.RemoteAddr())
			sConn, chans, reqs, err := ssh.NewServerConn(conn, config)
			if err != nil {
				if err == io.EOF {
					log.Printf("SSH: Handshaking was terminated: %v", err)
				} else {
					log.Printf("SSH: Error on handshaking: %v", err)
				}
				return
			}

			log.Printf("SSH: Connection from %s (%s)", sConn.RemoteAddr(), sConn.ClientVersion())
			// The incoming Request channel must be serviced.
			go ssh.DiscardRequests(reqs)
			go handleChannels(chans)
		}()
	}
}
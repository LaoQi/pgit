package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
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

func handleConn(chans <-chan ssh.NewChannel) {
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
				payload := string(req.Payload)
				switch req.Type {
				case "env":
					args := strings.Split(strings.Replace(payload, "\x00", "", -1), "\v")
					if len(args) != 2 {
						log.Printf("SSH: Invalid env arguments: '%#v'", args)
						continue
					}
					args[0] = strings.TrimLeft(args[0], "\x04")
					cmd := exec.Command("env", fmt.Sprintf("%s=%s", args[0], args[1]))
					err := cmd.Run()
					if err != nil {
						log.Printf("env: %v", err)
						return
					}
				case "exec":
					cmdName := strings.TrimLeft(payload, " ")
					// cmdName := payload
					log.Printf("SSH: Payload: %v", cmdName)

					cmd := exec.Command(cmdName)
					// cmd.Env = append(
					// 	os.Environ(),
					// )

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
				}
			}
		}(reqs)
	}
}

func (s *SSHServer) ListenAndServe() error {
	config := &ssh.ServerConfig{
		// Config: ssh.Config{
		// 	Ciphers:      ciphers,
		// 	KeyExchanges: keyExchanges,
		// 	MACs:         macs,
		// },
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
		// Once a ServerConfig has been configured, connections can be accepted.
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("SSH: Error accepting incoming connection: %v", err)
			continue
		}

		// Before use, a handshake must be performed on the incoming net.Conn.
		// It must be handled in a separate goroutine,
		// otherwise one user could easily block entire loop.
		// For example, user could be asked to trust server key fingerprint and hangs.
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
			go handleConn(chans)
		}()
	}
	return nil
}

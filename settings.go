package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type Settings struct {
	HTTPPort     int
	HTTPAddress  string
	SSHPort      int
	SSHAddress   string
	SSHHostKey   string
	SSHPublicKey []byte
	GitRoot      string
	PathPrefix   string
	Credentials  map[string]string
}

func (s Settings) getHttpListenAddr() string {
	return fmt.Sprintf("%s:%d", s.HTTPAddress, s.HTTPPort)
}

func (s Settings) getSSHListenAddr() string {
	return fmt.Sprintf("%s:%d", s.SSHAddress, s.SSHPort)
}

var instance *Settings

func InitSettings() {
	workDir, _ := os.Getwd()
	gitRoot := filepath.Join(workDir, "repo")
	keyPath := filepath.Join(workDir, "repo", "key")
	instance = &Settings{
		GitRoot:     gitRoot,
		HTTPPort:    3000,
		HTTPAddress: "0.0.0.0",
		SSHPort:     22,
		SSHAddress:  "0.0.0.0",
		SSHHostKey:  keyPath,
		SSHPublicKey: []byte(`
-----BEGIN PUBLIC KEY-----
MIIBCgKCAQEA28/uKD4zSY/T41COOqeGosyzmKo3NDlZZK2apQ2RYqozQ4AjceHQ
gK7fASTGGnw/JhEwCUpbHQnptV/bS0qVGkFkn0vviHOFb9vzuLzqiktREcXDLXzW
wOmNcxs1EoDIcRntE94Ywphr7D5zm7K3WPO9vEQIUwWNeMKX0xXZ/Hq+G5++O+LR
X9S1nz6Fq9ydptaHZZX2eqWgv16NpWj16tWhNH72E2kVEbnNP/46r2fTdHvdZYzB
Q6c7+//7l4kmNo2IhwCIALC51OrO0aPRqg3a1K6d940bw0eDf7F1Hw5sIbCP5r6S
gx1pqzxPCb/AgJgN5GQt0yqIE7BkNfQVuQIDAQAB
-----END PUBLIC KEY-----
`),
		PathPrefix: "repo",
		Credentials: map[string]string{
			"test": "123456",
		},
	}
}

func GetSettings() *Settings {
	if instance == nil {
		panic(fmt.Errorf("Settings not init!"))
	}
	return instance
}

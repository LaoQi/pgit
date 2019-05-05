package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
)

type Setting struct {
	HTTPPort     int
	HTTPAddress  string
	SSHPort      int
	SSHAddress   string
	SSHHostKey   string
	SSHPublicKey string
	GitRoot      string
	PathPrefix   string
	Credentials  map[string]string
}

func GenerateHostKey() (string, error) {
	reader := rand.Reader
	bitSize := 2048

	key, err := rsa.GenerateKey(reader, bitSize)

	if err != nil {
		return "", err
	}
	skey := &pem.Block{}
	skey.Type = "PUBLIC KEY"
	skey.Bytes = x509.MarshalPKCS1PublicKey(&key.PublicKey)
	buffer := new(bytes.Buffer)
	pem.Encode(buffer, skey)
	return string(buffer.Bytes()), err
}

func (s *Setting) GetHttpListenAddr() string {
	return fmt.Sprintf("%s:%d", s.HTTPAddress, s.HTTPPort)
}

func (s *Setting) GetSSHListenAddr() string {
	return fmt.Sprintf("%s:%d", s.SSHAddress, s.SSHPort)
}

func (s *Setting) Output() string {
	out, err := json.MarshalIndent(s, "", "    ")
	if err != nil {
		log.Panic(err)
	}
	return string(out)
}

func (s *Setting) Reload() {
	data, err := ioutil.ReadFile("")
	if err != nil {
		panic(err)
	}
	err = json.Unmarshal(data, &s)
	if err != nil {
		panic(err)
	}
}

var Settings *Setting

func init() {
	workDir, _ := os.Getwd()
	gitRoot := filepath.Join(workDir, "repo")
	keyPath := filepath.Join(workDir, "repo", "key")
	hostKey, err := GenerateHostKey()
	if err != nil {
		log.Panic(err)
	}
	Settings = &Setting{
		GitRoot:      gitRoot,
		HTTPPort:     3000,
		HTTPAddress:  "0.0.0.0",
		SSHPort:      3022,
		SSHAddress:   "0.0.0.0",
		SSHHostKey:   keyPath,
		SSHPublicKey: hostKey,
		PathPrefix:   "repo",
		Credentials: map[string]string{
			"test": "123456",
		},
	}

	// Settings.Reload()
}

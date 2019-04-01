package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
)

type SSHServer struct {
	HostKey []byte
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

func (s *SSHServer) SavePrivateKey(path string, key *rsa.PrivateKey) error {

	outFile, err := os.Create(path)
	if err != nil {
		return err
	}
	defer outFile.Close()

	var privateKey = &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}

	err = pem.Encode(outFile, privateKey)
	return err
}

func (s *SSHServer) LoadPrivateKey(path string) error {
	return nil
}

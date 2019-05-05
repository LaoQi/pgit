package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
)

func FileExist(path string) bool {
	_, err := os.Stat(path)
	if err != nil {
		return false
	}
	return true
}

func GenerateKey() (*rsa.PrivateKey, error) {
	reader := rand.Reader
	bitSize := 2048

	key, err := rsa.GenerateKey(reader, bitSize)

	if err != nil {
		return nil, err
	}
	return key, err
}

func KeyEncode(key *rsa.PrivateKey, isPrivate bool) []byte {

	skey := &pem.Block{}

	if isPrivate {
		skey.Type = "PRIVATE KEY"
		skey.Bytes = x509.MarshalPKCS1PrivateKey(key)
	} else {
		skey.Type = "PUBLIC KEY"
		skey.Bytes = x509.MarshalPKCS1PublicKey(&key.PublicKey)
	}

	buffer := pem.EncodeToMemory(skey)
	return buffer
}

package pgs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
)

func FileExist(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func GenerateKey() (*rsa.PrivateKey, error) {
	reader := rand.Reader
	bitSize := 2048
	return rsa.GenerateKey(reader, bitSize)
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
	return pem.EncodeToMemory(skey)
}

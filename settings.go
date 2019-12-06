package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
)

type Setting struct {
	path string

	HTTPPort     int
	HTTPAddress  string
	EnableSSH    bool
	SSHPort      int
	SSHAddress   string
	SSHHostKey   string
	SSHPublicKey string
	GitRoot      string
	PathPrefix   string
	HttpAuth     bool
	SSHAuthType  string
	Credentials  map[string]string
}

func (s *Setting) SetConfigPath(path string) {
	s.path = path
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

func (s *Setting) Reload() error {
	data, err := ioutil.ReadFile(s.path)
	if err != nil {
		return err
	}
	err = json.Unmarshal(data, &s)
	if err != nil {
		return err
	}
	return nil
}

var Settings *Setting

func init() {
	workDir, _ := os.Getwd()
	gitRoot := filepath.Join(workDir, "repo")
	publicKey := filepath.Join(workDir, "repo", "key")
	hostKey := filepath.Join(workDir, "repo", "hostkey")
	Settings = &Setting{
		GitRoot:      gitRoot,
		HTTPPort:     3011,
		HTTPAddress:  "0.0.0.0",
		EnableSSH:    true,
		SSHPort:      3022,
		SSHAddress:   "0.0.0.0",
		SSHHostKey:   hostKey,
		SSHPublicKey: publicKey,
		PathPrefix:   "repo",
		HttpAuth:     false,
		SSHAuthType:  "password",
		Credentials: map[string]string{
			"user": "password",
		},
	}

}

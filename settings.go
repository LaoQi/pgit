package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Settings struct {
	HTTPPort    int
	HTTPAddress string
	SSHPort     int
	SSHAddress  string
	SSHHostKey  string
	GitRoot     string
	PathPrefix  string
	Credentials map[string]string
}

func (s Settings) getHttpListenAddr() string {
	return fmt.Sprintf("%s:%d", s.HTTPAddress, s.HTTPPort)
}

var instance *Settings
var mut sync.Mutex

func InitSettings() {
	workDir, _ := os.Getwd()
	gitRoot := filepath.Join(workDir, "repo")
	keyPath := filepath.Join(workDir, "repo", "key")
	instance = &Settings{
		GitRoot:     gitRoot,
		HTTPPort:    3000,
		HTTPAddress: "0.0.0.0",
		SSHPort:     3022,
		SSHAddress:  "0.0.0.0",
		SSHHostKey:  keyPath,
		PathPrefix:  "repo",
		Credentials: map[string]string{
			"test": "123456",
		},
	}
}

func GetSettings() *Settings {
	mut.Lock()
	defer mut.Unlock()

	if instance == nil {
		panic(fmt.Errorf("Settings not init!"))
	}
	return instance
}

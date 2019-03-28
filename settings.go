package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Settings struct {
	Address    string
	Port       int
	GitRoot    string
	PathPrefix string
	Credentials  map[string]string
}

func (s Settings) getListenAddr() string {
	return fmt.Sprintf("%s:%d", s.Address, s.Port)
}

var instance *Settings
var mut sync.Mutex

func InitSettings() {
	workDir, _ := os.Getwd()
	gitRoot := filepath.Join(workDir, "repo")
	instance = &Settings{
		GitRoot: gitRoot,
		Port:    3000,
		Address: "0.0.0.0",
		PathPrefix: "repo",
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

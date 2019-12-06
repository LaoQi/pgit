package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"

	"github.com/akamensky/argparse"
)

const (
	// Version version
	Version = "1.0.0"
)

const (
	NoError = iota
	Failed
	ConfigError
	EnvError
)

var ReloadSignal chan bool
var wait sync.WaitGroup

func serverHTTP() *http.Server {
	log.Printf("Start HTTP Server at %s", Settings.GetHttpListenAddr())
	srv := &http.Server{
		Addr:    Settings.GetHttpListenAddr(),
		Handler: NewRouters(),
	}

	go func() {
		defer wait.Done()
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("ListenAndServe : %s", err)
		}
	}()
	return srv
}

func serverSSH() *SSHServer {
	ssh, err := NewSSHServer()
	if err != nil {
		log.Printf("Failed to start SSH server: %v", err)
	}
	go func() {
		defer wait.Done()
		log.Printf("Start SSH Server at %s", Settings.GetSSHListenAddr())
		err = ssh.ListenAndServe(Settings.GetSSHListenAddr())
		if err != nil {
			log.Printf("Failed to start SSH server: %v", err)
		}
	}()
	return ssh
}

func checkEnv() error {
	// git
	gitInitCmd := exec.Command("git", "--version")
	_, err := gitInitCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git command not found, install git")
	}
	return nil
}

func main() {
	parser := argparse.NewParser("Pgit", "Personal git server")
	config := parser.String("c", "config", &argparse.Options{Default: "", Help: "config file"})

	version := parser.Flag("v", "version", &argparse.Options{Help: "show version"})
	output := parser.Flag("d", "default", &argparse.Options{Help: "print default config, eg: `pgit -d > config.json`"})
	err := parser.Parse(os.Args)
	if err != nil {
		fmt.Print(parser.Usage(err))
		os.Exit(Failed)
	}

	if *version {
		fmt.Printf("Pgit version %s\n", Version)
		os.Exit(NoError)
	}

	if *output {
		fmt.Println(Settings.Output())
		os.Exit(NoError)
	}

	if *config == "" {
		fmt.Println("Need config file, Run `pgit -h` for help")
		os.Exit(ConfigError)
	}

	err = checkEnv()
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(EnvError)
	}

	Settings.SetConfigPath(*config)
	err = Settings.Reload()
	if err != nil {
		fmt.Printf("Config parse error %s\n", err)
		os.Exit(ConfigError)
	}

	InitReposManager()

	serverHTTP()
	wait.Add(1)
	if Settings.EnableSSH {
		wait.Add(1)
		serverSSH()
	}

	wait.Wait()
	log.Panic("Never here")
}

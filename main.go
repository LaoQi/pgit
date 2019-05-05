package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/akamensky/argparse"
)

const (
	Version = "1.0.0"
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

func main() {
	parser := argparse.NewParser("Pgit", "Personal git server")
	config := parser.String("c", "config", &argparse.Options{Default: "", Help: "config file"})

	version := parser.Flag("v", "version", &argparse.Options{Help: "show version"})
	output := parser.Flag("d", "default", &argparse.Options{Help: "print default config, eg: `pgit -d > config.json`"})
	err := parser.Parse(os.Args)
	if err != nil {
		fmt.Print(parser.Usage(err))
		os.Exit(1)
	}

	if *version {
		fmt.Printf("Pgit version %s", Version)
		os.Exit(0)
	}

	if *output {
		fmt.Print(Settings.Output())
		os.Exit(0)
	}

	if *config == "" {
		fmt.Print("Need config file, Run `pgit -h` for help")
		os.Exit(4)
	}

	// for {

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

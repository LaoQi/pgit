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

func serverHTTP(wg *sync.WaitGroup) {
	wg.Add(1)
	log.Printf("Start HTTP Server at %s", Settings.GetHttpListenAddr())
	go func() {
		defer wg.Done()
		err := http.ListenAndServe(Settings.GetHttpListenAddr(), NewRouters())
		if err != nil {
			log.Fatal(err)
		}
	}()
}

func serverSSH(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ssh := &SSHServer{}
		// key, _ := ssh.GenerateKey()
		// ssh.SaveKey(filepath.Join(GetSettings().GitRoot, "private.pem"), key, true)
		// ssh.SaveKey(filepath.Join(GetSettings().GitRoot, "public.pem"), key, false)
		err := ssh.LoadPrivateKey(Settings.SSHHostKey)
		if err != nil {
			log.Printf("Failed to start SSH server: %v", err)
		}
		err = ssh.ListenAndServe(Settings.GetSSHListenAddr())
		if err != nil {
			log.Printf("Failed to start SSH server: %v", err)
		}
	}()
}

func main() {
	parser := argparse.NewParser("Pgit", "Personal git server")
	port := parser.Int("p", "port", &argparse.Options{Default: 3000, Help: "http port to serve"})
	sshport := parser.Int("s", "ssh", &argparse.Options{Default: 3022, Help: "ssh port to serve"})
	gitroot := parser.String("r", "root", &argparse.Options{Default: "repo", Help: "Repostories root path"})
	configFile := parser.String("c", "config", &argparse.Options{Default: nil, Help: "config file"})

	version := parser.Flag("v", "version", &argparse.Options{Help: "show version"})
	output := parser.Flag("o", "output", &argparse.Options{Help: "output config template"})
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
	log.Printf("%d %d %s %s %v", *port, *sshport, *configFile, *gitroot, *version)

	wg := &sync.WaitGroup{}
	serverHTTP(wg)
	serverSSH(wg)
	wg.Wait()

	log.Panic("Never here")
}

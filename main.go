package main

import (
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/akamensky/argparse"
)

func serverHttp(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := http.ListenAndServe(GetSettings().getHttpListenAddr(), NewRouters())
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
		err := ssh.LoadPrivateKey(GetSettings().SSHHostKey)
		if err != nil {
			log.Printf("Failed to start SSH server: %v", err)
		}
		err = ssh.ListenAndServe()
		if err != nil {
			log.Printf("Failed to start SSH server: %v", err)
		}
	}()
}

func main() {
	parser := argparse.NewParser("Pgit", "Personal git server")
	port := parser.Int("p", "port", &argparse.Options{Default: 3000, Help: "http port to serve"})
	sshport := parser.Int("s", "ssh", &argparse.Options{Default: 3022, Help: "ssh port to serve"})
	configFile := parser.String("c", "config", &argparse.Options{Default: nil, Help: "config file"})

	version := parser.Flag("v", "version", &argparse.Options{Help: "show version"})
	err := parser.Parse(os.Args)
	if err != nil {
		// In case of error print error and print usage
		// This can also be done by passing -h or --help flags
		panic(err)
	}
	log.Printf("%d %d %s %v", *port, *sshport, *configFile, *version)
	InitSettings()

	wg := &sync.WaitGroup{}
	serverHttp(wg)
	serverSSH(wg)
	wg.Wait()

	log.Print("Never here")
}

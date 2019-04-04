package main

import (
	"log"
	"net/http"
)

func serverHttp() {
	err := http.ListenAndServe(GetSettings().getHttpListenAddr(), NewRouters())
	if err != nil {
		log.Fatal(err)
	}
}

func serverSSH() {
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
}

func main() {

	InitSettings()

	serverHttp()
	// serverSSH()
}

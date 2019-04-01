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
	key, _ := ssh.GenerateKey()
	ssh.SavePrivateKey(GetSettings().SSHHostKey, key)
}

func main() {

	InitSettings()

	// go serverHttp()
	serverSSH()
}

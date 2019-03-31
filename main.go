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

}

func main() {

	InitSettings()

	go serverHttp()
	go serverSSH()
}

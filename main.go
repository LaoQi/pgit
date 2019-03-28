package main

import (
	"github.com/coredns/coredns/plugin/pkg/log"
	"net/http"
)

func main() {

	InitSettings()
	settings := GetSettings()

	err := http.ListenAndServe(settings.getListenAddr(), NewRouters())
	if err != nil {
		log.Fatal(err)
	}
}

package main

import (
	"net/http"
)

func main() {

	InitSettings()
	settings := GetSettings()

	http.ListenAndServe(settings.getListenAddr(), NewRouters())
}

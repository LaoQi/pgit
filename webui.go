package main

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi"
)

type WebUI struct {
}

func (v WebUI) GetRouters() chi.Router {
	r := chi.NewRouter()
	// r.Use() // some middleware..

	r.Get("/*", v.Handler())

	return r
}

func (v WebUI) Handler() http.HandlerFunc {
	workDir, _ := os.Getwd()
	filesDir := filepath.Join(workDir, "static")
	fs := http.FileServer(http.Dir(filesDir))
	return func(w http.ResponseWriter, r *http.Request) {
		fs.ServeHTTP(w, r)
	}
}

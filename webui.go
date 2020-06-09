package main

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi"
)

type WebUI struct {
}

func StaticFileMime() {
	_ = mime.AddExtensionType(".js", "application/javascript")
	_ = mime.AddExtensionType(".css", "text/css")
}

func (v WebUI) GetRouters() chi.Router {
	StaticFileMime()
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

package main

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi"
)

type View struct {
}

func (v View) GetRouters() chi.Router {
	r := chi.NewRouter()
	// r.Use() // some middleware..

	r.Get("/*", v.Handler())

	return r
}

func (v View) Handler() http.HandlerFunc {
	workDir, _ := os.Getwd()
	filesDir := filepath.Join(workDir, "static")
	fs := http.FileServer(http.Dir(filesDir))
	return func(w http.ResponseWriter, r *http.Request) {
		fs.ServeHTTP(w, r)
	}
}

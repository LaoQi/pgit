package main

import (
	"net/http"

	"github.com/go-chi/chi"
)

type Api struct {
	RepoApi *RepoApi
}

func (api Api) GetRouters() chi.Router {
	r := chi.NewRouter()
	// r.Use() // some middleware..

	r.Put("/", api.Create)
	r.Get("/", api.Get)

	return r
}

func (api Api) Create(w http.ResponseWriter, r *http.Request) {

	w.Write([]byte("aaa create"))
}

func (api Api) Get(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("aaa get"))
}

func (api Api) Update(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("aaa update"))
}

func (api Api) Delete(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("aaa delete"))
}

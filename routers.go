package main

import (
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
)

func NewRouters() chi.Router {
	r := chi.NewRouter()

	// A good base middleware stack
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// r.Use(Auth("somesome", map[string]string{
	// 	"test": "123456",
	// }))

	// Set a timeout value on the request context (ctx), that will signal
	// through ctx.Done() that the request has timed out and further
	// processing should be stopped.
	r.Use(middleware.Timeout(60 * time.Second))

	// WebUI static file
	r.Mount("/", WebUI{}.GetRouters())

	handler := NewRepoHandler()

	r.Get("/repo", handler.View)
	r.Post("/repo/{repoName}", handler.Create)
	r.Delete("/repo/{repoName}", handler.Delete)

	r.Get("/repo/{repoName}.git/info/refs", handler.InfoRefs)
	r.Post("/repo/{repoName}.git/git-{command}", handler.Command)
	r.HandleFunc("/repo/{repoName}.git/*", handler.StaticFiles)

	return r
}

package main

import (
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
)

func NewRouters() chi.Router {
	r := chi.NewRouter()

	// A good base middleware stack
	// r.Use(middleware.RequestID)
	// r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// r.Use(Auth("somesome", map[string]string{
	// 	"test": "123456",
	// }))

	// Set a timeout value on the request context (ctx), that will signal
	// through ctx.Done() that the request has timed out and further
	// processing should be stopped.
	// r.Use(middleware.Timeout(60 * time.Second))

	// WebUI static file
	r.Mount("/", WebUI{}.GetRouters())

	handler := NewRepoHandler()

	// r.Get("/_pgit")
	// r.Post("/_pgit")

	r.Get("/repo", handler.Explorer)
	r.Get("/repo/{repoName}", handler.Detail)
	r.Post("/repo/{repoName}", handler.Create)
	r.Delete("/repo/{repoName}", handler.Delete)

	r.Get("/repo/{repoName}/tree/{ref}/*", handler.Tree)
	r.Get("/repo/{repoName}/blob/{ref}/*", handler.Blob)
	r.Get("/repo/{repoName}/archive/{ref}", handler.Archive)
	r.Get("/repo/{repoName}/commit/{commit}", handler.Commit)
	r.Get("/repo/{repoName}/commits/{ref}", handler.Commits)

	r.Get("/repo/{repoName}.git/info/refs", handler.InfoRefs)
	r.Post("/repo/{repoName}.git/git-{command}", handler.Command)

	return r
}

package main

import (
	"net/http"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
)

func main() {

	InitSettings()
	settings := GetSettings()

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

	// r.Get("/", func(w http.ResponseWriter, r *http.Request) {
	// 	w.Write([]byte("hi"))
	// })

	repoApi := NewRepoApi()
	api := Api{
		RepoApi: repoApi,
	}

	r.Mount("/", View{}.GetRouters())
	r.Mount("/api", api.GetRouters())
	r.Mount("/repo", repoApi.GetRouters())

	http.ListenAndServe(settings.getListenAddr(), r)
}

package main

import (
	"fmt"
	"net/http"
)

func Auth(realm string, credentials map[string]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			username, password, ok := r.BasicAuth()
			if !ok {
				unauthorized(w, realm)
				return
			}

			validPassword, userFound := credentials[username]
			if !userFound {
				unauthorized(w, realm)
				return
			}

			if validPassword == password {
				next.ServeHTTP(w, r)
				return
			}

			unauthorized(w, realm)
			return
		})
	}
}

func unauthorized(w http.ResponseWriter, realm string) {
	w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Basic realm="%s"`, realm))
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte("Unauthorized"))
}

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi"

	"pgit/internal/pgs"
	"pgit/internal/pgs/git"
)

type HTTPHandler struct {
	Manager   *pgs.RepositoriesManager
	Settings  *pgs.Setting
	server    *http.Server
	router    http.Handler
}

func NewHTTPHandler(manager *pgs.RepositoriesManager, settings *pgs.Setting) *HTTPHandler {
	h := &HTTPHandler{Manager: manager, Settings: settings}
	h.router = h.buildRouter()
	return h
}

func (h *HTTPHandler) buildRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger)

	if h.Settings.HttpAuth {
		r.Use(basicAuth("pgit", h.Settings.Credentials))
	}

	prefix := "/" + h.Settings.WebUIPrefix

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/", h.serveAPIDocs)
		r.Get("/repos", h.listRepos)
		r.Post("/repos/{name}", h.createRepo)
		r.Get("/repos/{name}", h.getRepo)
		r.Delete("/repos/{name}", h.deleteRepo)
		r.Post("/repos/{name}/aliases", h.addAlias)
		r.Delete("/repos/{name}/aliases/{alias}", h.removeAlias)
		r.Post("/repos/{name}/default-branch", h.setDefaultBranch)
		r.Get("/repos/{name}/tree/{ref}/*", h.tree)
		r.Get("/repos/{name}/blob/{ref}/*", h.blob)
		r.Get("/repos/{name}/archive/{ref}", h.archive)
		r.Get("/repos/{name}/commits/{ref}", h.commits)
	})

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, prefix+"/", http.StatusFound)
	})
	r.Get(prefix, h.serveWebUI)
	r.Get(prefix+"/*", h.serveWebUI)

	r.NotFound(h.gitTransport)
	return r
}

func (h *HTTPHandler) HandleConn(conn net.Conn) {
	h.server = &http.Server{Handler: h.router}
	h.server.Serve(&singleConnListener{conn: conn})
}

type singleConnListener struct {
	conn    net.Conn
	served  bool
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.served {
		return nil, io.EOF
	}
	l.served = true
	return l.conn, nil
}
func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "pgit-mux" }

// --- Management API handlers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *HTTPHandler) listRepos(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total":        len(h.Manager.List()),
		"repositories": h.Manager.List(),
	})
}

func (h *HTTPHandler) createRepo(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	description := r.FormValue("description")
	defaultBranch := r.FormValue("defaultBranch")
	if err := h.Manager.CreateRepository(name, description, defaultBranch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	repo, _ := h.Manager.GetRepository(name)
	writeJSON(w, http.StatusOK, repo)
}

func (h *HTTPHandler) getRepo(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	repo, err := h.Manager.GetRepository(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	refs, err := repo.ForEachRef()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defaultBranch, _ := repo.DefaultBranch()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"metadata":      repo,
		"refs":          refs,
		"defaultBranch": defaultBranch,
	})
}

func (h *HTTPHandler) deleteRepo(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	confirm := r.FormValue("confirm")
	if confirm != name {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("confirm mismatch, expected %s", name))
		return
	}
	if err := h.Manager.DeleteRepository(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *HTTPHandler) addAlias(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	alias := r.FormValue("alias")
	if err := h.Manager.AddAlias(name, alias); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	repo, _ := h.Manager.GetRepository(name)
	writeJSON(w, http.StatusOK, repo)
}

func (h *HTTPHandler) removeAlias(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	alias := chi.URLParam(r, "alias")
	if err := h.Manager.RemoveAlias(name, alias); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	repo, _ := h.Manager.GetRepository(name)
	writeJSON(w, http.StatusOK, repo)
}

func (h *HTTPHandler) setDefaultBranch(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	branch := r.FormValue("branch")
	if branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	repo, err := h.Manager.GetRepository(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := repo.SetDefaultBranch(branch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":            true,
		"defaultBranch": branch,
	})
}

func (h *HTTPHandler) tree(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ref := chi.URLParam(r, "ref")
	if unesc, err := url.QueryUnescape(ref); err == nil {
		ref = unesc
	}
	repo, err := h.Manager.GetRepository(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if ref == "" {
		if db, err := repo.DefaultBranch(); err == nil && db != "" {
			ref = db
		} else {
			ref = "master"
		}
	}
	subtree := chi.URLParam(r, "*")
	files, err := repo.Tree(ref, subtree)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (h *HTTPHandler) blob(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ref := chi.URLParam(r, "ref")
	if unesc, err := url.QueryUnescape(ref); err == nil {
		ref = unesc
	}
	repo, err := h.Manager.GetRepository(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if ref == "" {
		if db, err := repo.DefaultBranch(); err == nil && db != "" {
			ref = db
		} else {
			ref = "master"
		}
	}
	path := chi.URLParam(r, "*")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is empty")
		return
	}
	body, err := repo.Blob(ref, path)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, body)
}

func (h *HTTPHandler) archive(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ref := chi.URLParam(r, "ref")
	if unesc, err := url.QueryUnescape(ref); err == nil {
		ref = unesc
	}
	repo, err := h.Manager.GetRepository(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if ref == "" {
		if db, err := repo.DefaultBranch(); err == nil && db != "" {
			ref = db
		} else {
			ref = "master"
		}
	}
	body, err := repo.Archive(ref)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-%s.zip", name, ref))
	_, _ = io.Copy(w, body)
}

func (h *HTTPHandler) commits(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ref := chi.URLParam(r, "ref")
	if unesc, err := url.QueryUnescape(ref); err == nil {
		ref = unesc
	}
	repo, err := h.Manager.GetRepository(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if ref == "" {
		if db, err := repo.DefaultBranch(); err == nil && db != "" {
			ref = db
		} else {
			ref = "master"
		}
	}
	limit := 20
	if n := r.FormValue("limit"); n != "" {
		if v, err := strconv.Atoi(n); err == nil && v > 0 {
			limit = v
		}
	}
	commits, err := repo.Commits(ref, limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, commits)
}

// --- Git smart-http transport ---

var gitTransportRe = regexp.MustCompile(`^/(.+?)/git/(info/refs|git-.+)$`)

func (h *HTTPHandler) gitTransport(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// split alias and git subpath; alias is everything before ".git"
	idx := strings.Index(path, ".git/")
	if idx <= 0 {
		http.NotFound(w, r)
		return
	}
	alias := strings.TrimPrefix(path[:idx], "/")
	sub := path[idx+len(".git/"):]
	if alias == "" || sub == "" {
		http.NotFound(w, r)
		return
	}

	repo, err := h.Manager.GetByAlias(alias)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	repoPath := repo.Path()

	switch {
	case sub == "info/refs":
		h.infoRefs(w, r, repoPath)
	case strings.HasPrefix(sub, "git-"):
		h.gitCommand(w, r, repoPath, strings.TrimPrefix(sub, "git-"))
	default:
		http.NotFound(w, r)
	}
}

func (h *HTTPHandler) infoRefs(w http.ResponseWriter, r *http.Request, repoPath string) {
	service := r.FormValue("service")
	if service == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
	out, err := git.ServeInfoRefs(repoPath, service)
	if err != nil {
		log.Printf("info-refs %s: %v", service, err)
		http.Error(w, "internal server error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(out)
	log.Printf("info-refs %s ok", service)
}

func (h *HTTPHandler) gitCommand(w http.ResponseWriter, r *http.Request, repoPath string, command string) {
	w.Header().Set("Content-Type", fmt.Sprintf("application/x-git-%s-result", command))
	w.WriteHeader(http.StatusOK)
	switch command {
	case "upload-pack":
		if err := git.HandleUploadPack(repoPath, r.Body, w); err != nil {
			log.Printf("upload-pack: %v", err)
		} else {
			log.Printf("upload-pack ok")
		}
	case "receive-pack":
		if err := git.HandleReceivePack(repoPath, r.Body, w); err != nil {
			log.Printf("receive-pack: %v", err)
		} else {
			log.Printf("receive-pack ok")
		}
	default:
		http.Error(w, "unknown command", http.StatusBadRequest)
	}
}

// --- Basic Auth middleware ---

func basicAuth(realm string, credentials map[string]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok {
				unauthorized(w, realm)
				return
			}
			valid, found := credentials[user]
			if !found || valid != pass {
				unauthorized(w, realm)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func unauthorized(w http.ResponseWriter, realm string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm="%s"`, realm))
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("Unauthorized"))
}

type responseStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseStatusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &responseStatusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		user, _, hasAuth := r.BasicAuth()
		remote, _, _ := net.SplitHostPort(r.RemoteAddr)
		if remote == "" {
			remote = r.RemoteAddr
		}
		authInfo := "-"
		if hasAuth {
			authInfo = user
		}
		log.Printf("HTTP %s %s %d %s %s %s",
			r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond), authInfo, remote)
	})
}

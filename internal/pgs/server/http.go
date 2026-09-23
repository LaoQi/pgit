package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"pgit/internal/pgs"
	"pgit/internal/pgs/git"
)

type HTTPHandler struct {
	Manager  *pgs.RepositoriesManager
	Settings *pgs.Setting
	Sync     *pgs.SyncManager
	router   http.Handler

	// packSem 限制并发的 pack 传输（clone/push），避免并发大仓库请求耗尽内存/fd。
	packSem chan struct{}
}

func NewHTTPHandler(manager *pgs.RepositoriesManager, settings *pgs.Setting, syncMgr *pgs.SyncManager) *HTTPHandler {
	h := &HTTPHandler{
		Manager:  manager,
		Settings: settings,
		Sync:     syncMgr,
		packSem:  make(chan struct{}, settings.LimitConcurrentPacks()),
	}
	h.router = h.buildRouter()
	return h
}

// acquirePack 获取一个 pack 传输名额；ctx 结束时放弃等待。
func (h *HTTPHandler) acquirePack(ctx context.Context) bool {
	select {
	case h.packSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (h *HTTPHandler) releasePack() { <-h.packSem }

func (h *HTTPHandler) buildRouter() http.Handler {
	// 探针与指标：独立 mux，只挂请求日志与请求 ID，不挂 BasicAuth。
	probe := http.NewServeMux()
	probe.HandleFunc("GET /healthz", h.healthz)
	probe.HandleFunc("GET /metrics", h.metrics)

	// 管理 API：独立 mux，随后整体套一层鉴权与日志中间件。
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/{$}", h.serveAPIDocs)
	api.HandleFunc("GET /api/v1/repos", h.listRepos)
	api.HandleFunc("POST /api/v1/repos/{name}", h.createRepo)
	api.HandleFunc("GET /api/v1/repos/{name}", h.getRepo)
	api.HandleFunc("DELETE /api/v1/repos/{name}", h.deleteRepo)
	api.HandleFunc("POST /api/v1/repos/{name}/aliases", h.addAlias)
	api.HandleFunc("DELETE /api/v1/repos/{name}/aliases/{alias}", h.removeAlias)
	api.HandleFunc("POST /api/v1/repos/{name}/default-branch", h.setDefaultBranch)
	api.HandleFunc("POST /api/v1/repos/{name}/settings", h.updateSettings)
	api.HandleFunc("GET /api/v1/repos/{name}/tree/{ref}/{path...}", h.tree)
	api.HandleFunc("GET /api/v1/repos/{name}/blob/{ref}/{path...}", h.blob)
	api.HandleFunc("GET /api/v1/repos/{name}/archive/{ref}", h.archive)
	api.HandleFunc("GET /api/v1/repos/{name}/commits/{ref}", h.commits)
	api.HandleFunc("POST /api/v1/repos/{name}/sync", h.syncRepo)
	api.HandleFunc("GET /api/v1/repos/{name}/sync-log", h.syncLog)
	api.HandleFunc("GET /api/v1/repos/{name}/mirror-status", h.mirrorStatus)

	prefix := "/" + h.Settings.WebUIPrefix

	// 两组中间件链：
	//   probeChain —— 请求 ID + 请求日志（探针/抓取端无需凭据）
	//   mainChain  —— 请求 ID + 请求日志 +（可选）Basic Auth
	probeChain := func(next http.Handler) http.Handler {
		return requestIDMiddleware(requestLogger(next))
	}
	mainChain := func(next http.Handler) http.Handler {
		next = requestIDMiddleware(requestLogger(next))
		if h.Settings.HttpAuth {
			next = basicAuth("pgit", h.Settings)(next)
		}
		return next
	}

	// 主 mux。路由优先级由 ServeMux 的「更具体模式优先」规则决定：
	// 精确模式（探针/API/WebUI）压过 "/" 兜底（git 传输）。
	root := http.NewServeMux()
	root.Handle("/healthz", probeChain(probe))
	root.Handle("/metrics", probeChain(probe))
	root.Handle("/api/v1/", mainChain(api))
	// WebUI 前缀为空时 prefix 退化为 "/"，会与下面的 "GET /{$}" 冲突；
	// 此种情况跳过 WebUI 路由（配置校验保证运行期 prefix 非空）。
	if prefix != "/" {
		root.Handle("GET "+prefix, mainChain(http.HandlerFunc(h.serveWebUI)))
		root.Handle("GET "+prefix+"/", mainChain(http.HandlerFunc(h.serveWebUI)))
	}
	root.Handle("GET /{$}", mainChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, prefix+"/", http.StatusFound)
	})))
	root.Handle("/", mainChain(http.HandlerFunc(h.gitTransport)))

	return root
}

// pathParam 读取路径参数（stdlib ServeMux 的 PathValue 封装）。
func pathParam(r *http.Request, name string) string { return r.PathValue(name) }

// healthz 健康检查：进程存活 + 仓库根可读 + 同步管理器就绪。
func (h *HTTPHandler) healthz(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	checks := map[string]string{}

	// 仓库根目录可读
	root := ""
	if h.Manager != nil && h.Manager.Config != nil {
		root = h.Manager.Config.GitRoot
	}
	if root == "" {
		root = pgs.GitRoot
	}
	if _, err := os.Stat(root); err != nil {
		status = http.StatusServiceUnavailable
		checks["gitRoot"] = "error: " + err.Error()
	} else {
		checks["gitRoot"] = "ok"
	}

	if h.Sync != nil {
		checks["syncManager"] = "ok"
	} else {
		checks["syncManager"] = "absent" // 非致命：无镜像功能
	}

	repos := 0
	if h.Manager != nil {
		repos = len(h.Manager.List())
	}
	pgs.SetReposTotal(repos)
	checks["repositories"] = fmt.Sprintf("%d", repos)

	pgs.DefaultRegistry().Gauge("pgit_pack_inflight", "当前进行中的 pack 传输数。").Set(float64(pgs.InflightPacks()))

	body := map[string]any{"status": "ok", "checks": checks}
	if status != http.StatusOK {
		body["status"] = "degraded"
	}
	writeJSON(w, status, body)
}

// metrics 输出 Prometheus 文本格式指标。
func (h *HTTPHandler) metrics(w http.ResponseWriter, r *http.Request) {
	pgs.DefaultRegistry().Gauge("pgit_pack_inflight", "当前进行中的 pack 传输数。").Set(float64(pgs.InflightPacks()))
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pgs.DefaultRegistry().Render())
}

// Router 返回 HTTP 路由处理器；连接由 MuxServer 统一交付给共享的 http.Server
// （见 mux.go），handler 本身不再自行创建 server。
func (h *HTTPHandler) Router() http.Handler { return h.router }

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
	repos := h.Manager.List() // 取一次：此前调用两次会各自构造切片
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total":        len(repos),
		"repositories": repos,
	})
}

func (h *HTTPHandler) createRepo(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	description := r.FormValue("description")
	defaultBranch := r.FormValue("defaultBranch")

	mirrorURL := r.FormValue("mirrorUrl")
	if mirrorURL != "" {
		syncInterval := 0
		if s := r.FormValue("mirrorInterval"); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v >= 0 {
				syncInterval = v
			}
		}
		authType := r.FormValue("mirrorAuthType")
		if authType == "" {
			authType = "none"
		}
		mirror := &pgs.MirrorConfig{
			RemoteURL:    mirrorURL,
			SyncInterval: syncInterval,
			AuthType:     authType,
			Username:     r.FormValue("mirrorUsername"),
			Password:     r.FormValue("mirrorPassword"),
			Proxy:        r.FormValue("mirrorProxy"),
		}
		if err := h.Manager.CreateMirrorRepository(name, description, mirror); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		repo, _ := h.Manager.GetRepository(name)
		if h.Sync != nil {
			h.Sync.Register(repo)
		}
		writeJSON(w, http.StatusOK, repo)
		return
	}

	if err := h.Manager.CreateRepository(name, description, defaultBranch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	repo, _ := h.Manager.GetRepository(name)
	writeJSON(w, http.StatusOK, repo)
}

func (h *HTTPHandler) getRepo(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
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
	name := pathParam(r, "name")
	confirm := r.FormValue("confirm")
	if confirm != name {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("confirm mismatch, expected %s", name))
		return
	}
	if h.Sync != nil {
		h.Sync.Unregister(name)
	}
	if err := h.Manager.DeleteRepository(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *HTTPHandler) addAlias(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	alias := r.FormValue("alias")
	if err := h.Manager.AddAlias(name, alias); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	repo, _ := h.Manager.GetRepository(name)
	writeJSON(w, http.StatusOK, repo)
}

func (h *HTTPHandler) removeAlias(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	alias := pathParam(r, "alias")
	if err := h.Manager.RemoveAlias(name, alias); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	repo, _ := h.Manager.GetRepository(name)
	writeJSON(w, http.StatusOK, repo)
}

func (h *HTTPHandler) setDefaultBranch(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
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
	name := pathParam(r, "name")
	ref := pathParam(r, "ref")
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
	subtree := pathParam(r, "path")
	files, err := repo.Tree(ref, subtree)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (h *HTTPHandler) blob(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	ref := pathParam(r, "ref")
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
	path := pathParam(r, "path")
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
	name := pathParam(r, "name")
	ref := pathParam(r, "ref")
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
	name := pathParam(r, "name")
	ref := pathParam(r, "ref")
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

func (h *HTTPHandler) syncRepo(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	if h.Sync == nil {
		writeError(w, http.StatusInternalServerError, "sync manager not initialized")
		return
	}
	entry, err := h.Sync.SyncNow(name)
	if err != nil {
		switch {
		case errors.Is(err, pgs.ErrRepoNotFound), errors.Is(err, pgs.ErrAliasNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, pgs.ErrNotMirror):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, pgs.ErrSyncInProgress):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":   true,
		"sync": entry,
	})
}

func (h *HTTPHandler) syncLog(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	repo, err := h.Manager.GetRepository(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if !repo.IsMirror() {
		writeError(w, http.StatusBadRequest, "not a mirror repository")
		return
	}
	limit := 50
	if s := r.FormValue("limit"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			limit = v
		}
	}
	entries, err := pgs.ReadSyncLog(repo.Path(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries,
	})
}

// mirrorStatus 返回镜像仓库的调度/同步状态（含上次错误与下次触发时间）。
func (h *HTTPHandler) mirrorStatus(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	if h.Sync == nil {
		writeError(w, http.StatusInternalServerError, "sync manager not initialized")
		return
	}
	st, err := h.Sync.Status(name)
	if err != nil {
		if errors.Is(err, pgs.ErrRepoNotFound) || errors.Is(err, pgs.ErrAliasNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *HTTPHandler) updateSettings(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	repo, err := h.Manager.GetRepository(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	description := r.FormValue("description")

	var mirrorUpdates *pgs.MirrorConfig
	if repo.IsMirror() {
		interval := 0
		if s := r.FormValue("mirrorInterval"); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v >= 0 {
				interval = v
			} else {
				writeError(w, http.StatusBadRequest, "invalid mirrorInterval")
				return
			}
		}
		authType := r.FormValue("mirrorAuthType")
		if authType == "" {
			authType = "none"
		}
		mirrorUpdates = &pgs.MirrorConfig{
			RemoteURL:    r.FormValue("mirrorRemoteUrl"),
			SyncInterval: interval,
			AuthType:     authType,
			Username:     r.FormValue("mirrorUsername"),
			Password:     r.FormValue("mirrorPassword"),
			Proxy:        r.FormValue("mirrorProxy"),
		}
	}

	oldInterval, err := h.Manager.UpdateRepositorySettings(name, description, mirrorUpdates)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	updated, _ := h.Manager.GetRepository(name)

	// syncInterval 变化时重新注册定时调度（0<->N、N->M 均重建）
	if h.Sync != nil && updated != nil && updated.IsMirror() {
		if oldInterval != updated.Mirror.SyncInterval {
			h.Sync.Unregister(name)
			h.Sync.Register(updated)
		}
	}

	writeJSON(w, http.StatusOK, updated)
}

// --- Git smart-http transport ---

var gitTransportRe = regexp.MustCompile(`^/(.+?)/git/(info/refs|git-.+)$`)

func (h *HTTPHandler) gitTransport(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// split alias and git subpath; alias is everything before ".git"
	// 用最后出现的 ".git/" 切分：alias 自身可能包含 ".git/"
	idx := strings.LastIndex(path, ".git/")
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

	// mirror 仓库禁止 push：拒绝 receive-pack 的广告(info/refs)与实际推送(git-receive-pack)。
	if repo.IsMirror() {
		service := ""
		if sub == "info/refs" {
			service = r.FormValue("service")
		} else if strings.HasPrefix(sub, "git-") {
			service = "git-" + strings.TrimPrefix(sub, "git-")
		}
		if service == "git-receive-pack" {
			slog.Warn("receive-pack denied for mirror repository", "requestId", requestID(r.Context()), "repo", repo.Name, "alias", alias)
			http.Error(w, "mirror repository: push disabled", http.StatusForbidden)
			return
		}
	}

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
		slog.Error("info-refs failed", "requestId", requestID(r.Context()), "service", service, "error", err)
		http.Error(w, "internal server error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(out)
	slog.Info("info-refs ok", "requestId", requestID(r.Context()), "service", service)
}

func (h *HTTPHandler) gitCommand(w http.ResponseWriter, r *http.Request, repoPath string, command string) {
	// receive-pack 请求体上限（含 packfile）；upload-pack 请求体小，无需限制。
	if command == "receive-pack" && h.Settings.LimitPushBytes() > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, h.Settings.LimitPushBytes())
	}

	// 并发 pack 传输限流：超限时排队，客户端断开即放弃。
	if !h.acquirePack(r.Context()) {
		http.Error(w, "server busy", http.StatusServiceUnavailable)
		return
	}
	defer h.releasePack()
	pgs.PackStarted()
	defer pgs.PackFinished()

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-git-%s-result", command))
	w.WriteHeader(http.StatusOK)

	body := r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			slog.Error("gzip decode failed", "requestId", requestID(r.Context()), "command", command, "error", err)
			return
		}
		defer gz.Close()
		body = gz
	}

	switch command {
	case "upload-pack":
		if err := git.HandleUploadPack(repoPath, body, w); err != nil {
			pgs.ObserveGitOperation("upload-pack", "failure")
			slog.Error("upload-pack failed", "requestId", requestID(r.Context()), "error", err)
		} else {
			pgs.ObserveGitOperation("upload-pack", "success")
		}
	case "receive-pack":
		if err := git.HandleReceivePack(repoPath, body, w); err != nil {
			pgs.ObserveGitOperation("receive-pack", "failure")
			slog.Error("receive-pack failed", "requestId", requestID(r.Context()), "error", err)
		} else {
			pgs.ObserveGitOperation("receive-pack", "success")
		}
	default:
		http.Error(w, "unknown command", http.StatusBadRequest)
	}
}

// --- Basic Auth middleware ---

// basicAuth 校验 HTTP Basic 凭据。凭据表每请求从 Setting 取副本，
// 因此 SIGHUP 热加载更新凭据后立即生效，且与并发读无竞态。
func basicAuth(realm string, settings *pgs.Setting) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok {
				unauthorized(w, realm)
				return
			}
			valid, found := settings.CredentialsCopy()[user]
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

// responseStatusWriter 记录响应状态码，并透传底层 ResponseWriter 的可选能力。
// 若不实现这些接口，下游 handler 对 Flush/大文件转发/长连接的处理会退化
// （例如 http.ResponseWriter 的 io.ReaderFrom 快速路径失效、SSE 无法即时刷新）。
type responseStatusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *responseStatusWriter) WriteHeader(code int) {
	if w.wrote {
		return // 与 net/http 语义一致：重复 WriteHeader 无效
	}
	w.wrote = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseStatusWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// Flush 透传（SSE/流式响应需要）。
func (w *responseStatusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 透传（协议升级，如 websocket）。
func (w *responseStatusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// Push 透传（HTTP/2 server push）。
func (w *responseStatusWriter) Push(target string, opts *http.PushOptions) error {
	p, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

// ReadFrom 走底层快速路径（io.Copy 大响应时避免额外拷贝）。
func (w *responseStatusWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

// Unwrap 供 http.ResponseController 访问底层 writer。
func (w *responseStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// requestIDHeader 是请求 ID 的入口/出口头名（透传客户端提供的值，否则服务端生成）。
const requestIDHeader = "X-Request-Id"

type ctxKey int

const requestIDKey ctxKey = iota

// requestID 从上下文取请求 ID（中间件注入）。
func requestID(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// newRequestID 生成随机请求 ID（16 字节 hex）。
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// requestIDMiddleware 为每个请求分配/透传请求 ID，写入上下文与响应头，
// 供请求日志与下游 handler 关联同一请求的多条日志。
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(requestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
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
		pgs.ObserveHTTPRequest(r.Method, sw.status, time.Since(start))

		attrs := []any{
			"requestId", requestID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"durationMs", time.Since(start).Milliseconds(),
			"remote", remote,
		}
		if hasAuth {
			attrs = append(attrs, "user", user)
		}
		switch {
		case sw.status >= 500:
			slog.Error("http request", attrs...)
		case sw.status >= 400:
			slog.Warn("http request", attrs...)
		default:
			slog.Info("http request", attrs...)
		}
	})
}

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
	"sort"
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
	// Relay 管理中转仓库的 push 准入与转发上游（可为 nil，表示未启用中转能力）。
	Relay  *pgs.RelayManager
	router http.Handler

	// packSem 限制并发的 pack 传输（clone/push），避免并发大仓库请求耗尽内存/fd。
	packSem chan struct{}
}

func NewHTTPHandler(manager *pgs.RepositoriesManager, settings *pgs.Setting, syncMgr *pgs.SyncManager, relayMgr *pgs.RelayManager) *HTTPHandler {
	h := &HTTPHandler{
		Manager:  manager,
		Settings: settings,
		Sync:     syncMgr,
		Relay:    relayMgr,
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
	// 仓库引用一律走 ref 参数（不占路径段），路径只表达动作：
	// 这样多段别名（owner/repo）与含斜杠的别名在全部接口上都不需要百分号编码。
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/{$}", h.serveAPIDocs)
	api.HandleFunc("GET /api/v1/repos", h.listRepos)
	api.HandleFunc("POST /api/v1/repos", h.createRepo)
	api.HandleFunc("GET /api/v1/repos/info", h.getRepo)
	api.HandleFunc("DELETE /api/v1/repos/info", h.deleteRepo)
	api.HandleFunc("POST /api/v1/repos/aliases", h.addAlias)
	api.HandleFunc("DELETE /api/v1/repos/aliases", h.removeAlias)
	api.HandleFunc("POST /api/v1/repos/default-branch", h.setDefaultBranch)
	api.HandleFunc("POST /api/v1/repos/settings", h.updateSettings)
	// tree/blob 的文件路径是尾随通配（根目录时为空），两种形态都注册以避免尾斜杠陷阱。
	api.HandleFunc("GET /api/v1/repos/tree", h.tree)
	api.HandleFunc("GET /api/v1/repos/tree/{path...}", h.tree)
	api.HandleFunc("GET /api/v1/repos/blob", h.blob)
	api.HandleFunc("GET /api/v1/repos/blob/{path...}", h.blob)
	api.HandleFunc("GET /api/v1/repos/archive", h.archive)
	api.HandleFunc("GET /api/v1/repos/commits", h.commits)
	api.HandleFunc("POST /api/v1/repos/sync", h.syncRepo)
	api.HandleFunc("GET /api/v1/repos/sync-log", h.syncLog)
	api.HandleFunc("GET /api/v1/repos/mirror-status", h.mirrorStatus)
	api.HandleFunc("POST /api/v1/repos/relay/push", h.relayPush)
	api.HandleFunc("POST /api/v1/repos/relay/align", h.relayAlign)
	api.HandleFunc("GET /api/v1/repos/relay-status", h.relayStatus)
	api.HandleFunc("GET /api/v1/repos/relay-log", h.relayLog)
	api.HandleFunc("GET /api/v1/github/repos", h.githubRepos)
	api.HandleFunc("POST /api/v1/github/import", h.githubImport)

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

	if h.Sync != nil {
		st := h.Sync.QueueStats()
		checks["syncQueue"] = fmt.Sprintf("workers=%d queued=%d running=%d dropped=%d", st.Workers, st.Queued, st.Running, st.Dropped)
	}
	if h.Relay != nil {
		st := h.Relay.QueueStats()
		checks["relayQueue"] = fmt.Sprintf("workers=%d queued=%d running=%d dropped=%d", st.Workers, st.Queued, st.Running, st.Dropped)
	}

	body := map[string]any{"status": "ok", "checks": checks}
	if status != http.StatusOK {
		body["status"] = "degraded"
	}
	writeJSON(w, status, body)
}

// metrics 输出 Prometheus 文本格式指标。
func (h *HTTPHandler) metrics(w http.ResponseWriter, r *http.Request) {
	pgs.DefaultRegistry().Gauge("pgit_pack_inflight", "当前进行中的 pack 传输数。").Set(float64(pgs.InflightPacks()))
	if h.Sync != nil {
		h.Sync.QueueStats() // 采集同步任务队列的瞬时指标
	}
	if h.Relay != nil {
		h.Relay.QueueStats() // 采集中转转发任务队列的瞬时指标
	}
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

// repoSummary 是列表接口的仓库视图：平铺 Repository 元数据并附加最后提交时间。
type repoSummary struct {
	*pgs.Repository
	// LastCommitTime 为 nil 表示仓库尚无提交（空仓库）。
	LastCommitTime *time.Time `json:"lastCommitTime,omitempty"`
}

func (h *HTTPHandler) listRepos(w http.ResponseWriter, r *http.Request) {
	repos := h.Manager.List() // 取一次：此前调用两次会各自构造切片
	summaries := make([]repoSummary, len(repos))
	for i, repo := range repos {
		s := repoSummary{Repository: repo}
		if ts := repo.LastCommitTime(); !ts.IsZero() {
			s.LastCommitTime = &ts
		}
		summaries[i] = s
	}
	// 最后提交降序（新→旧）；无提交的空仓库排最后；同刻按 name 升序稳定。
	sort.Slice(summaries, func(i, j int) bool {
		a, b := summaries[i].LastCommitTime, summaries[j].LastCommitTime
		switch {
		case a != nil && b != nil:
			if !a.Equal(*b) {
				return a.After(*b)
			}
		case a != nil:
			return true
		case b != nil:
			return false
		}
		return summaries[i].Name < summaries[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total":        len(summaries),
		"repositories": summaries,
	})
}

func (h *HTTPHandler) createRepo(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	description := r.FormValue("description")
	defaultBranch := r.FormValue("defaultBranch")

	mirrorURL := r.FormValue("mirrorUrl")
	if mirrorURL != "" {
		syncInterval := 0
		if v := r.FormValue("mirrorInterval"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				syncInterval = n
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
			Mode:         r.FormValue("mirrorMode"),
		}
		if mirror.Mode == pgs.MirrorModeRelay {
			refs, err := parseRefPrefixes(r.FormValue("relayRefs"))
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			mirror.Refs = refs
			mirror.AllowDelete = formBoolPtr(r, "relayAllowDelete")
		}
		if err := h.Manager.CreateMirrorRepository(name, description, mirror); err != nil {
			writeError(w, refErrorStatus(err), err.Error())
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
		writeError(w, refErrorStatus(err), err.Error())
		return
	}
	repo, _ := h.Manager.GetRepository(name)
	writeJSON(w, http.StatusOK, repo)
}

func (h *HTTPHandler) getRepo(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
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
	repo, ref, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	// 确认值必须等于 canonical name：即使按别名寻址，也要写出真实名字才允许删除。
	confirm := r.FormValue("confirm")
	if confirm != repo.Name {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("confirm mismatch, expected %s", repo.Name))
		return
	}
	if h.Sync != nil {
		h.Sync.Unregister(repo.Name)
	}
	if h.Relay != nil {
		h.Relay.Unregister(repo.Name)
	}
	if err := h.Manager.DeleteRepository(repo.Name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	slog.Info("deleted repository", "repo", repo.Name, "ref", ref)
	w.WriteHeader(http.StatusOK)
}

// refParam 读取仓库引用参数（仓库名或别名）。
func refParam(r *http.Request) string { return strings.TrimSpace(r.FormValue("ref")) }

// resolveRepo 把 ref 参数解析为仓库元数据快照；失败时已写完响应，调用方直接 return。
//
// 后续步骤一律使用 canonical name 调用 Manager（Sync/设置等接口按 name 工作），
// 调试日志同时记录 ref 与解析结果，便于审计「用哪个别名访问的」。
func (h *HTTPHandler) resolveRepo(w http.ResponseWriter, r *http.Request) (*pgs.Repository, string, bool) {
	ref := refParam(r)
	if ref == "" {
		writeError(w, http.StatusBadRequest, "ref is required")
		return nil, "", false
	}
	repo, err := h.Manager.Resolve(ref)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return nil, "", false
	}
	slog.Debug("api ref resolved", "repo", repo.Name, "ref", ref, "path", r.URL.Path)
	return repo, ref, true
}

// treeishParam 读取 treeish 参数；缺省时回落到仓库默认分支。
func treeishParam(r *http.Request, repo *pgs.Repository) string {
	if v := strings.TrimSpace(r.FormValue("treeish")); v != "" {
		return v
	}
	if db, err := repo.DefaultBranch(); err == nil && db != "" {
		return db
	}
	return "master"
}

// refErrorStatus 把 ref 唯一性冲突映射为 409，其余校验错误映射为 400。
func refErrorStatus(err error) int {
	if errors.Is(err, pgs.ErrRefConflict) || errors.Is(err, pgs.ErrRepoExist) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func (h *HTTPHandler) addAlias(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	if err := h.Manager.AddAlias(repo.Name, r.FormValue("alias")); err != nil {
		writeError(w, refErrorStatus(err), err.Error())
		return
	}
	updated, _ := h.Manager.GetRepository(repo.Name)
	writeJSON(w, http.StatusOK, updated)
}

func (h *HTTPHandler) removeAlias(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	if err := h.Manager.RemoveAlias(repo.Name, r.FormValue("alias")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, _ := h.Manager.GetRepository(repo.Name)
	writeJSON(w, http.StatusOK, updated)
}

func (h *HTTPHandler) setDefaultBranch(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	branch := r.FormValue("branch")
	if branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
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
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	files, err := repo.Tree(treeishParam(r, repo), pathParam(r, "path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (h *HTTPHandler) blob(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	path := pathParam(r, "path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is empty")
		return
	}
	body, err := repo.Blob(treeishParam(r, repo), path)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, body)
}

func (h *HTTPHandler) archive(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	treeish := treeishParam(r, repo)
	body, err := repo.Archive(treeish)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	// treeish 可能含 '/'（如 feature/x），写进文件名前替换掉。
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%s-%s.zip", repo.Name, strings.ReplaceAll(treeish, "/", "-")))
	_, _ = io.Copy(w, body)
}

func (h *HTTPHandler) commits(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	limit := 20
	if n := r.FormValue("limit"); n != "" {
		if v, err := strconv.Atoi(n); err == nil && v > 0 {
			limit = v
		}
	}
	commits, err := repo.Commits(treeishParam(r, repo), limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, commits)
}

func (h *HTTPHandler) syncRepo(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	if h.Sync == nil {
		writeError(w, http.StatusInternalServerError, "sync manager not initialized")
		return
	}
	entry, err := h.Sync.SyncNow(repo.Name)
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
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	if !repo.IsMirror() {
		writeError(w, http.StatusBadRequest, "not a mirror repository")
		return
	}
	limit := 50
	if v := r.FormValue("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
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
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	if h.Sync == nil {
		writeError(w, http.StatusInternalServerError, "sync manager not initialized")
		return
	}
	st, err := h.Sync.Status(repo.Name)
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

// --- 中转仓库（relay）---

func (h *HTTPHandler) relayManager(w http.ResponseWriter) (*pgs.RelayManager, bool) {
	if h.Relay == nil {
		writeError(w, http.StatusInternalServerError, "relay manager not initialized")
		return nil, false
	}
	return h.Relay, true
}

// relayPush 手动触发一次转发（把本地领先于上游基线的 ref 推给上游）。
func (h *HTTPHandler) relayPush(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	rm, ok := h.relayManager(w)
	if !ok {
		return
	}
	entry, err := rm.RelayNow(repo.Name)
	if err != nil {
		switch {
		case errors.Is(err, pgs.ErrNotRelay):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, pgs.ErrSyncInProgress):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":    entry != nil && entry.Success,
		"relay": entry,
	})
}

// relayStatus 返回中转仓库的基线/待转发/最近转发状态。
func (h *HTTPHandler) relayStatus(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	rm, ok := h.relayManager(w)
	if !ok {
		return
	}
	st, err := rm.Status(repo.Name)
	if err != nil {
		if errors.Is(err, pgs.ErrNotRelay) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// relayLog 返回最近的转发日志（最新在前）。
func (h *HTTPHandler) relayLog(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	if !repo.IsRelay() {
		writeError(w, http.StatusBadRequest, "not a relay repository")
		return
	}
	limit := 50
	if v := r.FormValue("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	entries, err := pgs.ReadRelayLog(repo.Path(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"entries": entries})
}

// relayAlign 危险操作：把本地 refs 强制对齐上游基线并丢弃待转发集合（需 confirm=仓库名）。
func (h *HTTPHandler) relayAlign(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	if !repo.IsRelay() {
		writeError(w, http.StatusBadRequest, "not a relay repository")
		return
	}
	if strings.TrimSpace(r.FormValue("confirm")) != repo.Name {
		writeError(w, http.StatusBadRequest, "confirm must equal the repository name")
		return
	}
	rm, ok := h.relayManager(w)
	if !ok {
		return
	}
	result, err := rm.AlignToUpstream(repo.Name)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// --- GitHub 导入（一次性发现 + 勾选生成镜像仓库）---

// githubToken 从表单或 X-Github-Token 头取 Token（头传递可避免 Token 出现在 URL）。
func githubToken(r *http.Request) string {
	if v := r.FormValue("token"); v != "" {
		return v
	}
	return r.Header.Get("X-Github-Token")
}

// formBool 解析表单布尔值；未提供（或无法识别）时返回默认值。
func formBool(r *http.Request, name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(r.FormValue(name))) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// formBoolPtr 解析可选布尔字段：字段缺失时返回 nil（表示「未提供，沿用默认/原值」）。
func formBoolPtr(r *http.Request, name string) *bool {
	if strings.TrimSpace(r.FormValue(name)) == "" {
		return nil
	}
	v := formBool(r, name, true)
	return &v
}

// parseRefPrefixes 解析 relay 的 ref 前缀白名单：接受逗号/空白/换行分隔；空表示使用默认值。
func parseRefPrefixes(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.HasPrefix(f, "refs/") {
			return nil, fmt.Errorf("relayRefs entry %q must start with \"refs/\"", f)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// writeGithubError 把发现/导入错误映射为 HTTP 状态码：
// 未找到 → 404，无效 Token → 401，限流 → 429，上游/网络错误 → 502，其余（参数校验）→ 400。
func writeGithubError(w http.ResponseWriter, err error) {
	var apiErr *pgs.GithubAPIError
	switch {
	case errors.Is(err, pgs.ErrGithubNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, pgs.ErrGithubRateLimited):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized:
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.As(err, &apiErr), errors.Is(err, pgs.ErrGithubUpstream):
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// githubRepos 发现账号仓库（只读，无副作用），并标注本地命名与冲突。
func (h *HTTPHandler) githubRepos(w http.ResponseWriter, r *http.Request) {
	owner := strings.TrimSpace(r.FormValue("owner"))
	if owner == "" {
		writeError(w, http.StatusBadRequest, "owner is required")
		return
	}
	namePrefix := r.FormValue("namePrefix")
	repos, err := pgs.DiscoverGithubRepos(pgs.GithubDiscoverQuery{
		Owner:           owner,
		Token:           githubToken(r),
		APIBase:         r.FormValue("apiBase"),
		Proxy:           r.FormValue("proxy"),
		IncludeForks:    formBool(r, "includeForks", false),
		IncludeArchived: formBool(r, "includeArchived", true),
		NamePrefix:      namePrefix,
	})
	if err != nil {
		writeGithubError(w, err)
		return
	}
	h.Manager.AnnotateGithubRepos(repos, namePrefix)
	creatable := 0
	for _, repo := range repos {
		if repo.Conflict == "" {
			creatable++
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"owner":        owner,
		"total":        len(repos),
		"creatable":    creatable,
		"repositories": repos,
	})
}

// githubImport 为勾选的仓库创建镜像仓库（逐个独立处理）。
func (h *HTTPHandler) githubImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form: "+err.Error())
		return
	}
	owner := strings.TrimSpace(r.FormValue("owner"))
	if owner == "" {
		writeError(w, http.StatusBadRequest, "owner is required")
		return
	}
	interval := 0
	if v := strings.TrimSpace(r.FormValue("syncInterval")); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid syncInterval")
			return
		}
		interval = parsed
	}
	repos := r.Form["repos"]
	if len(repos) == 0 {
		// 也接受逗号分隔的单字段形式
		if v := strings.TrimSpace(r.FormValue("repos")); v != "" {
			repos = strings.Split(v, ",")
		}
	}

	results, err := pgs.ImportGithubMirrors(h.Manager, h.Sync, pgs.GithubImportRequest{
		Owner:        owner,
		Token:        githubToken(r),
		APIBase:      r.FormValue("apiBase"),
		CloneBase:    r.FormValue("cloneBase"),
		Proxy:        r.FormValue("proxy"),
		NamePrefix:   r.FormValue("namePrefix"),
		SyncInterval: interval,
		Repos:        repos,
	})
	if err != nil {
		writeGithubError(w, err)
		return
	}
	created, failed := pgs.SummarizeGithubImport(results)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      failed == 0,
		"created": created,
		"failed":  failed,
		"results": results,
	})
}

func (h *HTTPHandler) updateSettings(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := h.resolveRepo(w, r)
	if !ok {
		return
	}
	description := r.FormValue("description")

	var mirrorUpdates *pgs.MirrorConfig
	if repo.IsMirror() {
		interval := 0
		if v := r.FormValue("mirrorInterval"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				interval = n
			} else {
				writeError(w, http.StatusBadRequest, "invalid mirrorInterval")
				return
			}
		}
		authType := r.FormValue("mirrorAuthType")
		if authType == "" {
			authType = "none"
		}
		mode := r.FormValue("mirrorMode")
		if mode == "" {
			mode = repo.Mirror.Mode // 未显式指定时保持原模式（避免改描述就把 relay 降级为镜像）
		}
		mirrorUpdates = &pgs.MirrorConfig{
			RemoteURL:    r.FormValue("mirrorRemoteUrl"),
			SyncInterval: interval,
			AuthType:     authType,
			Username:     r.FormValue("mirrorUsername"),
			Password:     r.FormValue("mirrorPassword"),
			Proxy:        r.FormValue("mirrorProxy"),
			Mode:         mode,
		}
		if mode == pgs.MirrorModeRelay {
			// relay 字段「未提供=保留原值」：与 mirrorPassword 语义一致，
			// 只改描述等无关设置时不得把白名单/删除开关静默重置为默认。
			if strings.TrimSpace(r.FormValue("relayRefs")) == "" {
				mirrorUpdates.Refs = repo.Mirror.Refs
			} else {
				refs, err := parseRefPrefixes(r.FormValue("relayRefs"))
				if err != nil {
					writeError(w, http.StatusBadRequest, err.Error())
					return
				}
				mirrorUpdates.Refs = refs
			}
			if strings.TrimSpace(r.FormValue("relayAllowDelete")) == "" {
				mirrorUpdates.AllowDelete = repo.Mirror.AllowDelete
			} else {
				mirrorUpdates.AllowDelete = formBoolPtr(r, "relayAllowDelete")
			}
		}
	}

	oldInterval, err := h.Manager.UpdateRepositorySettings(repo.Name, description, mirrorUpdates)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	updated, _ := h.Manager.GetRepository(repo.Name)

	// syncInterval 变化时重新注册定时调度（0<->N、N->M 均重建）
	if h.Sync != nil && updated != nil && updated.IsMirror() {
		if oldInterval != updated.Mirror.SyncInterval {
			h.Sync.Unregister(repo.Name)
			h.Sync.Register(updated)
		}
	}
	// 中转运行态跟随模式：切走 relay 时注销（清掉重试定时器与基线），切到 relay 时重置状态
	if h.Relay != nil && updated != nil {
		if !updated.IsRelay() {
			h.Relay.Unregister(repo.Name)
		}
	}

	writeJSON(w, http.StatusOK, updated)
}

// --- Git smart-http transport ---

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

	// 镜像仓库禁止 push：拒绝 receive-pack 的广告(info/refs)与实际推送(git-receive-pack)。
	// 中转仓库（relay）同样属于「带远端配置」的仓库，但它的核心能力就是接收下游推送，
	// 因此必须放行；真正的准入由 RelayManager 在 ref 更新前把关。
	if repo.IsMirror() && !repo.IsRelay() {
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
		h.gitCommand(w, r, repo, strings.TrimPrefix(sub, "git-"))
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

func (h *HTTPHandler) gitCommand(w http.ResponseWriter, r *http.Request, repo *pgs.Repository, command string) {
	repoPath := repo.Path()
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
			if errors.Is(err, git.ErrClientAborted) {
				// 客户端在交换前放弃（空 body / 首帧 flush）：正常收尾，不计失败
				pgs.ObserveGitOperation("upload-pack", "aborted")
				slog.Debug("upload-pack aborted by client", "requestId", requestID(r.Context()))
				return
			}
			pgs.ObserveGitOperation("upload-pack", "failure")
			slog.Error("upload-pack failed", "requestId", requestID(r.Context()), "error", err)
		} else {
			pgs.ObserveGitOperation("upload-pack", "success")
		}
	case "receive-pack":
		var opts []git.ReceivePackOptions
		if repo.IsRelay() && h.Relay != nil {
			opts = append(opts, git.ReceivePackOptions{
				PreRefs: func(updates []git.RefUpdate) []*git.RefUpdateResult {
					return h.Relay.Admit(repo, updates)
				},
			})
		}
		results, err := git.HandleReceivePack(repoPath, body, w, opts...)
		if err != nil {
			pgs.ObserveGitOperation("receive-pack", "failure")
			slog.Error("receive-pack failed", "requestId", requestID(r.Context()), "error", err)
		} else {
			pgs.ObserveGitOperation("receive-pack", "success")
			if repo.IsRelay() && h.Relay != nil {
				// 已通过准入并更新成功的 ref 记入待转发集合（非阻塞，异步转发上游）
				h.Relay.OnRefsUpdated(repo, results)
			}
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

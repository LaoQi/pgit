package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pgit/internal/pgs"
)

// newRouterHandler 构造带给定 WebUI 前缀的 handler（其余设置默认）。
func newRouterHandler(t *testing.T, prefix string, auth bool) *HTTPHandler {
	t.Helper()
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	settings := &pgs.Setting{WebUIPrefix: prefix}
	if auth {
		settings.HttpAuth = true
		settings.Credentials = map[string]string{"u": "p"}
	}
	return NewHTTPHandler(pgs.ReposManager, settings, nil, nil)
}

func do(h *HTTPHandler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// 每条管理 API 路由都应命中对应 handler，而不是落到 "/" 兜底（gitTransport → 404）。
func TestRouterAdminRoutesNotFallingThrough(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	cases := []struct {
		method, path string
		wantNot404   bool
	}{
		{http.MethodGet, "/api/v1/", true},
		{http.MethodGet, "/api/v1/repos", true},
		{http.MethodPost, "/api/v1/repos", true},
		{http.MethodGet, "/api/v1/repos/info?ref=r1", true},
		{http.MethodDelete, "/api/v1/repos/info?ref=r1", true},
		{http.MethodPost, "/api/v1/repos/aliases", true},
		{http.MethodDelete, "/api/v1/repos/aliases?ref=r1&alias=a%2Fb", true},
		{http.MethodPost, "/api/v1/repos/default-branch", true},
		{http.MethodPost, "/api/v1/repos/settings", true},
		{http.MethodGet, "/api/v1/repos/tree?ref=r1&treeish=master", true},
		{http.MethodGet, "/api/v1/repos/tree/src/pkg?ref=r1", true},
		{http.MethodGet, "/api/v1/repos/blob?ref=r1&path=a.txt", true},
		{http.MethodGet, "/api/v1/repos/blob/a.txt?ref=r1", true},
		{http.MethodGet, "/api/v1/repos/archive?ref=r1&treeish=master", true},
		{http.MethodGet, "/api/v1/repos/commits?ref=r1&treeish=master", true},
		{http.MethodPost, "/api/v1/repos/sync", true},
		{http.MethodGet, "/api/v1/repos/sync-log?ref=r1", true},
		{http.MethodGet, "/api/v1/repos/mirror-status?ref=r1", true},
		{http.MethodGet, "/api/v1/github/repos", true},
		{http.MethodPost, "/api/v1/github/import", true},
	}
	for _, c := range cases {
		rec := do(h, c.method, c.path)
		// gitTransport 兜底用 http.NotFound → 纯文本 "404 page not found"；
		// API handler 无论状态码都会输出 JSON。据此区分是否命中真实路由。
		if c.wantNot404 && strings.Contains(rec.Body.String(), "404 page not found") {
			t.Errorf("%s %s -> 落到 gitTransport 兜底，body=%s", c.method, c.path, rec.Body.String())
		}
	}
}

// 未注册的方法应返回 405（stdlib 语义），而非静默落到兜底。
func TestRouterMethodNotAllowed(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	// PUT 未注册；POST /api/v1/repos 现在是创建接口，不再是 405。
	rec := do(h, http.MethodPut, "/api/v1/repos")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/v1/repos status = %d, want 405", rec.Code)
	}
}

// git 传输走 "/" 兜底，alias 含 ".git/" 也能切分。
func TestRouterGitTransportFallback(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	rec := do(h, http.MethodGet, "/foo.git/info/refs")
	// repo 不存在 → gitTransport 内 404，但关键是不 panic 且确实进入兜底。
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown repo info/refs = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "404") {
		t.Errorf("body = %q, want 404 page from fallback", rec.Body.String())
	}
}

// 探针端点不受鉴权拦截；管理 API 与根重定向在开启鉴权时需凭据（与原 chi 分组一致）。
func TestRouterProbeBypassAndAuthScope(t *testing.T) {
	h := newRouterHandler(t, "custom/ui", true)

	for _, p := range []string{"/healthz", "/metrics"} {
		rec := do(h, http.MethodGet, p)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200 (probe must bypass auth)", p, rec.Code)
		}
	}

	// 对照：管理 API 需鉴权
	if rec := do(h, http.MethodGet, "/api/v1/repos"); rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/v1/repos = %d, want 401", rec.Code)
	}
	// 根重定向同样在鉴权范围内（与 chi 原 Group 行为一致）
	if rec := do(h, http.MethodGet, "/"); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET / (auth on) = %d, want 401", rec.Code)
	}
}

// 根路径在无鉴权时 302 到 WebUI 前缀。
func TestRouterRootRedirect(t *testing.T) {
	h := newRouterHandler(t, "custom/ui", false)
	rec := do(h, http.MethodGet, "/")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/custom/ui/" {
		t.Errorf("GET / -> %d loc=%q, want 302 /custom/ui/", rec.Code, rec.Header().Get("Location"))
	}
}

// 多段 WebUI 前缀与静态资源命中 serveWebUI。
func TestRouterMultiSegmentWebUIPrefix(t *testing.T) {
	h := newRouterHandler(t, "custom/ui", false)
	for _, p := range []string{"/custom/ui", "/custom/ui/", "/custom/ui/assets/app.js"} {
		rec := do(h, http.MethodGet, p)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, rec.Code)
		}
	}
}

// ref 缺失时应由 handler 明确报 400，而不是落到底部兜底。
func TestRouterMissingRefParam(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	rec := do(h, http.MethodGet, "/api/v1/repos/info")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("info without ref = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ref is required") {
		t.Errorf("body = %q, want ref is required", rec.Body.String())
	}
}

// tree/blob 的尾随通配：根目录（无 path）与多段 path 都应命中 handler。
func TestRouterTreePathWildcard(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/repos/tree?ref=r1", http.StatusNotFound}, // 仓库不存在
		{"/api/v1/repos/tree/src", http.StatusBadRequest},  // 缺 ref
		{"/api/v1/repos/tree/src/pkg/deep?ref=r1&treeish=master", http.StatusNotFound},
		{"/api/v1/repos/blob?ref=r1", http.StatusNotFound}, // 先解析 ref，仓库不存在
		{"/api/v1/repos/blob/a.txt?ref=r1", http.StatusNotFound},
	}
	for _, c := range cases {
		rec := do(h, http.MethodGet, c.path)
		if strings.Contains(rec.Body.String(), "404 page not found") {
			t.Errorf("GET %s 落到 gitTransport 兜底", c.path)
		}
		if rec.Code != c.want {
			t.Errorf("GET %s = %d, want %d", c.path, rec.Code, c.want)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("GET %s Content-Type = %q, want application/json", c.path, ct)
		}
	}
}

// 别名（含多段）可解析为仓库引用：ref=owner/repo 不带百分号编码也应命中 handler。
// 仓库存在时，blob 缺 path 应报 400（而不是 404），且按 name 访问 info 正常。
func TestRouterBlobWithoutPath(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	if err := pgs.ReposManager.CreateRepository("r1", "", "master"); err != nil {
		t.Fatal(err)
	}
	rec := do(h, http.MethodGet, "/api/v1/repos/blob?ref=r1")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("blob without path = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "path is empty") {
		t.Errorf("body = %q, want path is empty", rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/api/v1/repos/info?ref=r1")
	if rec.Code != http.StatusOK {
		t.Errorf("info by name = %d, want 200", rec.Code)
	}
}

func TestRouterMultiSegmentAliasRef(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	rec := do(h, http.MethodGet, "/api/v1/repos/info?ref=owner/repo")
	if strings.Contains(rec.Body.String(), "404 page not found") {
		t.Fatalf("多段 ref 落到兜底: %s", rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (repo not found)", rec.Code)
	}
}

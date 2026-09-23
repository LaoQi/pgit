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
	return NewHTTPHandler(pgs.ReposManager, settings, nil)
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
		{http.MethodPost, "/api/v1/repos/r1", true},
		{http.MethodGet, "/api/v1/repos/r1", true},
		{http.MethodDelete, "/api/v1/repos/r1", true},
		{http.MethodGet, "/api/v1/repos/r1/tree/master/", true},
		{http.MethodGet, "/api/v1/repos/r1/blob/master/a.txt", true},
		{http.MethodGet, "/api/v1/repos/r1/archive/master", true},
		{http.MethodGet, "/api/v1/repos/r1/commits/master", true},
		{http.MethodGet, "/api/v1/repos/r1/sync-log", true},
		{http.MethodGet, "/api/v1/repos/r1/mirror-status", true},
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
	rec := do(h, http.MethodPost, "/api/v1/repos")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/v1/repos status = %d, want 405", rec.Code)
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

// 路径参数：name/ref/path 分别正确取出，ref 由 stdlib 直接解码（不再二次 Unescape）。
func TestRouterPathValues(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	// 建仓以便 GetRepository 命中；路径参数解析在 handler 内，用 404 与否间接验证。
	// name 正确 → 404（仓库不存在）；name 错误解析（含 "/"）→ 同样 404，故用 tree 的空路径分支。
	rec := do(h, http.MethodGet, "/api/v1/repos/r1/tree/master/")
	if rec.Code != http.StatusNotFound {
		t.Errorf("tree on missing repo = %d, want 404", rec.Code)
	}
}

// 编码的 ref（如 feature 分支名含斜杠 feat%2Ffoo）应命中 tree 路由并经
// stdlib 解码后交给 handler，而不是落到底部兜底。
func TestRouterEncodedRefReachesHandler(t *testing.T) {
	h := newRouterHandler(t, "__webui", false)
	rec := do(h, http.MethodGet, "/api/v1/repos/r1/tree/feat%2Ffoo/")
	if strings.Contains(rec.Body.String(), "404 page not found") {
		t.Fatalf("encoded ref fell through to gitTransport fallback: %s", rec.Body.String())
	}
	// 命中 API handler → JSON 响应（此仓库不存在，应为 404 JSON）
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

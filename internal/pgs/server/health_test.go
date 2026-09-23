package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pgit/internal/pgs"
)

func newTestHandler(t *testing.T, withSync bool) *HTTPHandler {
	t.Helper()
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	settings := &pgs.Setting{}
	var syncMgr *pgs.SyncManager
	if withSync {
		syncMgr = pgs.NewSyncManager(pgs.ReposManager)
		t.Cleanup(syncMgr.Stop)
	}
	return NewHTTPHandler(pgs.ReposManager, settings, syncMgr)
}

func TestHealthzOK(t *testing.T) {
	h := newTestHandler(t, true)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Checks["gitRoot"] != "ok" {
		t.Errorf("gitRoot check = %q", body.Checks["gitRoot"])
	}
	if body.Checks["syncManager"] != "ok" {
		t.Errorf("syncManager check = %q", body.Checks["syncManager"])
	}
}

// gitRoot 不存在时健康检查应报 503（degraded）。
func TestHealthzDegradedOnMissingGitRoot(t *testing.T) {
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	settings := &pgs.Setting{}
	h := NewHTTPHandler(pgs.ReposManager, settings, nil)

	// 让仓库根失效
	pgs.ReposManager.Config.GitRoot = dir + "/does-not-exist"
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "degraded") {
		t.Errorf("body should report degraded: %s", rec.Body.String())
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newTestHandler(t, false)
	pgs.ObserveGitOperation("upload-pack", "success")

	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Errorf("Content-Type = %q, want Prometheus text format", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"# TYPE pgit_git_operations_total counter",
		`pgit_git_operations_total{service="upload-pack",result="success"}`,
		"pgit_repositories_total",
		"pgit_pack_inflight",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q\n---\n%s", want, out)
		}
	}
}

// 健康检查与指标不受 BasicAuth 拦截（探针/抓取端无需凭据）。
func TestHealthAndMetricsBypassBasicAuth(t *testing.T) {
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	settings := &pgs.Setting{HttpAuth: true, Credentials: map[string]string{"u": "p"}}
	h := NewHTTPHandler(pgs.ReposManager, settings, nil)

	for _, path := range []string{"/healthz", "/metrics"} {
		rec := httptest.NewRecorder()
		h.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, rec.Code)
		}
	}
	// 对照：管理 API 仍需凭据
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/v1/repos status = %d, want 401", rec.Code)
	}
}

// 请求日志/中间件会采集 HTTP 指标，且请求 ID 出现在响应头。
func TestRequestIDAndHTTPMetrics(t *testing.T) {
	h := newTestHandler(t, false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)
	req.Header.Set("X-Request-Id", "trace-abc")
	h.router.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != "trace-abc" {
		t.Errorf("X-Request-Id = %q, want passthrough", got)
	}

	rec2 := httptest.NewRecorder()
	h.router.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil))
	if got := rec2.Header().Get("X-Request-Id"); len(got) != 32 {
		t.Errorf("generated X-Request-Id = %q, want 32-hex", got)
	}

	out := string(pgs.DefaultRegistry().Render())
	if !strings.Contains(out, `pgit_http_requests_total{method="GET",status="200",result="ok"}`) {
		t.Errorf("http request metric not recorded:\n%s", out)
	}
}

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"pgit/internal/pgs"
)

// fakeGitHubAPI 是发现/导入测试用的假 GitHub API。
type fakeGitHubAPI struct {
	mu         sync.Mutex
	paths      []string
	authHeader []string
	repos      []map[string]any
	notFound   bool
	rateLimit  bool
}

func (f *fakeGitHubAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path+"?"+r.URL.RawQuery)
		f.authHeader = append(f.authHeader, r.Header.Get("Authorization"))
		f.mu.Unlock()

		switch {
		case f.rateLimit:
			w.Header().Set("x-ratelimit-remaining", "0")
			w.Header().Set("x-ratelimit-reset", "1800000000")
			w.WriteHeader(http.StatusForbidden)
		case f.notFound:
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/user":
			write(w, map[string]any{"login": "LaoQi"})
		default:
			write(w, f.repos)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newGithubTestHandler(t *testing.T) (*HTTPHandler, *pgs.RepositoriesManager, *pgs.SyncManager) {
	t.Helper()
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	syncMgr := pgs.NewSyncManager(pgs.ReposManager)
	settings := &pgs.Setting{WebUIPrefix: "__webui"}
	h := NewHTTPHandler(pgs.ReposManager, settings, syncMgr)
	t.Cleanup(syncMgr.Stop)
	return h, pgs.ReposManager, syncMgr
}

func githubAPIRepo(name string, extra map[string]any) map[string]any {
	m := map[string]any{
		"full_name":      "LaoQi/" + name,
		"name":           name,
		"owner":          map[string]any{"login": "LaoQi"},
		"description":    name + " desc",
		"default_branch": "main",
		"size":           10,
		"updated_at":     "2026-09-01T00:00:00Z",
		"clone_url":      "https://github.com/LaoQi/" + name + ".git",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestGithubReposEndpoint(t *testing.T) {
	h, _, _ := newGithubTestHandler(t)
	fake := &fakeGitHubAPI{repos: []map[string]any{
		githubAPIRepo("alpha", nil),
		githubAPIRepo("forked", map[string]any{"fork": true}),
	}}
	srv := fake.server(t)

	// owner 必填
	rec := do(h, http.MethodGet, "/api/v1/github/repos")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	path := "/api/v1/github/repos?owner=LaoQi&apiBase=" + url.QueryEscape(srv.URL)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Github-Token", "tok")
	rec = httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Owner        string           `json:"owner"`
		Total        int              `json:"total"`
		Creatable    int              `json:"creatable"`
		Repositories []pgs.GithubRepo `json:"repositories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 1 || body.Creatable != 1 {
		t.Fatalf("body = %+v, want 1 repo (fork filtered)", body)
	}
	if body.Repositories[0].LocalName != "LaoQi_alpha" || body.Repositories[0].Conflict != "" {
		t.Errorf("repo = %+v, want localName LaoQi_alpha without conflict", body.Repositories[0])
	}
	// Token 只在请求头里，不应出现在 URL 路径/查询串
	for _, p := range fake.paths {
		if strings.Contains(p, "tok") {
			t.Errorf("token leaked into request path: %s", p)
		}
	}
	for _, a := range fake.authHeader {
		if a != "Bearer tok" {
			t.Errorf("auth = %q, want Bearer tok", a)
		}
	}
}

func TestGithubReposEndpointErrorMapping(t *testing.T) {
	h, _, _ := newGithubTestHandler(t)

	limited := &fakeGitHubAPI{rateLimit: true}
	limitedSrv := limited.server(t)
	rec := do(h, http.MethodGet, "/api/v1/github/repos?owner=LaoQi&apiBase="+url.QueryEscape(limitedSrv.URL))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("rate limited status = %d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}

	missing := &fakeGitHubAPI{notFound: true}
	missingSrv := missing.server(t)
	rec = do(h, http.MethodGet, "/api/v1/github/repos?owner=nobody&apiBase="+url.QueryEscape(missingSrv.URL))
	if rec.Code != http.StatusNotFound {
		t.Errorf("not found status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}

	// 上游不可达（端口未监听）→ 502
	rec = do(h, http.MethodGet, "/api/v1/github/repos?owner=LaoQi&apiBase=http%3A%2F%2F127.0.0.1%3A1")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("unreachable upstream status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestGithubReposEndpointInvalidToken(t *testing.T) {
	h, _, _ := newGithubTestHandler(t)
	fake := &fakeGitHubAPI{}
	srv := fake.server(t)

	// 让 /user 返回 401（伪造无效 Token 场景）
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(unauthorized.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/github/repos?owner=LaoQi&apiBase="+url.QueryEscape(unauthorized.URL), nil)
	req.Header.Set("X-Github-Token", "bad")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("invalid token status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "bad") {
		t.Error("token must not be echoed back in the error body")
	}

	// 正常令牌但账号不存在 → 404（保持既有映射）
	missing := &fakeGitHubAPI{notFound: true}
	missingSrv := missing.server(t)
	rec = do(h, http.MethodGet, "/api/v1/github/repos?owner=nobody&apiBase="+url.QueryEscape(missingSrv.URL))
	if rec.Code != http.StatusNotFound {
		t.Errorf("not found status = %d, want 404", rec.Code)
	}
	_ = srv
}

func TestGithubImportEndpoint(t *testing.T) {
	h, manager, syncMgr := newGithubTestHandler(t)
	fake := &fakeGitHubAPI{repos: []map[string]any{
		githubAPIRepo("alpha", nil),
		githubAPIRepo("beta", map[string]any{"default_branch": "master"}),
	}}
	srv := fake.server(t)

	form := url.Values{}
	form.Set("owner", "LaoQi")
	form.Set("apiBase", srv.URL)
	form.Set("syncInterval", "600")
	form.Add("repos", "alpha")
	form.Add("repos", "beta")
	form.Add("repos", "ghost")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/github/import", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Github-Token", "tok")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		OK      bool                     `json:"ok"`
		Created int                      `json:"created"`
		Failed  int                      `json:"failed"`
		Results []pgs.GithubImportResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Created != 2 || body.Failed != 1 || body.OK {
		t.Fatalf("body = %+v, want 2 created 1 failed", body)
	}

	repo, err := manager.GetRepository("LaoQi_alpha")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Mirror == nil || repo.Mirror.SyncInterval != 600 || repo.Mirror.Password != "tok" {
		t.Errorf("mirror = %+v, want interval 600 + token auth", repo.Mirror)
	}
	if _, err := manager.GetByAlias("LaoQi/alpha"); err != nil {
		t.Errorf("alias LaoQi/alpha missing: %v", err)
	}
	if st, err := syncMgr.Status("LaoQi_alpha"); err != nil || !st.Scheduled {
		t.Errorf("mirror not scheduled: %+v %v", st, err)
	}

	// 二次导入：全部已是镜像 → 200 但 failed=2、ok=false
	form.Del("repos")
	form.Add("repos", "alpha")
	form.Add("repos", "beta")
	req = httptest.NewRequest(http.MethodPost, "/api/v1/github/import", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second import status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Created != 0 || body.Failed != 2 || body.OK {
		t.Errorf("second import = %+v, want 0 created 2 failed", body)
	}

	// 缺少 repos → 400
	rec = do(h, http.MethodPost, "/api/v1/github/import?owner=LaoQi&apiBase="+url.QueryEscape(srv.URL))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing repos status = %d, want 400", rec.Code)
	}
}

// 管理 API 文档需覆盖新端点（防止文档与实际路由漂移）。
func TestAPIDocsCoverGithubEndpoints(t *testing.T) {
	h, _, _ := newGithubTestHandler(t)
	rec := do(h, http.MethodGet, "/api/v1/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"/api/v1/github/repos", "/api/v1/github/import"} {
		if !strings.Contains(body, want) {
			t.Errorf("api docs missing %s", want)
		}
	}
}

// mirror-status 需包含 queued 字段（任务队列状态）。
func TestMirrorStatusIncludesQueued(t *testing.T) {
	h, manager, _ := newGithubTestHandler(t)
	mirror := &pgs.MirrorConfig{RemoteURL: "http://127.0.0.1:1/none.git", SyncInterval: 0, AuthType: "none"}
	if err := manager.CreateMirrorRepository("m1", "", mirror); err != nil {
		t.Fatal(err)
	}
	rec := do(h, http.MethodGet, "/api/v1/repos/m1/mirror-status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"queued"`) {
		t.Errorf("mirror-status body missing queued: %s", rec.Body.String())
	}
}

package pgs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGithub 是一个最小可用的 GitHub API 假服务，用于发现/导入逻辑的单元测试。
type fakeGithub struct {
	mu       sync.Mutex
	requests []string
	auth     []string
	// login 是 /user 返回的登录名（空 = 该端点 404）
	login string
	// userRepos / orgRepos 分别对应 /users/{owner}/repos 与 /orgs/{owner}/repos
	userRepos []map[string]any
	orgRepos  []map[string]any
	// userStatus 强制 /users/{owner}/repos 返回的状态码（0 = 200）
	userStatus int
	// rateLimited 为真时所有列表端点返回 403 + 限流头
	rateLimited bool
	// userUnauthorized 为真时 /user 返回 401
	userUnauthorized bool
}

func (f *fakeGithub) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
}

func (f *fakeGithub) seenPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeGithub) authHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}

func githubRepoJSON(name string, extra map[string]any) map[string]any {
	m := map[string]any{
		"full_name":      "LaoQi/" + name,
		"name":           name,
		"owner":          map[string]any{"login": "LaoQi"},
		"private":        false,
		"fork":           false,
		"archived":       false,
		"description":    name + " desc",
		"default_branch": "main",
		"size":           42,
		"updated_at":     "2026-09-01T00:00:00Z",
		"clone_url":      "https://github.com/LaoQi/" + name + ".git",
		"ssh_url":        "git@github.com:LaoQi/" + name + ".git",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (f *fakeGithub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if f.userUnauthorized {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.login == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"login": f.login})
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, f.userRepos)
	})
	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if f.rateLimited {
			w.Header().Set("x-ratelimit-remaining", "0")
			w.Header().Set("x-ratelimit-reset", "1800000000")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if f.userStatus != 0 {
			w.WriteHeader(f.userStatus)
			return
		}
		writeJSON(w, f.userRepos)
	})
	// 单仓库元数据端点（导入兜底用）：/repos/{owner}/{repo}
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
		if len(parts) != 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for _, item := range append(append([]map[string]any{}, f.userRepos...), f.orgRepos...) {
			if name, _ := item["name"].(string); strings.EqualFold(name, parts[1]) {
				writeJSON(w, item)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/orgs/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if len(f.orgRepos) == 0 {
			w.WriteHeader(http.StatusNotFound) // 组织不存在
			return
		}
		writeJSON(w, f.orgRepos)
	})
	// 分页入口：/owner/repos?page=2 之类（仅测试用）
	mux.HandleFunc("/paged/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if r.URL.Path == "/paged/users/LaoQi/repos" {
			writeJSON(w, f.userRepos)
			return
		}
		if r.URL.Path == "/paged/users/LaoQi/repos2" {
			writeJSON(w, f.orgRepos)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDiscoverGithubReposPublicFiltersAndNaming(t *testing.T) {
	fake := &fakeGithub{userRepos: []map[string]any{
		githubRepoJSON("zeta", nil),
		githubRepoJSON("forked", map[string]any{"fork": true}),
		githubRepoJSON("old", map[string]any{"archived": true}),
		githubRepoJSON("alpha", map[string]any{"default_branch": "master"}),
	}}
	srv := fake.server(t)

	repos, err := DiscoverGithubRepos(GithubDiscoverQuery{Owner: "LaoQi", APIBase: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("repos = %d (%v), want 2 (fork and archived filtered out)", len(repos), names(repos))
	}
	if repos[0].Name != "alpha" || repos[1].Name != "zeta" {
		t.Errorf("not sorted by name: %v", names(repos))
	}
	if repos[0].LocalName != "LaoQi_alpha" {
		t.Errorf("localName = %q, want LaoQi_alpha", repos[0].LocalName)
	}
	if repos[0].DefaultBranch != "master" {
		t.Errorf("defaultBranch = %q, want master", repos[0].DefaultBranch)
	}
	if repos[0].SizeKB != 42 || repos[0].CloneURL == "" {
		t.Errorf("size/cloneUrl not mapped: %+v", repos[0])
	}
	// 无 token：不应带 Authorization 头
	for _, a := range fake.authHeaders() {
		if a != "" {
			t.Errorf("unexpected auth header without token: %q", a)
		}
	}

	// 包含 fork（仍排除 archived）
	withForks, err := DiscoverGithubRepos(GithubDiscoverQuery{Owner: "LaoQi", APIBase: srv.URL, IncludeForks: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withForks) != 3 {
		t.Errorf("repos = %d, want 3 when including forks", len(withForks))
	}
	// 全包含
	all, err := DiscoverGithubRepos(GithubDiscoverQuery{Owner: "LaoQi", APIBase: srv.URL, IncludeForks: true, IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("repos = %d, want 4 when including forks and archived", len(all))
	}
}

func names(repos []GithubRepo) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.Name)
	}
	return out
}

func TestDiscoverGithubReposOwnAccountUsesUserRepos(t *testing.T) {
	fake := &fakeGithub{
		login:     "laoqi", // 大小写不同也算本人
		userRepos: []map[string]any{githubRepoJSON("priv", map[string]any{"private": true})},
	}
	srv := fake.server(t)

	repos, err := DiscoverGithubRepos(GithubDiscoverQuery{Owner: "LaoQi", Token: "tok", APIBase: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || !repos[0].Private {
		t.Fatalf("repos = %+v, want 1 private repo", repos)
	}
	paths := fake.seenPaths()
	if len(paths) < 2 || paths[0] != "/user?" {
		t.Errorf("first request = %v, want /user", paths)
	}
	if paths[1] != "/user/repos?affiliation=owner&per_page=100&sort=full_name&visibility=all" {
		t.Errorf("list request = %q, want /user/repos with per_page/sort", paths[1])
	}
	for _, a := range fake.authHeaders() {
		if a != "Bearer tok" {
			t.Errorf("auth header = %q, want Bearer tok", a)
		}
	}
}

func TestDiscoverGithubReposOrgFallback(t *testing.T) {
	fake := &fakeGithub{
		userStatus: http.StatusNotFound,
		orgRepos:   []map[string]any{githubRepoJSON("orgrepo", nil)},
	}
	srv := fake.server(t)

	repos, err := DiscoverGithubRepos(GithubDiscoverQuery{Owner: "acme", APIBase: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Name != "orgrepo" {
		t.Fatalf("repos = %+v, want org repo", repos)
	}
	paths := fake.seenPaths()
	if len(paths) != 2 || paths[0] != "/users/acme/repos?per_page=100&sort=full_name" {
		t.Errorf("paths = %v, want users then orgs", paths)
	}
	// 组织入口需要 type=all（组织拥有的全部仓库，含成员可见的私有）
	if paths[1] != "/orgs/acme/repos?per_page=100&sort=full_name&type=all" {
		t.Errorf("org request = %q, want type=all", paths[1])
	}
}

func TestDiscoverGithubReposNotFound(t *testing.T) {
	fake := &fakeGithub{userStatus: http.StatusNotFound}
	srv := fake.server(t)
	_, err := DiscoverGithubRepos(GithubDiscoverQuery{Owner: "nobody", APIBase: srv.URL})
	if !errors.Is(err, ErrGithubNotFound) {
		t.Fatalf("err = %v, want ErrGithubNotFound", err)
	}
}

func TestDiscoverGithubReposRateLimited(t *testing.T) {
	fake := &fakeGithub{rateLimited: true}
	srv := fake.server(t)
	_, err := DiscoverGithubRepos(GithubDiscoverQuery{Owner: "LaoQi", APIBase: srv.URL})
	if !errors.Is(err, ErrGithubRateLimited) {
		t.Fatalf("err = %v, want ErrGithubRateLimited", err)
	}
}

func TestDiscoverGithubReposInvalidToken(t *testing.T) {
	fake := &fakeGithub{userUnauthorized: true}
	srv := fake.server(t)
	_, err := DiscoverGithubRepos(GithubDiscoverQuery{Owner: "LaoQi", Token: "bad", APIBase: srv.URL})
	if err == nil || errors.Is(err, ErrGithubNotFound) {
		t.Fatalf("err = %v, want invalid token error", err)
	}
}

func TestDiscoverGithubReposPagination(t *testing.T) {
	// 用 Link 头模拟两页（此处直接构造响应，覆盖 parseGithubLinkNext + 循环）
	page2 := []map[string]any{githubRepoJSON("second", nil)}
	page1 := []map[string]any{githubRepoJSON("first", nil)}

	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		var body any
		if r.URL.Query().Get("page") == "2" {
			body = page2
		} else {
			body = page1
			w.Header().Set("Link", fmt.Sprintf(`<%s/paged/users/LaoQi/repos2>; rel="next", <%s/paged/users/LaoQi/repos2>; rel="last"`, srvURL, srvURL))
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/paged/users/LaoQi/repos2", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(page2)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	repos, err := DiscoverGithubRepos(GithubDiscoverQuery{Owner: "LaoQi", APIBase: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].Name != "first" || repos[1].Name != "second" {
		t.Fatalf("repos = %v, want both pages", names(repos))
	}
}

func TestParseGithubLinkNext(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{`<https://api.github.com/x?page=2>; rel="next"`, "https://api.github.com/x?page=2"},
		{`<https://api.github.com/x?page=2>; rel="last"`, ""},
		{`<https://api.github.com/x?page=1>; rel="prev", <https://api.github.com/x?page=3>; rel="next"`, "https://api.github.com/x?page=3"},
	}
	for _, c := range cases {
		if got := parseGithubLinkNext(c.in); got != c.want {
			t.Errorf("parseGithubLinkNext(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidateGithubOwnerAndCloneBase(t *testing.T) {
	for _, bad := range []string{"a/b", "a b", "..", ".hidden"} {
		if err := validateGithubOwner(bad); err == nil {
			t.Errorf("validateGithubOwner(%q) should fail", bad)
		}
	}
	if err := validateGithubOwner("LaoQi"); err != nil {
		t.Errorf("validateGithubOwner(LaoQi) = %v", err)
	}
	for _, bad := range []string{"ftp://x", "https://", "not a url"} {
		if err := validateCloneBase(bad); err == nil {
			t.Errorf("validateCloneBase(%q) should fail", bad)
		}
	}
	if err := validateCloneBase("https://ghe.example.com"); err != nil {
		t.Errorf("validateCloneBase valid = %v", err)
	}
}

// 列表缺项时回退到单仓库接口（分页中断/列表瞬时不一致不应导致导入失败）。
func TestFetchGithubRepoFallback(t *testing.T) {
	fake := &fakeGithub{userRepos: nil, orgRepos: nil}
	srv := fake.server(t)

	// 账号里没有该仓库 → 单仓库接口返回 404
	if _, err := FetchGithubRepo(GithubDiscoverQuery{Owner: "LaoQi", APIBase: srv.URL, NamePrefix: "gh-"}, "alpha"); err == nil {
		t.Fatal("expected not-found without the repo in the fake account")
	}

	fake.orgRepos = []map[string]any{githubRepoJSON("alpha", nil)}
	repo, err := FetchGithubRepo(GithubDiscoverQuery{Owner: "LaoQi", APIBase: srv.URL, NamePrefix: "gh-"}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Name != "alpha" || repo.LocalName != "gh-LaoQi_alpha" || repo.DefaultBranch != "main" {
		t.Errorf("repo = %+v, want alpha with prefixed local name", repo)
	}
	if _, err := FetchGithubRepo(GithubDiscoverQuery{Owner: "LaoQi", APIBase: srv.URL}, "a/b"); err == nil {
		t.Error("repo name with slash should be rejected")
	}
}

// 导入时列表返回空（模拟分页缺项）→ 逐个走单仓库接口兜底。
func TestImportGithubMirrorsListMissingFallback(t *testing.T) {
	manager := newImportTestManager(t)
	fake := &fakeGithub{orgRepos: []map[string]any{githubRepoJSON("alpha", nil)}}
	srv := fake.server(t)

	results, err := ImportGithubMirrors(manager, nil, GithubImportRequest{Owner: "LaoQi", APIBase: srv.URL, Repos: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("results = %+v, want created via single-repo fallback", results)
	}
	if _, err := manager.GetRepository("LaoQi_alpha"); err != nil {
		t.Errorf("repository missing: %v", err)
	}
}

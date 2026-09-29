package pgs

import (
	"strings"
	"sync"
	"testing"
)

func newImportTestManager(t *testing.T) *RepositoriesManager {
	t.Helper()
	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { ReposManager = nil })
	return ReposManager
}

func findResult(results []GithubImportResult, repo string) *GithubImportResult {
	for i := range results {
		if results[i].Repo == repo {
			return &results[i]
		}
	}
	return nil
}

func TestImportGithubMirrorsCreatesMirrors(t *testing.T) {
	manager := newImportTestManager(t)
	fake := &fakeGithub{userRepos: []map[string]any{
		githubRepoJSON("alpha", nil),
		githubRepoJSON("beta", map[string]any{"default_branch": "master"}),
		githubRepoJSON("secret", map[string]any{"private": true}),
	}}
	srv := fake.server(t)

	results, err := ImportGithubMirrors(manager, nil, GithubImportRequest{
		Owner:        "LaoQi",
		Token:        "tok",
		APIBase:      srv.URL,
		SyncInterval: 300,
		Repos:        []string{"alpha", "LaoQi/beta", "secret", "alpha"}, // 含 owner/ 前缀与重复项
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v, want 3 (deduped)", results)
	}
	for _, r := range results {
		if !r.OK {
			t.Fatalf("import %s failed: %s", r.Repo, r.Error)
		}
	}

	repo, err := manager.GetRepository("LaoQi_alpha")
	if err != nil {
		t.Fatal(err)
	}
	m := repo.Mirror
	if m == nil {
		t.Fatal("mirror config missing")
	}
	if m.RemoteURL != "https://github.com/LaoQi/alpha.git" {
		t.Errorf("remoteUrl = %q", m.RemoteURL)
	}
	// public 仓库：匿名同步，token 不落盘
	if m.AuthType != "none" || m.Username != "" || m.Password != "" {
		t.Errorf("public mirror auth = %+v, want none/empty/empty", m)
	}
	if m.SyncInterval != 300 {
		t.Errorf("syncInterval = %d, want 300", m.SyncInterval)
	}
	if repo.Description != "alpha desc" {
		t.Errorf("description = %q", repo.Description)
	}
	if !repo.HasAlias("LaoQi/alpha") || !repo.HasAlias("LaoQi_alpha") {
		t.Errorf("aliases = %v, want name + owner/repo", repo.Aliases)
	}

	// private 仓库：token 落盘为 basic 认证
	priv, err := manager.GetRepository("LaoQi_secret")
	if err != nil {
		t.Fatal(err)
	}
	pm := priv.Mirror
	if pm == nil {
		t.Fatal("private mirror config missing")
	}
	if pm.AuthType != "basic" || pm.Username != "x-access-token" || pm.Password != "tok" {
		t.Errorf("private mirror auth = %+v, want basic/x-access-token/tok", pm)
	}
	if byAlias, err := manager.GetByAlias("LaoQi/alpha"); err != nil || byAlias.Name != "LaoQi_alpha" {
		t.Errorf("alias lookup failed: %v %v", byAlias, err)
	}
	// GitHub 的 default_branch 成为本地默认分支
	if db, err := repo.DefaultBranch(); err != nil || db != "main" {
		t.Errorf("default branch = %q (%v), want main", db, err)
	}
	beta, _ := manager.GetRepository("LaoQi_beta")
	if db, _ := beta.DefaultBranch(); db != "master" {
		t.Errorf("beta default branch = %q, want master", db)
	}
}

func TestImportGithubMirrorsSecondRunSkipsExisting(t *testing.T) {
	manager := newImportTestManager(t)
	fake := &fakeGithub{userRepos: []map[string]any{githubRepoJSON("alpha", nil)}}
	srv := fake.server(t)

	req := GithubImportRequest{Owner: "LaoQi", APIBase: srv.URL, Repos: []string{"alpha"}}
	if _, err := ImportGithubMirrors(manager, nil, req); err != nil {
		t.Fatal(err)
	}
	results, err := ImportGithubMirrors(manager, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].OK {
		t.Fatalf("second import = %+v, want failure (already mirrored)", results)
	}
	if !strings.Contains(results[0].Error, "already mirrored") {
		t.Errorf("error = %q, want already-mirrored hint", results[0].Error)
	}
	created, failed := SummarizeGithubImport(results)
	if created != 0 || failed != 1 {
		t.Errorf("summary = %d/%d, want 0 created 1 failed", created, failed)
	}
}

func TestImportGithubMirrorsConflictWithNonMirror(t *testing.T) {
	manager := newImportTestManager(t)
	fake := &fakeGithub{userRepos: []map[string]any{githubRepoJSON("alpha", nil)}}
	srv := fake.server(t)

	if err := manager.CreateRepository("LaoQi_alpha", "local repo", "master"); err != nil {
		t.Fatal(err)
	}
	results, err := ImportGithubMirrors(manager, nil, GithubImportRequest{Owner: "LaoQi", APIBase: srv.URL, Repos: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].OK || !strings.Contains(results[0].Error, "not a mirror") {
		t.Fatalf("result = %+v, want not-a-mirror conflict", results[0])
	}
	// 原有仓库不被改动
	repo, _ := manager.GetRepository("LaoQi_alpha")
	if repo.IsMirror() || repo.Description != "local repo" {
		t.Errorf("existing repo was modified: %+v", repo)
	}
}

func TestImportGithubMirrorsAliasConflict(t *testing.T) {
	manager := newImportTestManager(t)
	fake := &fakeGithub{userRepos: []map[string]any{githubRepoJSON("alpha", nil)}}
	srv := fake.server(t)

	if err := manager.CreateRepository("holder", "", "master"); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddAlias("holder", "LaoQi/alpha"); err != nil {
		t.Fatal(err)
	}
	results, err := ImportGithubMirrors(manager, nil, GithubImportRequest{Owner: "LaoQi", APIBase: srv.URL, Repos: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].OK || !strings.Contains(results[0].Error, "alias") {
		t.Fatalf("result = %+v, want alias conflict", results[0])
	}
	if manager.RepositoryExist("LaoQi_alpha") {
		t.Error("repository should not be created on alias conflict")
	}
}

func TestImportGithubMirrorsCloneBaseAndNoToken(t *testing.T) {
	manager := newImportTestManager(t)
	fake := &fakeGithub{userRepos: []map[string]any{githubRepoJSON("alpha", nil)}}
	srv := fake.server(t)

	results, err := ImportGithubMirrors(manager, nil, GithubImportRequest{
		Owner:     "LaoQi",
		APIBase:   srv.URL,
		CloneBase: "https://git.example.com/",
		Repos:     []string{"alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].OK {
		t.Fatalf("import failed: %s", results[0].Error)
	}
	repo, _ := manager.GetRepository("LaoQi_alpha")
	if repo.Mirror.RemoteURL != "https://git.example.com/LaoQi/alpha.git" {
		t.Errorf("remoteUrl = %q, want cloneBase override", repo.Mirror.RemoteURL)
	}
	if repo.Mirror.AuthType != "none" || repo.Mirror.Password != "" {
		t.Errorf("auth = %+v, want none without token", repo.Mirror)
	}
}

func TestImportGithubMirrorsValidation(t *testing.T) {
	manager := newImportTestManager(t)
	fake := &fakeGithub{userRepos: []map[string]any{githubRepoJSON("alpha", nil)}}
	srv := fake.server(t)

	cases := []struct {
		name string
		req  GithubImportRequest
		want string
	}{
		{"empty owner", GithubImportRequest{APIBase: srv.URL, Repos: []string{"alpha"}}, "owner is required"},
		{"bad owner", GithubImportRequest{Owner: "a/b", APIBase: srv.URL, Repos: []string{"alpha"}}, "invalid github owner"},
		{"no repos", GithubImportRequest{Owner: "LaoQi", APIBase: srv.URL}, "no repositories selected"},
		{"bad name", GithubImportRequest{Owner: "LaoQi", APIBase: srv.URL, Repos: []string{"x/y/z"}}, "invalid repository name"},
		{"negative interval", GithubImportRequest{Owner: "LaoQi", APIBase: srv.URL, Repos: []string{"alpha"}, SyncInterval: -1}, "syncInterval"},
		{"bad cloneBase", GithubImportRequest{Owner: "LaoQi", APIBase: srv.URL, Repos: []string{"alpha"}, CloneBase: "ftp://x"}, "cloneBase"},
	}
	for _, c := range cases {
		if _, err := ImportGithubMirrors(manager, nil, c.req); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want contains %q", c.name, err, c.want)
		}
	}

	tooMany := make([]string, maxGithubImportRepos+1)
	for i := range tooMany {
		tooMany[i] = "r" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if _, err := ImportGithubMirrors(manager, nil, GithubImportRequest{Owner: "LaoQi", APIBase: srv.URL, Repos: tooMany}); err == nil ||
		!strings.Contains(err.Error(), "too many repositories") {
		t.Errorf("too many: err = %v, want limit error", err)
	}
}

func TestImportGithubMirrorsMissingRepoAndRegistersSync(t *testing.T) {
	manager := newImportTestManager(t)
	fake := &fakeGithub{userRepos: []map[string]any{
		githubRepoJSON("alpha", nil),
		githubRepoJSON("hidden", map[string]any{"private": true}),
	}}
	srv := fake.server(t)

	syncMgr := NewSyncManager(manager)
	t.Cleanup(syncMgr.Stop)

	results, err := ImportGithubMirrors(manager, syncMgr, GithubImportRequest{
		Owner:        "LaoQi",
		APIBase:      srv.URL,
		SyncInterval: 600,
		Repos:        []string{"alpha", "ghost"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := findResult(results, "ghost"); r == nil || r.OK || !strings.Contains(r.Error, "not found") {
		t.Errorf("ghost result = %+v, want not-found error", r)
	}
	if r := findResult(results, "alpha"); r == nil || !r.OK {
		t.Fatalf("alpha result = %+v, want ok", r)
	}
	st, err := syncMgr.Status("LaoQi_alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Scheduled || st.IntervalSec != 600 {
		t.Errorf("status = %+v, want scheduled with 600s", st)
	}
}

func TestAnnotateGithubRepos(t *testing.T) {
	manager := newImportTestManager(t)

	if err := manager.CreateRepository("LaoQi_plain", "", "master"); err != nil {
		t.Fatal(err)
	}
	same := &MirrorConfig{RemoteURL: "https://github.com/LaoQi/same.git"}
	if err := manager.CreateMirrorRepository("LaoQi_same", "", same); err != nil {
		t.Fatal(err)
	}
	diff := &MirrorConfig{RemoteURL: "https://github.com/LaoQi/same.git"}
	if err := manager.CreateMirrorRepositoryWithBranch("LaoQi_diff", "", diff, "main"); err != nil {
		t.Fatal(err)
	}
	// 不同远端：改远端后校验
	if _, err := manager.UpdateRepositorySettings("LaoQi_diff", "", &MirrorConfig{
		RemoteURL: "https://github.com/other/diff.git", AuthType: "none",
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.CreateRepository("holder", "", "master"); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddAlias("holder", "LaoQi/aliased"); err != nil {
		t.Fatal(err)
	}

	repos := []GithubRepo{
		{Name: "plain", Owner: "LaoQi", CloneURL: "https://github.com/LaoQi/plain.git"},
		{Name: "same", Owner: "LaoQi", CloneURL: "https://github.com/LaoQi/same.git"},
		{Name: "diff", Owner: "LaoQi", CloneURL: "https://github.com/LaoQi/diff.git"},
		{Name: "aliased", Owner: "LaoQi", CloneURL: "https://github.com/LaoQi/aliased.git"},
		{Name: "free", Owner: "LaoQi", CloneURL: "https://github.com/LaoQi/free.git"},
	}
	manager.AnnotateGithubRepos(repos, "")
	want := map[string]string{
		"plain":   "not-a-mirror",
		"same":    "mirror-same-remote",
		"diff":    "mirror-other-remote",
		"aliased": "alias-exists",
		"free":    "",
	}
	for _, r := range repos {
		if r.Conflict != want[r.Name] {
			t.Errorf("%s conflict = %q, want %q", r.Name, r.Conflict, want[r.Name])
		}
		if r.LocalName != "LaoQi_"+r.Name {
			t.Errorf("%s localName = %q", r.Name, r.LocalName)
		}
	}

	// namePrefix 参与本地命名
	prefixed := []GithubRepo{{Name: "x", Owner: "LaoQi"}}
	manager.AnnotateGithubRepos(prefixed, "gh-")
	if prefixed[0].LocalName != "gh-LaoQi_x" {
		t.Errorf("localName = %q, want gh-LaoQi_x", prefixed[0].LocalName)
	}
}

// 并发导入同一仓库：只允许一个成功，另一个报冲突，且不留下重复/半成品状态。
func TestImportGithubMirrorsConcurrentSameRepo(t *testing.T) {
	manager := newImportTestManager(t)
	fake := &fakeGithub{userRepos: []map[string]any{githubRepoJSON("alpha", nil)}}
	srv := fake.server(t)

	const n = 4
	results := make([]GithubImportResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := ImportGithubMirrors(manager, nil, GithubImportRequest{Owner: "LaoQi", APIBase: srv.URL, Repos: []string{"alpha"}})
			if err != nil {
				results[i] = GithubImportResult{Error: err.Error()}
				return
			}
			results[i] = res[0]
		}(i)
	}
	wg.Wait()

	okCount, failCount := 0, 0
	for _, r := range results {
		if r.OK {
			okCount++
		} else {
			failCount++
			if r.Error == "" {
				t.Errorf("failed result without error: %+v", r)
			}
		}
	}
	if okCount != 1 || failCount != n-1 {
		t.Fatalf("ok=%d fail=%d, want 1 ok and %d failures (%+v)", okCount, failCount, n-1, results)
	}

	repos := manager.List()
	if len(repos) != 1 {
		t.Fatalf("repos = %d, want exactly 1", len(repos))
	}
	if _, err := manager.GetByAlias("LaoQi/alpha"); err != nil {
		t.Errorf("alias missing after concurrent import: %v", err)
	}
	repo := repos[0]
	if len(repo.Aliases) != 2 {
		t.Errorf("aliases = %v, want name + owner/repo (no duplicates)", repo.Aliases)
	}
}

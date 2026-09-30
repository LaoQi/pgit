package server

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"pgit/internal/pgs"
	"pgit/internal/pgs/git"
)

// --- 测试辅助 ---

type relayServerEnv struct {
	handler     *HTTPHandler
	manager     *pgs.RepositoriesManager
	relay       *pgs.RelayManager
	upstream    *pgs.Repository
	upstreamURL string
	upstreamSrv *httptest.Server
}

// newRelayServerHandler 构造带 RelayManager 的 HTTPHandler，并起一个「上游 pgit 实例」
// （用 git 包的协议实现直接当服务端，等价于真实 GitHub 的 smart-http 行为）。
func newRelayServerHandler(t *testing.T) *relayServerEnv {
	t.Helper()
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	settings := &pgs.Setting{WebUIPrefix: "__webui"}
	syncMgr := pgs.NewSyncManager(pgs.ReposManager)
	relayMgr := pgs.NewRelayManager(pgs.ReposManager)
	t.Cleanup(syncMgr.Stop)
	t.Cleanup(relayMgr.Stop)
	h := NewHTTPHandler(pgs.ReposManager, settings, syncMgr, relayMgr)

	upstream, err := pgs.InitBare(t.TempDir(), "upstream", "upstream", "master")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	serve := func(service string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
			out, err := git.ServeInfoRefs(upstream.Path(), service)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(out)
		}
	}
	mux.HandleFunc("/up.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		serve(r.URL.Query().Get("service"))(w, r)
	})
	mux.HandleFunc("/up.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(http.StatusOK)
		if err := git.HandleUploadPack(upstream.Path(), r.Body, w); err != nil {
			t.Logf("upstream upload-pack: %v", err)
		}
	})
	mux.HandleFunc("/up.git/git-receive-pack", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		if _, err := git.HandleReceivePack(upstream.Path(), r.Body, w); err != nil {
			t.Logf("upstream receive-pack: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &relayServerEnv{
		handler:     h,
		manager:     pgs.ReposManager,
		relay:       relayMgr,
		upstream:    upstream,
		upstreamURL: srv.URL + "/up.git",
		upstreamSrv: srv,
	}
}

func (e *relayServerEnv) postForm(t *testing.T, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	e.handler.router.ServeHTTP(rec, req)
	return rec
}

func (e *relayServerEnv) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	e.handler.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func testObject(t *testing.T, repoRoot string, typ git.ObjectType, content []byte) git.Oid {
	t.Helper()
	store := git.NewObjectStore(repoRoot)
	oid, err := store.Write(git.NewRawObject(typ, content))
	if err != nil {
		t.Fatalf("write object: %v", err)
	}
	return oid
}

func testCommit(t *testing.T, repoRoot string, parent git.Oid, file, content string) git.Oid {
	t.Helper()
	blob := testObject(t, repoRoot, git.ObjBlob, []byte(content))
	bin, err := hex.DecodeString(blob.String())
	if err != nil {
		t.Fatal(err)
	}
	tree := testObject(t, repoRoot, git.ObjTree, append([]byte("100644 "+file+"\x00"), bin...))
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	if !parent.IsZero() {
		fmt.Fprintf(&b, "parent %s\n", parent)
	}
	b.WriteString("author T <t@pgit.dev> 1700000000 +0800\ncommitter T <t@pgit.dev> 1700000000 +0800\n\nm\n")
	return testObject(t, repoRoot, git.ObjCommit, []byte(b.String()))
}

func testSetRef(t *testing.T, repoRoot, ref string, oid git.Oid) {
	t.Helper()
	rs := git.NewRefStore(repoRoot)
	cur, err := rs.Get(ref)
	if err != nil {
		cur = git.ZeroOid
	}
	results, err := rs.Update([]git.RefUpdate{{Name: ref, OldOid: cur, NewOid: oid}})
	if err != nil || len(results) != 1 || !results[0].Ok {
		t.Fatalf("set ref %s: err=%v results=%+v", ref, err, results)
	}
}

// --- API ---

func TestRelayAPI_CreateConfigureAndInspect(t *testing.T) {
	env := newRelayServerHandler(t)

	form := url.Values{}
	form.Set("name", "relay-repo")
	form.Set("description", "relay to upstream")
	form.Set("mirrorUrl", env.upstreamURL)
	form.Set("mirrorMode", "relay")
	form.Set("relayRefs", "refs/heads/, refs/tags/")
	form.Set("relayAllowDelete", "false")
	rec := env.postForm(t, "/api/v1/repos", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created pgs.Repository
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Mirror == nil || created.Mirror.Mode != pgs.MirrorModeRelay {
		t.Fatalf("created = %+v", created.Mirror)
	}
	if created.Mirror.AllowsDelete() {
		t.Error("relayAllowDelete=false 应生效")
	}
	if len(created.Mirror.Refs) != 2 {
		t.Errorf("relayRefs = %v", created.Mirror.Refs)
	}

	// status：未校准（还没有 push）时 baseAt 为零
	rec = env.get(t, "/api/v1/repos/relay-status?ref=relay-repo")
	if rec.Code != http.StatusOK {
		t.Fatalf("relay-status status = %d body=%s", rec.Code, rec.Body.String())
	}
	var st pgs.RelayStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Upstream != env.upstreamURL || st.Repo != "relay-repo" {
		t.Errorf("status = %+v", st)
	}

	// 日志端点：尚无记录
	rec = env.get(t, "/api/v1/repos/relay-log?ref=relay-repo&limit=10")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "entries") {
		t.Fatalf("relay-log status = %d body=%s", rec.Code, rec.Body.String())
	}

	// settings：未提供 mirrorMode 时保持 relay（不能因为改描述而降级）
	form = url.Values{}
	form.Set("ref", "relay-repo")
	form.Set("description", "updated description")
	form.Set("mirrorRemoteUrl", env.upstreamURL)
	form.Set("mirrorAuthType", "none")
	rec = env.postForm(t, "/api/v1/repos/settings", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings status = %d body=%s", rec.Code, rec.Body.String())
	}
	repo, _ := env.manager.GetRepository("relay-repo")
	if !repo.IsRelay() {
		t.Fatalf("settings 后 relay 模式丢失: %+v", repo.Mirror)
	}
	if repo.Description != "updated description" {
		t.Errorf("description = %q", repo.Description)
	}
	// relay 字段未提供=保留原值（不得静默重置为默认）
	if len(repo.Mirror.Refs) != 2 {
		t.Errorf("relayRefs 被静默重置: %v", repo.Mirror.Refs)
	}
	if repo.Mirror.AllowsDelete() {
		t.Error("relayAllowDelete 被静默重置为允许删除")
	}

	// 显式提供则覆盖
	form = url.Values{}
	form.Set("ref", "relay-repo")
	form.Set("mirrorRemoteUrl", env.upstreamURL)
	form.Set("mirrorAuthType", "none")
	form.Set("mirrorMode", "relay")
	form.Set("relayRefs", "refs/heads/main")
	form.Set("relayAllowDelete", "true")
	if rec := env.postForm(t, "/api/v1/repos/settings", form); rec.Code != http.StatusOK {
		t.Fatalf("settings(override) status = %d body=%s", rec.Code, rec.Body.String())
	}
	repo, _ = env.manager.GetRepository("relay-repo")
	if len(repo.Mirror.Refs) != 1 || repo.Mirror.Refs[0] != "refs/heads/main" {
		t.Errorf("relayRefs 覆盖失败: %v", repo.Mirror.Refs)
	}
	if !repo.Mirror.AllowsDelete() {
		t.Error("relayAllowDelete=true 覆盖失败")
	}
}

func TestRelayAPI_ErrorCases(t *testing.T) {
	env := newRelayServerHandler(t)

	// 普通仓库
	pForm := url.Values{}
	pForm.Set("name", "plain")
	if rec := env.postForm(t, "/api/v1/repos", pForm); rec.Code != http.StatusOK {
		t.Fatalf("create plain: %d %s", rec.Code, rec.Body.String())
	}

	if rec := env.get(t, "/api/v1/repos/relay-status?ref=plain"); rec.Code != http.StatusBadRequest {
		t.Errorf("relay-status(plain) = %d, want 400", rec.Code)
	}
	pushForm := url.Values{}
	pushForm.Set("ref", "plain")
	if rec := env.postForm(t, "/api/v1/repos/relay/push", pushForm); rec.Code != http.StatusBadRequest {
		t.Errorf("relay/push(plain) = %d, want 400", rec.Code)
	}
	if rec := env.get(t, "/api/v1/repos/relay-log?ref=plain"); rec.Code != http.StatusBadRequest {
		t.Errorf("relay-log(plain) = %d, want 400", rec.Code)
	}

	// 未知仓库
	if rec := env.get(t, "/api/v1/repos/relay-status?ref=ghost"); rec.Code != http.StatusNotFound {
		t.Errorf("relay-status(ghost) = %d, want 404", rec.Code)
	}

	// relay 仓库：align 必须 confirm；上游不可达时 push/align 报 502
	form := url.Values{}
	form.Set("name", "relay-bad")
	form.Set("mirrorUrl", "http://127.0.0.1:1/repo.git")
	form.Set("mirrorMode", "relay")
	if rec := env.postForm(t, "/api/v1/repos", form); rec.Code != http.StatusOK {
		t.Fatalf("create relay-bad: %d %s", rec.Code, rec.Body.String())
	}

	align := url.Values{}
	align.Set("ref", "relay-bad")
	if rec := env.postForm(t, "/api/v1/repos/relay/align", align); rec.Code != http.StatusBadRequest {
		t.Errorf("align without confirm = %d, want 400", rec.Code)
	}
	align.Set("confirm", "wrong-name")
	if rec := env.postForm(t, "/api/v1/repos/relay/align", align); rec.Code != http.StatusBadRequest {
		t.Errorf("align wrong confirm = %d, want 400", rec.Code)
	}
	align.Set("confirm", "relay-bad")
	if rec := env.postForm(t, "/api/v1/repos/relay/align", align); rec.Code != http.StatusBadGateway {
		t.Errorf("align unreachable upstream = %d, want 502", rec.Code)
	}

	pushForm = url.Values{}
	pushForm.Set("ref", "relay-bad")
	if rec := env.postForm(t, "/api/v1/repos/relay/push", pushForm); rec.Code != http.StatusBadGateway {
		t.Errorf("relay/push unreachable upstream = %d, want 502", rec.Code)
	}
}

// TestRelayEndToEnd_HTTPPushForwardsToUpstream 覆盖真实接线：
// 下游 HTTP push → gitTransport 的准入 gate → 本地更新 → 异步转发上游。
func TestRelayEndToEnd_HTTPPushForwardsToUpstream(t *testing.T) {
	env := newRelayServerHandler(t)

	// 上游基线
	base := testCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
	testSetRef(t, env.upstream.Path(), "refs/heads/master", base)

	// 创建 relay 仓库并建立基线（pull）
	form := url.Values{}
	form.Set("name", "relay-e2e")
	form.Set("mirrorUrl", env.upstreamURL)
	form.Set("mirrorMode", "relay")
	if rec := env.postForm(t, "/api/v1/repos", form); rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := env.manager.SyncRepository("relay-e2e"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	// 下游开发者仓库（基于上游基线）
	dev, err := pgs.InitBare(t.TempDir(), "dev", "dev", "master")
	if err != nil {
		t.Fatal(err)
	}
	testCommit(t, dev.Path(), git.ZeroOid, "a.txt", "base\n")
	testSetRef(t, dev.Path(), "refs/heads/master", base)
	next := testCommit(t, dev.Path(), base, "b.txt", "next\n")
	testSetRef(t, dev.Path(), "refs/heads/master", next)

	// pgit 实例对外暴露（httptest），下游用 git.PushRemote 推送
	srv := httptest.NewServer(env.handler.Router())
	t.Cleanup(srv.Close)
	res, err := git.PushRemote(srv.URL+"/relay-e2e.git", dev.Path(),
		[]git.RefSpec{{Ref: "refs/heads/master", Oid: next}}, nil, git.FetchOptions{MaxAttempts: 1})
	if err != nil {
		t.Fatalf("downstream push: %v", err)
	}
	if !res.AllOK() {
		t.Fatalf("下游 push 被拒绝: %+v", res.Failed())
	}

	// 本地立即更新 + 异步转发到上游
	repo, _ := env.manager.GetRepository("relay-e2e")
	if got, _ := git.NewRefStore(repo.Path()).Get("refs/heads/master"); got != next {
		t.Fatalf("relay 本地 ref = %s, want %s", got, next)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if oid, err := git.NewRefStore(env.upstream.Path()).Get("refs/heads/master"); err == nil && oid == next {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("上游未收到转发的 ref")
}

// TestRelayEndToEnd_MirrorStillDenied 确认镜像仓库仍然禁止 push，中转仓库放行。
func TestRelayEndToEnd_MirrorStillDenied(t *testing.T) {
	env := newRelayServerHandler(t)

	form := url.Values{}
	form.Set("name", "mirror1")
	form.Set("mirrorUrl", env.upstreamURL) // 未指定 mode → 镜像
	if rec := env.postForm(t, "/api/v1/repos", form); rec.Code != http.StatusOK {
		t.Fatalf("create mirror: %d %s", rec.Code, rec.Body.String())
	}

	srv := httptest.NewServer(env.handler.Router())
	t.Cleanup(srv.Close)

	// 广告阶段就应被拒绝（403）
	resp, err := http.Get(srv.URL + "/mirror1.git/info/refs?service=git-receive-pack")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("mirror info/refs status = %d, want 403", resp.StatusCode)
	}
	// relay 仓库的同样请求应放行（200）
	relayForm := url.Values{}
	relayForm.Set("name", "relay-ok")
	relayForm.Set("mirrorUrl", env.upstreamURL)
	relayForm.Set("mirrorMode", "relay")
	if rec := env.postForm(t, "/api/v1/repos", relayForm); rec.Code != http.StatusOK {
		t.Fatalf("create relay: %d %s", rec.Code, rec.Body.String())
	}
	resp2, err := http.Get(srv.URL + "/relay-ok.git/info/refs?service=git-receive-pack")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("relay info/refs status = %d, want 200", resp2.StatusCode)
	}
}

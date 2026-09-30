package pgs

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pgit/internal/pgs/git"
)

// --- 测试辅助：构造 git 对象 / refs / 假上游 ---

// relayTestObject 把一个对象写入仓库并返回 oid。
func relayTestObject(t *testing.T, repoRoot string, typ git.ObjectType, content []byte) git.Oid {
	t.Helper()
	store := git.NewObjectStore(repoRoot)
	oid, err := store.Write(git.NewRawObject(typ, content))
	if err != nil {
		t.Fatalf("write %s object: %v", typ, err)
	}
	return oid
}

func relayTestOidBin(t *testing.T, oid git.Oid) []byte {
	t.Helper()
	b, err := hex.DecodeString(oid.String())
	if err != nil {
		t.Fatalf("decode oid %s: %v", oid, err)
	}
	return b
}

// relayTestCommit 在仓库中构造「单文件 + 可选父提交」的提交，返回其 oid。
// 相同的参数在不同仓库得到相同 oid（内容寻址），便于构造「上游与本地同基线」。
func relayTestCommit(t *testing.T, repoRoot string, parent git.Oid, file, content string) git.Oid {
	t.Helper()
	blob := relayTestObject(t, repoRoot, git.ObjBlob, []byte(content))
	treeContent := append([]byte(fmt.Sprintf("100644 %s\x00", file)), relayTestOidBin(t, blob)...)
	tree := relayTestObject(t, repoRoot, git.ObjTree, treeContent)

	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	if !parent.IsZero() {
		fmt.Fprintf(&b, "parent %s\n", parent)
	}
	b.WriteString("author Test <t@pgit.dev> 1700000000 +0800\n")
	b.WriteString("committer Test <t@pgit.dev> 1700000000 +0800\n\n")
	b.WriteString("commit " + file + "\n")
	return relayTestObject(t, repoRoot, git.ObjCommit, []byte(b.String()))
}

// relayTestSetRef 把 ref 指向 oid（不存在则创建）。
func relayTestSetRef(t *testing.T, repoRoot, ref string, oid git.Oid) {
	t.Helper()
	rs := git.NewRefStore(repoRoot)
	cur, err := rs.Get(ref)
	if err != nil {
		cur = git.ZeroOid // ref 不存在（Get 对缺失返回错误）
	}
	results, err := rs.Update([]git.RefUpdate{{Name: ref, OldOid: cur, NewOid: oid}})
	if err != nil {
		t.Fatalf("update %s: %v", ref, err)
	}
	if len(results) != 1 || !results[0].Ok {
		t.Fatalf("update %s rejected: %+v", ref, results)
	}
}

func relayTestRefOid(t *testing.T, repoRoot, ref string) git.Oid {
	t.Helper()
	rs := git.NewRefStore(repoRoot)
	oid, err := rs.Get(ref)
	if err != nil {
		t.Fatalf("get %s: %v", ref, err)
	}
	return oid
}

// relayTestUpstream 用 pgit 自身的协议实现当假上游（与真实 GitHub 行为一致的部分：
// smart-http 广告 + receive-pack CAS）。
type relayTestUpstream struct {
	root    string
	server  *httptest.Server
	posting bool // 为真时 receive-pack 直接返回 5xx（模拟转发阶段故障）
	// beforePost 在第一次 receive-pack 之前执行一次：用于制造「准入后上游又被改动」的竞态。
	beforePost func()
	hookOnce   sync.Once
}

func newRelayTestUpstream(t *testing.T, root string) *relayTestUpstream {
	t.Helper()
	u := &relayTestUpstream{root: root}
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		service := r.URL.Query().Get("service")
		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
		out, err := git.ServeInfoRefs(root, service)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(http.StatusOK)
		if err := git.HandleUploadPack(root, r.Body, w); err != nil {
			t.Logf("fake upstream upload-pack: %v", err)
		}
	})
	mux.HandleFunc("/repo.git/git-receive-pack", func(w http.ResponseWriter, r *http.Request) {
		if u.beforePost != nil {
			u.hookOnce.Do(u.beforePost)
		}
		if u.posting {
			http.Error(w, "upstream unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		if _, err := git.HandleReceivePack(root, r.Body, w); err != nil {
			t.Logf("fake upstream receive-pack: %v", err)
		}
	})
	u.server = httptest.NewServer(mux)
	t.Cleanup(u.server.Close)
	return u
}

func (u *relayTestUpstream) url() string { return u.server.URL + "/repo.git" }

// relayTestEnv 搭建一套完整环境：临时 GitRoot + ReposManager + RelayManager +
// 上游裸仓库（不在 GitRoot 下，避免被扫描）+ 假上游 HTTP 服务。
type relayTestEnv struct {
	gitRoot  string
	upstream *Repository
	server   *relayTestUpstream
	manager  *RepositoriesManager
	relay    *RelayManager
}

func newRelayTestEnv(t *testing.T) *relayTestEnv {
	t.Helper()
	gitRoot := t.TempDir()
	GitRoot = gitRoot
	InitReposManager(&RepositoriesManagerConfig{GitRoot: gitRoot})
	t.Cleanup(func() { ReposManager = nil })

	upstreamDir := t.TempDir()
	upstream, err := InitBare(upstreamDir, "upstream", "fake upstream", "master")
	if err != nil {
		t.Fatalf("init upstream: %v", err)
	}
	rm := NewRelayManager(ReposManager)
	t.Cleanup(rm.Stop)

	return &relayTestEnv{
		gitRoot:  gitRoot,
		upstream: upstream,
		server:   newRelayTestUpstream(t, upstream.Path()),
		manager:  ReposManager,
		relay:    rm,
	}
}

// addRelay 创建一个指向假上游的中转仓库。
func (e *relayTestEnv) addRelay(t *testing.T, name string, mutate func(m *MirrorConfig)) *Repository {
	t.Helper()
	m := &MirrorConfig{
		RemoteURL: e.server.url(),
		AuthType:  "none",
		Mode:      MirrorModeRelay,
	}
	if mutate != nil {
		mutate(m)
	}
	if err := e.manager.CreateMirrorRepository(name, "relay repo", m); err != nil {
		t.Fatalf("CreateMirrorRepository: %v", err)
	}
	repo, err := e.manager.GetRepository(name)
	if err != nil {
		t.Fatalf("GetRepository: %v", err)
	}
	return repo
}

// --- 数据模型 ---

func TestMirrorConfig_RelayHelpers(t *testing.T) {
	m := &MirrorConfig{}
	if !m.AllowsDelete() {
		t.Error("AllowsDelete() default = false, want true（默认允许删除上游 ref）")
	}
	if !m.AcceptsRef("refs/heads/main") || !m.AcceptsRef("refs/tags/v1") {
		t.Error("默认白名单应包含分支与 tag")
	}
	if m.AcceptsRef("refs/notes/commits") || m.AcceptsRef("HEAD") {
		t.Error("默认白名单不应包含 refs/notes 或 HEAD")
	}

	deny := false
	m2 := &MirrorConfig{AllowDelete: &deny, Refs: []string{"refs/heads/release/"}}
	if m2.AllowsDelete() {
		t.Error("AllowDelete=false 应生效")
	}
	if !m2.AcceptsRef("refs/heads/release/1.0") || m2.AcceptsRef("refs/heads/main") {
		t.Error("自定义白名单应只放行前缀内的 ref")
	}

	repo := &Repository{Mirror: &MirrorConfig{Mode: MirrorModeRelay}}
	if !repo.IsRelay() {
		t.Error("IsRelay = false, want true")
	}
	repo.Mirror.Mode = ""
	if repo.IsRelay() {
		t.Error("缺省 mode 应视为镜像（非 relay）")
	}
}

func TestValidateMirror_RelayMode(t *testing.T) {
	base := func() *MirrorConfig {
		return &MirrorConfig{RemoteURL: "https://example.com/a.git"}
	}

	t.Run("非法 mode", func(t *testing.T) {
		m := base()
		m.Mode = "proxy"
		if err := validateMirror(m); err == nil {
			t.Fatal("want error for invalid mode")
		}
	})

	t.Run("非法 ref 白名单", func(t *testing.T) {
		for _, bad := range []string{"", "heads/", "refs/heads/a b", "refs/..x"} {
			m := base()
			m.Mode = MirrorModeRelay
			m.Refs = []string{bad}
			if err := validateMirror(m); err == nil {
				t.Errorf("want error for ref prefix %q", bad)
			}
		}
	})

	t.Run("合法 relay 配置", func(t *testing.T) {
		m := base()
		m.Mode = MirrorModeRelay
		m.Refs = []string{"refs/heads/", "refs/tags/"}
		if err := validateMirror(m); err != nil {
			t.Fatalf("validateMirror: %v", err)
		}
	})

	t.Run("非 relay 清除 relay 字段", func(t *testing.T) {
		m := base()
		m.Refs = []string{"refs/heads/"}
		deny := false
		m.AllowDelete = &deny
		if err := validateMirror(m); err != nil {
			t.Fatalf("validateMirror: %v", err)
		}
		if m.Refs != nil || m.AllowDelete != nil {
			t.Error("镜像模式下应清空 relay 专用字段")
		}
	})
}

func TestCreateRelayRepository_PersistsMode(t *testing.T) {
	env := newRelayTestEnv(t)
	env.addRelay(t, "relay1", func(m *MirrorConfig) {
		m.SyncInterval = 60
		m.Refs = []string{"refs/heads/"}
	})

	data, err := os.ReadFile(filepath.Join(env.gitRoot, "relay1.git", "pgit.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"mode": "relay"`) {
		t.Errorf("pgit.json 未持久化 mode: %s", data)
	}

	// 重新扫描（模拟重启）后仍是 relay
	InitReposManager(&RepositoriesManagerConfig{GitRoot: env.gitRoot})
	repo, err := ReposManager.GetRepository("relay1")
	if err != nil {
		t.Fatalf("GetRepository after rescan: %v", err)
	}
	if !repo.IsRelay() {
		t.Fatal("重启扫描后 IsRelay = false")
	}
	if repo.Mirror.SyncInterval != 60 || len(repo.Mirror.Refs) != 1 {
		t.Errorf("relay 配置未完整恢复: %+v", repo.Mirror)
	}
}

func TestUpdateRepositorySettings_KeepsRelayRuntime(t *testing.T) {
	env := newRelayTestEnv(t)
	env.addRelay(t, "relay1", nil)

	if err := env.manager.UpdateMirrorRuntime("relay1", func(m *MirrorConfig) {
		m.PendingRefs = []string{"refs/heads/main"}
		m.LastPush = time.Now()
		m.LastPushError = "boom"
	}); err != nil {
		t.Fatalf("UpdateMirrorRuntime: %v", err)
	}

	updated := &MirrorConfig{
		RemoteURL: "https://example.com/other.git",
		Mode:      MirrorModeRelay,
		AuthType:  "none",
	}
	if _, err := env.manager.UpdateRepositorySettings("relay1", "new description", updated); err != nil {
		t.Fatalf("UpdateRepositorySettings: %v", err)
	}

	repo, err := env.manager.GetRepository("relay1")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Mirror.RemoteURL != "https://example.com/other.git" {
		t.Errorf("RemoteURL 未更新: %q", repo.Mirror.RemoteURL)
	}
	if len(repo.Mirror.PendingRefs) != 1 || repo.Mirror.LastPushError != "boom" {
		t.Errorf("relay 运行态被设置更新覆盖: %+v", repo.Mirror)
	}
}

func TestSnapshot_DeepCopiesRelayFields(t *testing.T) {
	deny := true
	repo := &Repository{
		Name: "r",
		Mirror: &MirrorConfig{
			Mode:        MirrorModeRelay,
			Refs:        []string{"refs/heads/"},
			PendingRefs: []string{"refs/heads/a"},
			AllowDelete: &deny,
		},
	}
	snap := repo.Snapshot()
	snap.Mirror.Refs[0] = "mutated"
	snap.Mirror.PendingRefs[0] = "mutated"
	*snap.Mirror.AllowDelete = false

	if repo.Mirror.Refs[0] != "refs/heads/" || repo.Mirror.PendingRefs[0] != "refs/heads/a" || *repo.Mirror.AllowDelete != true {
		t.Errorf("快照未深拷贝 relay 字段: %+v", repo.Mirror)
	}
}

// --- 准入矩阵 ---

func TestRelayAdmit_Matrix(t *testing.T) {
	type admitCase struct {
		name       string
		mutate     func(m *MirrorConfig)
		setup      func(t *testing.T, env *relayTestEnv, relay *Repository) (git.RefUpdate, git.Oid)
		wantReject string // 期望拒绝原因包含的子串；空表示放行
	}

	cases := []admitCase{
		{
			name: "新分支（上游没有）放行",
			setup: func(t *testing.T, env *relayTestEnv, relay *Repository) (git.RefUpdate, git.Oid) {
				base := relayTestCommit(t, relay.Path(), git.ZeroOid, "a.txt", "base\n")
				relayTestSetRef(t, relay.Path(), "refs/heads/master", base)
				return git.RefUpdate{Name: "refs/heads/feature", OldOid: git.ZeroOid, NewOid: base}, base
			},
		},
		{
			name: "快进（old=基线且 new 是其后代）放行",
			setup: func(t *testing.T, env *relayTestEnv, relay *Repository) (git.RefUpdate, git.Oid) {
				base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
				relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
				// 本地同基线（内容一致 → 同 oid）
				relayTestCommit(t, relay.Path(), git.ZeroOid, "a.txt", "base\n")
				next := relayTestCommit(t, relay.Path(), base, "b.txt", "next\n")
				relayTestSetRef(t, relay.Path(), "refs/heads/master", next)
				return git.RefUpdate{Name: "refs/heads/master", OldOid: base, NewOid: next}, next
			},
		},
		{
			name: "stale base（old 与上游不一致）拒绝",
			setup: func(t *testing.T, env *relayTestEnv, relay *Repository) (git.RefUpdate, git.Oid) {
				base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
				relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
				next := relayTestCommit(t, relay.Path(), base, "b.txt", "next\n")
				relayTestSetRef(t, relay.Path(), "refs/heads/master", next)
				// 客户端以为基线是 base 但上游已经被别人推进
				other := relayTestCommit(t, env.upstream.Path(), base, "c.txt", "other\n")
				relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", other)
				return git.RefUpdate{Name: "refs/heads/master", OldOid: base, NewOid: next}, next
			},
			wantReject: "stale base",
		},
		{
			name: "上游有该 ref 但客户端以 old=0 推送（基线过期）拒绝",
			setup: func(t *testing.T, env *relayTestEnv, relay *Repository) (git.RefUpdate, git.Oid) {
				base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
				relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
				fresh := relayTestCommit(t, relay.Path(), git.ZeroOid, "x.txt", "fresh\n")
				return git.RefUpdate{Name: "refs/heads/master", OldOid: git.ZeroOid, NewOid: fresh}, fresh
			},
			wantReject: "stale base",
		},
		{
			name: "非快进（new 不是基线后代）拒绝",
			setup: func(t *testing.T, env *relayTestEnv, relay *Repository) (git.RefUpdate, git.Oid) {
				base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
				relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
				relayTestCommit(t, relay.Path(), git.ZeroOid, "a.txt", "base\n")
				side := relayTestCommit(t, relay.Path(), git.ZeroOid, "side.txt", "side\n") // 与 base 无关的另一条链
				return git.RefUpdate{Name: "refs/heads/master", OldOid: base, NewOid: side}, side
			},
			wantReject: "non-fast-forward",
		},
		{
			name: "白名单外 ref 拒绝",
			setup: func(t *testing.T, env *relayTestEnv, relay *Repository) (git.RefUpdate, git.Oid) {
				oid := relayTestCommit(t, relay.Path(), git.ZeroOid, "a.txt", "notes\n")
				return git.RefUpdate{Name: "refs/notes/commits", OldOid: git.ZeroOid, NewOid: oid}, oid
			},
			wantReject: "not forwarded",
		},
		{
			name: "tag 新建放行",
			setup: func(t *testing.T, env *relayTestEnv, relay *Repository) (git.RefUpdate, git.Oid) {
				base := relayTestCommit(t, relay.Path(), git.ZeroOid, "a.txt", "base\n")
				tag := relayTestObject(t, relay.Path(), git.ObjTag,
					[]byte(fmt.Sprintf("object %s\ntype commit\ntag v1\ntagger T <t@pgit.dev> 1700000000 +0800\n\nm\n", base)))
				return git.RefUpdate{Name: "refs/tags/v1", OldOid: git.ZeroOid, NewOid: tag}, tag
			},
		},
		{
			name: "删除上游 ref 默认放行",
			setup: func(t *testing.T, env *relayTestEnv, relay *Repository) (git.RefUpdate, git.Oid) {
				base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
				relayTestSetRef(t, env.upstream.Path(), "refs/heads/gone", base)
				return git.RefUpdate{Name: "refs/heads/gone", OldOid: base, NewOid: git.ZeroOid}, git.ZeroOid
			},
		},
		{
			name:   "AllowDelete=false 时删除拒绝",
			mutate: func(m *MirrorConfig) { deny := false; m.AllowDelete = &deny },
			setup: func(t *testing.T, env *relayTestEnv, relay *Repository) (git.RefUpdate, git.Oid) {
				base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
				relayTestSetRef(t, env.upstream.Path(), "refs/heads/gone", base)
				return git.RefUpdate{Name: "refs/heads/gone", OldOid: base, NewOid: git.ZeroOid}, git.ZeroOid
			},
			wantReject: "delete of upstream ref is disabled",
		},
		{
			name:   "自定义白名单只放行指定前缀",
			mutate: func(m *MirrorConfig) { m.Refs = []string{"refs/heads/release/"} },
			setup: func(t *testing.T, env *relayTestEnv, relay *Repository) (git.RefUpdate, git.Oid) {
				oid := relayTestCommit(t, relay.Path(), git.ZeroOid, "a.txt", "x\n")
				return git.RefUpdate{Name: "refs/heads/main", OldOid: git.ZeroOid, NewOid: oid}, oid
			},
			wantReject: "not forwarded",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRelayTestEnv(t)
			relay := env.addRelay(t, fmt.Sprintf("relay%d", i), tc.mutate)
			update, _ := tc.setup(t, env, relay)

			results := env.relay.Admit(relay, []git.RefUpdate{update})
			if len(results) != 1 {
				t.Fatalf("Admit 返回 %d 项, want 1", len(results))
			}
			got := results[0]
			if tc.wantReject == "" {
				if got != nil {
					t.Fatalf("期望放行, 实际拒绝: %s", got.Reason)
				}
				return
			}
			if got == nil {
				t.Fatal("期望拒绝, 实际放行")
			}
			if !strings.Contains(got.Reason, tc.wantReject) {
				t.Errorf("拒绝原因 = %q, want 包含 %q", got.Reason, tc.wantReject)
			}
		})
	}
}

func TestRelayAdmit_BaseUnavailable(t *testing.T) {
	env := newRelayTestEnv(t)
	relay := env.addRelay(t, "relay-unreachable", func(m *MirrorConfig) {
		m.RemoteURL = "http://127.0.0.1:1/repo.git" // 不可达
	})
	oid := relayTestCommit(t, relay.Path(), git.ZeroOid, "a.txt", "x\n")

	results := env.relay.Admit(relay, []git.RefUpdate{{Name: "refs/heads/master", OldOid: git.ZeroOid, NewOid: oid}})
	if len(results) != 1 || results[0] == nil {
		t.Fatalf("上游不可达时必须拒绝: %+v", results)
	}
	if !strings.Contains(results[0].Reason, "base unavailable") {
		t.Errorf("reason = %q", results[0].Reason)
	}
}

func TestRelayAdmit_NonRelayIsPassThrough(t *testing.T) {
	env := newRelayTestEnv(t)
	if err := env.manager.CreateRepository("plain", "plain repo", "master"); err != nil {
		t.Fatal(err)
	}
	repo, _ := env.manager.GetRepository("plain")
	if got := env.relay.Admit(repo, []git.RefUpdate{{Name: "refs/heads/master"}}); got != nil {
		t.Fatalf("普通仓库不应有准入限制: %+v", got)
	}
}

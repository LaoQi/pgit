package pgs

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pgit/internal/pgs/git"
)

// newRelayDownstream 把中转仓库包装成 smart-http 服务（等价于 server.HTTPHandler 的
// receive-pack 路径：注入准入 gate，成功后触发转发），供测试用 git.PushRemote 充当
// 「下游开发者」推送。
func newRelayDownstream(t *testing.T, env *relayTestEnv, relay *Repository) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/relay.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		service := r.URL.Query().Get("service")
		out, err := git.ServeInfoRefs(relay.Path(), service)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/relay.git/git-receive-pack", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		opts := []git.ReceivePackOptions{{
			PreRefs: func(updates []git.RefUpdate) []*git.RefUpdateResult {
				return env.relay.Admit(relay, updates)
			},
		}}
		results, err := git.HandleReceivePack(relay.Path(), r.Body, w, opts...)
		if err != nil {
			t.Logf("relay receive-pack: %v", err)
			return
		}
		env.relay.OnRefsUpdated(relay, results)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// waitFor 轮询等待条件成立（异步转发）。
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", desc)
}

// newDevRepo 构造一个「开发者本地仓库」，其基线提交与上游一致（内容相同 → oid 相同）。
func newDevRepo(t *testing.T, base git.Oid) *Repository {
	t.Helper()
	dev, err := InitBare(t.TempDir(), "dev", "developer clone", "master")
	if err != nil {
		t.Fatalf("init dev repo: %v", err)
	}
	if !base.IsZero() {
		relayTestCommit(t, dev.Path(), git.ZeroOid, "a.txt", "base\n")
		relayTestSetRef(t, dev.Path(), "refs/heads/master", base)
	}
	return dev
}

// pushToRelay 用 git.PushRemote 模拟下游 push。
func pushToRelay(t *testing.T, ds *httptest.Server, repoRoot string, specs []git.RefSpec) *git.PushResult {
	t.Helper()
	res, err := git.PushRemote(ds.URL+"/relay.git", repoRoot, specs, nil, git.FetchOptions{MaxAttempts: 1})
	if err != nil {
		t.Fatalf("PushRemote: %v", err)
	}
	return res
}

func TestRelayForward_AcceptsAndForwards(t *testing.T) {
	env := newRelayTestEnv(t)
	relay := env.addRelay(t, "relay1", nil)

	base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
	relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
	if _, err := env.manager.SyncRepository("relay1"); err != nil {
		t.Fatalf("SyncRepository: %v", err)
	}

	dev := newDevRepo(t, base)
	next := relayTestCommit(t, dev.Path(), base, "b.txt", "next\n")
	relayTestSetRef(t, dev.Path(), "refs/heads/master", next)

	ds := newRelayDownstream(t, env, relay)
	res := pushToRelay(t, ds, dev.Path(), []git.RefSpec{{Ref: "refs/heads/master", Oid: next}})
	if !res.AllOK() {
		t.Fatalf("下游 push 被拒绝: %+v", res.Failed())
	}
	// 本地立即更新
	if got := relayTestRefOid(t, relay.Path(), "refs/heads/master"); got != next {
		t.Fatalf("relay 本地 ref = %s, want %s", got, next)
	}
	// 异步转发到上游
	waitFor(t, 5*time.Second, "上游收到转发", func() bool {
		return relayTestRefOid(t, env.upstream.Path(), "refs/heads/master") == next
	})

	// 对象完整送到上游（blob 内容逐字节一致）
	store := git.NewObjectStore(env.upstream.Path())
	if _, _, err := store.Stat(next); err != nil {
		t.Errorf("上游缺少提交对象 %s: %v", next, err)
	}

	// pending 清空、日志与状态就绪
	waitFor(t, 5*time.Second, "pending 清空", func() bool {
		r, err := env.manager.GetRepository("relay1")
		return err == nil && len(r.Mirror.PendingRefs) == 0
	})
	repo, _ := env.manager.GetRepository("relay1")
	if repo.Mirror.LastPush.IsZero() || repo.Mirror.LastPushError != "" {
		t.Errorf("LastPush=%v LastPushError=%q", repo.Mirror.LastPush, repo.Mirror.LastPushError)
	}
	entries, err := ReadRelayLog(repo.Path(), 10)
	if err != nil || len(entries) == 0 {
		t.Fatalf("relay 日志缺失: err=%v entries=%d", err, len(entries))
	}
	if !entries[0].Success || entries[0].RefsPushed != 1 {
		t.Errorf("relay 日志 = %+v", entries[0])
	}

	st, err := env.relay.Status("relay1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.PendingRefs) != 0 || st.Pushing || st.Queued {
		t.Errorf("status = %+v", st)
	}
	if st.BaseAt.IsZero() || len(st.Differ) != 0 {
		t.Errorf("status base/differ = %+v", st)
	}
}

func TestRelayForward_RejectsStaleBaseEndToEnd(t *testing.T) {
	env := newRelayTestEnv(t)
	relay := env.addRelay(t, "relay1", nil)

	base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
	relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
	if _, err := env.manager.SyncRepository("relay1"); err != nil {
		t.Fatalf("SyncRepository: %v", err)
	}
	// 上游被别人推进（relay 本地尚未拉取）
	other := relayTestCommit(t, env.upstream.Path(), base, "c.txt", "other\n")
	relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", other)

	dev := newDevRepo(t, base)
	next := relayTestCommit(t, dev.Path(), base, "b.txt", "next\n")
	relayTestSetRef(t, dev.Path(), "refs/heads/master", next)

	ds := newRelayDownstream(t, env, relay)
	res := pushToRelay(t, ds, dev.Path(), []git.RefSpec{{Ref: "refs/heads/master", Oid: next}})
	if res.AllOK() {
		t.Fatal("基于过期基线的 push 必须被拒绝")
	}
	failed := res.Failed()
	if len(failed) != 1 || failed[0].Reason == "" {
		t.Fatalf("failed = %+v", failed)
	}
	// 本地 ref 未被污染，上游也确实没变
	if got := relayTestRefOid(t, relay.Path(), "refs/heads/master"); got != base {
		t.Errorf("relay 本地 ref = %s, want %s（拒绝的 push 不得改本地）", got, base)
	}
	if got := relayTestRefOid(t, env.upstream.Path(), "refs/heads/master"); got != other {
		t.Errorf("上游 ref = %s, want %s", got, other)
	}
}

func TestRelayForward_DeleteRef(t *testing.T) {
	env := newRelayTestEnv(t)
	relay := env.addRelay(t, "relay1", nil)

	base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
	relayTestSetRef(t, env.upstream.Path(), "refs/heads/topic", base)
	if _, err := env.manager.SyncRepository("relay1"); err != nil {
		t.Fatalf("SyncRepository: %v", err)
	}

	dev := newDevRepo(t, base)
	ds := newRelayDownstream(t, env, relay)
	res := pushToRelay(t, ds, dev.Path(), []git.RefSpec{{Ref: "refs/heads/topic", Oid: git.ZeroOid}})
	if !res.AllOK() {
		t.Fatalf("删除 push 被拒绝: %+v", res.Failed())
	}
	waitFor(t, 5*time.Second, "上游 ref 被删除", func() bool {
		_, err := git.NewRefStore(env.upstream.Path()).Get("refs/heads/topic")
		return err != nil
	})
}

func TestRelayForward_NewTag(t *testing.T) {
	env := newRelayTestEnv(t)
	relay := env.addRelay(t, "relay1", nil)

	base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
	relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
	if _, err := env.manager.SyncRepository("relay1"); err != nil {
		t.Fatalf("SyncRepository: %v", err)
	}

	dev := newDevRepo(t, base)
	tag := relayTestObject(t, dev.Path(), git.ObjTag,
		[]byte("object "+base.String()+"\ntype commit\ntag v1\ntagger T <t@pgit.dev> 1700000000 +0800\n\nrelease\n"))
	relayTestSetRef(t, dev.Path(), "refs/tags/v1", tag)

	ds := newRelayDownstream(t, env, relay)
	res := pushToRelay(t, ds, dev.Path(), []git.RefSpec{{Ref: "refs/tags/v1", Oid: tag}})
	if !res.AllOK() {
		t.Fatalf("tag push 被拒绝: %+v", res.Failed())
	}
	waitFor(t, 5*time.Second, "上游收到 tag", func() bool {
		oid, err := git.NewRefStore(env.upstream.Path()).Get("refs/tags/v1")
		return err == nil && oid == tag
	})
}

func TestRelayForward_UpstreamFailureKeepsPendingThenManualRetry(t *testing.T) {
	env := newRelayTestEnv(t)
	relay := env.addRelay(t, "relay1", nil)

	base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
	relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
	if _, err := env.manager.SyncRepository("relay1"); err != nil {
		t.Fatalf("SyncRepository: %v", err)
	}

	dev := newDevRepo(t, base)
	next := relayTestCommit(t, dev.Path(), base, "b.txt", "next\n")
	relayTestSetRef(t, dev.Path(), "refs/heads/master", next)

	env.server.posting = true // 上游推送阶段故障
	ds := newRelayDownstream(t, env, relay)
	res := pushToRelay(t, ds, dev.Path(), []git.RefSpec{{Ref: "refs/heads/master", Oid: next}})
	if !res.AllOK() {
		t.Fatalf("准入本身应通过（只有转发失败）: %+v", res.Failed())
	}

	waitFor(t, 5*time.Second, "pending 保留 + 错误记录", func() bool {
		r, err := env.manager.GetRepository("relay1")
		return err == nil && len(r.Mirror.PendingRefs) == 1 && r.Mirror.LastPushError != ""
	})
	// 本地领先上游：此时后续 push 会被准入拒绝（stale base），符合设计
	env.server.posting = false
	entry, err := env.relay.RelayNow("relay1")
	if err != nil {
		t.Fatalf("RelayNow: %v", err)
	}
	if entry == nil || !entry.Success {
		t.Fatalf("手动转发未成功: %+v", entry)
	}
	r, _ := env.manager.GetRepository("relay1")
	if len(r.Mirror.PendingRefs) != 0 || r.Mirror.LastPushError != "" {
		t.Errorf("重试成功后 pending/error 应清空: %+v", r.Mirror)
	}
	if got := relayTestRefOid(t, env.upstream.Path(), "refs/heads/master"); got != next {
		t.Errorf("上游 ref = %s, want %s", got, next)
	}
}

func TestRelayForward_UpstreamRefusalDropsPending(t *testing.T) {
	env := newRelayTestEnv(t)
	relay := env.addRelay(t, "relay1", nil)

	base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
	relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
	if _, err := env.manager.SyncRepository("relay1"); err != nil {
		t.Fatalf("SyncRepository: %v", err)
	}

	dev := newDevRepo(t, base)
	next := relayTestCommit(t, dev.Path(), base, "b.txt", "next\n")
	relayTestSetRef(t, dev.Path(), "refs/heads/master", next)

	// 准入通过后上游被改动 → 转发的 CAS 失败（ng），属于永久拒绝：不再无限重试
	other := relayTestCommit(t, env.upstream.Path(), base, "c.txt", "other\n")
	env.server.beforePost = func() {
		relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", other)
	}

	ds := newRelayDownstream(t, env, relay)
	if res := pushToRelay(t, ds, dev.Path(), []git.RefSpec{{Ref: "refs/heads/master", Oid: next}}); !res.AllOK() {
		t.Fatalf("准入应通过: %+v", res.Failed())
	}

	waitFor(t, 5*time.Second, "上游拒绝后 pending 被丢弃", func() bool {
		r, err := env.manager.GetRepository("relay1")
		return err == nil && len(r.Mirror.PendingRefs) == 0 && r.Mirror.LastPushError != ""
	})
	r, _ := env.manager.GetRepository("relay1")
	if !contains(r.Mirror.LastPushError, "rejected") {
		t.Errorf("LastPushError = %q", r.Mirror.LastPushError)
	}
	if got := relayTestRefOid(t, env.upstream.Path(), "refs/heads/master"); got != other {
		t.Errorf("上游应保持被他人推进的值 %s, got %s", other, got)
	}
}

func TestRelayNow_ReconcilesLocalAhead(t *testing.T) {
	env := newRelayTestEnv(t)
	relay := env.addRelay(t, "relay1", nil)

	base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
	relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
	if _, err := env.manager.SyncRepository("relay1"); err != nil {
		t.Fatalf("SyncRepository: %v", err)
	}

	// 本地领先但没有 pending 记录（例如重启后元数据丢失）：手动转发应能补齐
	next := relayTestCommit(t, relay.Path(), base, "b.txt", "next\n")
	relayTestSetRef(t, relay.Path(), "refs/heads/master", next)

	entry, err := env.relay.RelayNow("relay1")
	if err != nil {
		t.Fatalf("RelayNow: %v", err)
	}
	if entry == nil || !entry.Success {
		t.Fatalf("RelayNow entry = %+v", entry)
	}
	if got := relayTestRefOid(t, env.upstream.Path(), "refs/heads/master"); got != next {
		t.Errorf("上游 ref = %s, want %s", got, next)
	}
}

func TestRelayBootstrap_ResumesPending(t *testing.T) {
	env := newRelayTestEnv(t)
	relay := env.addRelay(t, "relay1", nil)

	base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
	relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
	if _, err := env.manager.SyncRepository("relay1"); err != nil {
		t.Fatalf("SyncRepository: %v", err)
	}
	next := relayTestCommit(t, relay.Path(), base, "b.txt", "next\n")
	relayTestSetRef(t, relay.Path(), "refs/heads/master", next)
	// 模拟上次运行留下未转发的 pending（已落盘）
	if err := env.manager.UpdateMirrorRuntime("relay1", func(m *MirrorConfig) {
		m.PendingRefs = []string{"refs/heads/master"}
	}); err != nil {
		t.Fatalf("UpdateMirrorRuntime: %v", err)
	}

	r, _ := env.manager.GetRepository("relay1")
	env.relay.Bootstrap(r)

	waitFor(t, 5*time.Second, "启动补推完成", func() bool {
		oid, err := git.NewRefStore(env.upstream.Path()).Get("refs/heads/master")
		return err == nil && oid == next
	})
	repo, _ := env.manager.GetRepository("relay1")
	if len(repo.Mirror.PendingRefs) != 0 {
		t.Errorf("pending 未清空: %v", repo.Mirror.PendingRefs)
	}
}

func TestRelayPull_ProtectsPendingRefs(t *testing.T) {
	env := newRelayTestEnv(t)
	relay := env.addRelay(t, "relay1", nil)

	base := relayTestCommit(t, env.upstream.Path(), git.ZeroOid, "a.txt", "base\n")
	relayTestSetRef(t, env.upstream.Path(), "refs/heads/master", base)
	if _, err := env.manager.SyncRepository("relay1"); err != nil {
		t.Fatalf("SyncRepository: %v", err)
	}

	next := relayTestCommit(t, relay.Path(), base, "b.txt", "next\n")
	relayTestSetRef(t, relay.Path(), "refs/heads/master", next)
	if err := env.manager.UpdateMirrorRuntime("relay1", func(m *MirrorConfig) {
		m.PendingRefs = []string{"refs/heads/master"}
	}); err != nil {
		t.Fatalf("UpdateMirrorRuntime: %v", err)
	}
	// 上游在另一个分支上前进
	upstreamNext := relayTestCommit(t, env.upstream.Path(), base, "up.txt", "up\n")
	relayTestSetRef(t, env.upstream.Path(), "refs/heads/upstream-work", upstreamNext)

	if _, err := env.manager.SyncRepository("relay1"); err != nil {
		t.Fatalf("SyncRepository with pending: %v", err)
	}
	// pending 的 master 保持本地领先（未被回退）
	if got := relayTestRefOid(t, relay.Path(), "refs/heads/master"); got != next {
		t.Errorf("pending ref 被 pull 回退: %s, want %s", got, next)
	}
	// 其他 ref 正常镜像
	if got := relayTestRefOid(t, relay.Path(), "refs/heads/upstream-work"); got != upstreamNext {
		t.Errorf("其他 ref 未被拉取: %s, want %s", got, upstreamNext)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

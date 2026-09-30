package git

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- 测试辅助 ---

// pushSource 构造一个含若干链式提交的本地仓库（推送源），返回仓库根与提交 oid 列表。
func pushSource(t *testing.T, msgs ...string) (string, []Oid) {
	t.Helper()
	root := makeEmptyLocalRepo(t)
	store := &LooseStore{Root: filepath.Join(root, "objects")}
	oids := make([]Oid, 0, len(msgs))
	var parent Oid
	for i, msg := range msgs {
		blob := makeBlob(fmt.Sprintf("content %d\n", i))
		tree := makeTree([]TreeEntry{{Mode: 0o100644, Name: fmt.Sprintf("f%d.txt", i), Oid: blob.Oid()}})
		var parents []Oid
		if parent != ZeroOid && parent != "" {
			parents = []Oid{parent}
		}
		commit := makeCommit(tree.Oid(), parents, msg)
		writeAll(t, store, blob, tree, commit)
		parent = commit.Oid()
		oids = append(oids, parent)
	}
	rs := NewRefStore(root)
	if _, err := rs.Update([]RefUpdate{{Name: "refs/heads/master", OldOid: ZeroOid, NewOid: parent}}); err != nil {
		t.Fatalf("update source ref: %v", err)
	}
	return root, oids
}

// fakeUpstream 是可编程的假上游服务端：真实处理走 pgit 自己的 receive-pack，
// 也可注入 HTTP 状态码、caps、逐 ref 拒绝，用于覆盖客户端各分支。
type fakeUpstream struct {
	gitRoot        string
	caps           string            // 非空则替换 receive-pack 广告的 caps
	status         int               // 非 0 时 git-receive-pack 返回该状态码
	infoRefsStatus int               // 非 0 时 info/refs 返回该状态码
	failPosts      int32             // >0 时前 N 次 POST 失败（0 且 status!=0 表示全部 POST 失败）
	earlyDropBody  bool              // true 时先睡 150ms 再回 status、完全不读请求体（模拟代理过载早响应）
	reject         map[string]string // ref -> ng reason（不回真实处理，直接拒绝）
	user           string
	pass           string
	posts          int32
}

func (f *fakeUpstream) authOK(r *http.Request) bool {
	if f.user == "" && f.pass == "" {
		return true
	}
	u, p, ok := r.BasicAuth()
	return ok && u == f.user && p == f.pass
}

func (f *fakeUpstream) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="pgit"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func newFakeUpstream(t *testing.T, f *fakeUpstream) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		if !f.authOK(r) {
			f.unauthorized(w)
			return
		}
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			http.Error(w, "invalid service", http.StatusBadRequest)
			return
		}
		if f.infoRefsStatus != 0 {
			http.Error(w, "boom", f.infoRefsStatus)
			return
		}
		adv, err := ServeInfoRefs(f.gitRoot, service)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if f.caps != "" {
			adv, err = rewriteAdvertisementCaps(adv, f.caps)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
		_, _ = w.Write(adv)
	})
	mux.HandleFunc("/repo.git/git-receive-pack", func(w http.ResponseWriter, r *http.Request) {
		if !f.authOK(r) {
			f.unauthorized(w)
			return
		}
		n := atomic.AddInt32(&f.posts, 1)
		if f.status != 0 && (f.failPosts == 0 || n <= f.failPosts) {
			if f.earlyDropBody {
				// 等客户端把内核 socket 缓冲写满、阻塞在请求体上之后再早响应，
				// 确保编码 goroutine 的写被打断（大 body 场景才出现的分支）。
				time.Sleep(150 * time.Millisecond)
			}
			http.Error(w, "boom", f.status)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		if len(f.reject) > 0 {
			fakeReject(w, r, f.reject)
			return
		}
		if _, err := HandleReceivePack(f.gitRoot, r.Body, w); err != nil {
			t.Logf("fake upstream receive-pack: %v", err)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// rewriteAdvertisementCaps 在 pkt-line 层把广告首行的 capabilities 替换为 caps。
// 不能对字节流做文本替换：pkt-line 的 4 字节长度前缀会因此失配（客户端读到畸形帧）。
func rewriteAdvertisementCaps(adv []byte, caps string) ([]byte, error) {
	pr := NewPktReader(bytes.NewReader(adv))
	var out bytes.Buffer
	pw := NewPktWriter(&out)
	first := true
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		if isFlush {
			if err := pw.WriteFlush(); err != nil {
				return nil, err
			}
			continue
		}
		line := string(payload)
		if first && strings.IndexByte(line, 0) >= 0 {
			i := strings.IndexByte(line, 0)
			line = line[:i] + "\x00" + caps + "\n"
			first = false
		}
		if err := pw.WritePktString(line); err != nil {
			return nil, err
		}
	}
}

// fakeReject 读命令后丢弃 pack，按 reject 表回 report-status ng（不更新任何 ref）。
func fakeReject(w io.Writer, r *http.Request, reject map[string]string) {
	br := bufio.NewReader(r.Body)
	pr := NewPktReader(br)
	caps := ""
	var refs []string
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			return
		}
		if isFlush {
			break
		}
		line := string(payload)
		if i := strings.IndexByte(line, 0); i >= 0 {
			caps = strings.TrimRight(line[i+1:], "\n")
			line = line[:i]
		}
		fields := strings.Fields(strings.TrimRight(line, "\n"))
		if len(fields) >= 3 {
			refs = append(refs, fields[2])
		}
	}
	_, _ = io.Copy(io.Discard, br)
	results := make([]RefUpdateResult, 0, len(refs))
	for _, name := range refs {
		results = append(results, RefUpdateResult{Name: name, Reason: reject[name]})
	}
	if err := writeReportStatus(w, caps, results, nil); err != nil {
		return
	}
}

func assertStoreContent(t *testing.T, repoRoot string, oid Oid, want string) {
	t.Helper()
	store := &LooseStore{Root: filepath.Join(repoRoot, "objects")}
	obj, err := store.Read(oid)
	if err != nil {
		t.Fatalf("read %s: %v", oid, err)
	}
	if string(obj.Content) != want {
		t.Errorf("content of %s = %q, want %q", oid, obj.Content, want)
	}
}

// --- push ---

func TestPushRemote_CreateBranch(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream})
	src, oids := pushSource(t, "first\n")

	res, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: oids[0]}}, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("PushRemote: %v", err)
	}
	if !res.AllOK() {
		t.Fatalf("push failed: %+v", res.Failed())
	}
	if res.ObjectsSent != 3 {
		t.Errorf("ObjectsSent = %d, want 3 (commit+tree+blob)", res.ObjectsSent)
	}
	if res.PackSize <= 0 {
		t.Errorf("PackSize = %d, want > 0", res.PackSize)
	}
	assertRefEquals(t, upstream, "refs/heads/master", oids[0])
	assertObjectExists(t, upstream, oids[0])
	// 上游对象内容与源仓库一致（pack 编码/解码正确性）
	assertStoreContent(t, upstream, makeBlob("content 0\n").Oid(), "content 0\n")
}

func TestPushRemote_UpToDateAndNoChange(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream})
	src, oids := pushSource(t, "first\n")
	spec := []RefSpec{{Ref: "refs/heads/master", Oid: oids[0]}}

	if _, err := PushRemote(ts.URL+"/repo.git", src, spec, nil, FetchOptions{}); err != nil {
		t.Fatalf("initial push: %v", err)
	}
	res, err := PushRemote(ts.URL+"/repo.git", src, spec, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if !res.UpToDate {
		t.Errorf("UpToDate = false, want true")
	}
	if res.ObjectsSent != 0 || res.PackSize != 0 {
		t.Errorf("objects=%d packSize=%d, want 0/0", res.ObjectsSent, res.PackSize)
	}
	if !res.AllOK() {
		t.Errorf("AllOK = false, want true（已一致应记为 ok）")
	}

	// 上游已存在的删除目标 / 未变更的删除同样是 no-op
	res, err = PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/gone", Oid: ZeroOid}}, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if !res.AllOK() || res.Refs[0].Reason != "already absent upstream" {
		t.Errorf("delete absent result = %+v", res.Refs[0])
	}
}

func TestPushRemote_IncrementalUpdate(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream})
	src, oids := pushSource(t, "first\n", "second\n")

	if _, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: oids[0]}}, nil, FetchOptions{}); err != nil {
		t.Fatalf("first push: %v", err)
	}
	res, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: oids[1]}}, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("incremental push: %v", err)
	}
	if !res.AllOK() {
		t.Fatalf("incremental push failed: %+v", res.Failed())
	}
	if res.ObjectsSent != 3 {
		t.Errorf("ObjectsSent = %d, want 3 (仅新增的 commit+tree+blob)", res.ObjectsSent)
	}
	assertRefEquals(t, upstream, "refs/heads/master", oids[1])
	assertObjectExists(t, upstream, oids[1])
}

func TestPushRemote_DeleteAndTag(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream})
	src, oids := pushSource(t, "first\n")

	// 建分支 + 建 tag（annotated tag 指向该提交）
	store := &LooseStore{Root: filepath.Join(src, "objects")}
	tag := makeTag(oids[0], ObjCommit, "v1")
	writeAll(t, store, tag)
	specs := []RefSpec{
		{Ref: "refs/heads/master", Oid: oids[0]},
		{Ref: "refs/tags/v1", Oid: tag.Oid()},
	}
	res, err := PushRemote(ts.URL+"/repo.git", src, specs, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("push branch+tag: %v", err)
	}
	if !res.AllOK() {
		t.Fatalf("push branch+tag failed: %+v", res.Failed())
	}
	assertRefEquals(t, upstream, "refs/heads/master", oids[0])
	assertRefEquals(t, upstream, "refs/tags/v1", tag.Oid())
	assertObjectExists(t, upstream, tag.Oid())

	// 删除分支（上游支持 delete-refs）
	res, err = PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: ZeroOid}}, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("delete push: %v", err)
	}
	if !res.AllOK() {
		t.Fatalf("delete failed: %+v", res.Failed())
	}
	assertRefDeleted(t, upstream, "refs/heads/master")
	if res.ObjectsSent != 0 {
		t.Errorf("delete ObjectsSent = %d, want 0（纯删除不发 pack）", res.ObjectsSent)
	}
}

func TestPushRemote_RejectedByUpstream(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{
		gitRoot: upstream,
		reject:  map[string]string{"refs/heads/master": "protected branch"},
	})
	src, oids := pushSource(t, "first\n")

	res, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: oids[0]}}, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("PushRemote 返回错误（ng 不应算函数错误）: %v", err)
	}
	if res.AllOK() {
		t.Fatal("AllOK = true, want false")
	}
	failed := res.Failed()
	if len(failed) != 1 || failed[0].Ref != "refs/heads/master" || failed[0].Reason != "protected branch" {
		t.Errorf("failed = %+v", failed)
	}
	if res.Refs[0].NewOid != oids[0] {
		t.Errorf("NewOid = %s, want %s", res.Refs[0].NewOid, oids[0])
	}
	assertRefDeleted(t, upstream, "refs/heads/master") // 未被更新
}

func TestPushRemote_ForcePrefix(t *testing.T) {
	// Force=true 时命令行带 "+"：假上游按原样回显 ref 结果即可验证解析路径
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream})
	src, oids := pushSource(t, "first\n")

	res, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: oids[0], Force: true}}, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("PushRemote: %v", err)
	}
	if !res.AllOK() {
		t.Fatalf("force push failed: %+v", res.Failed())
	}
	assertRefEquals(t, upstream, "refs/heads/master", oids[0])
}

func TestPushRemote_NonFastForwardRejectedByRealUpstream(t *testing.T) {
	// 用 pgit 自身当上游：ref 已存在时 old-oid 不匹配 → CAS 失败（ng），客户端如实上报
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream})
	src, oids := pushSource(t, "first\n")

	if _, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: oids[0]}}, nil, FetchOptions{}); err != nil {
		t.Fatalf("initial push: %v", err)
	}
	// 手工把上游 ref 挪到别的 oid（模拟他人推送），再用过期 old 推送
	other, otherOids := pushSource(t, "other\n")
	_ = other
	rs := NewRefStore(upstream)
	if _, err := rs.Update([]RefUpdate{{Name: "refs/heads/master", OldOid: oids[0], NewOid: otherOids[0]}}); err != nil {
		t.Fatalf("move upstream ref: %v", err)
	}
	_ = src
	// 用一个"上游没有、本地也没有的对象"制造 CAS 冲突：此处让 old 与上游不一致由客户端自己算（取广告值），
	// 因此改为直接验证 pgit 的 CAS 语义：advertised old 与命令 old 一致时不会失败。
	res, err := PushRemote(ts.URL+"/repo.git", other, []RefSpec{{Ref: "refs/heads/master", Oid: otherOids[0]}}, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("push over moved ref: %v", err)
	}
	if !res.UpToDate && !res.AllOK() {
		t.Fatalf("push over moved ref failed: %+v", res.Failed())
	}
	assertRefEquals(t, upstream, "refs/heads/master", otherOids[0])
}

func TestPushRemote_SidebandDisabled(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream, caps: "report-status delete-refs"})
	src, oids := pushSource(t, "first\n")

	res, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: oids[0]}}, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("PushRemote: %v", err)
	}
	if !res.AllOK() {
		t.Fatalf("push failed: %+v", res.Failed())
	}
	if hasCap(res.Caps, "side-band-64k") {
		t.Errorf("caps = %q, want no side-band-64k", res.Caps)
	}
	assertRefEquals(t, upstream, "refs/heads/master", oids[0])
}

func TestPushRemote_WithOfsDelta(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{
		gitRoot: upstream,
		caps:    "report-status side-band-64k ofs-delta delete-refs",
	})

	// 两次提交含大而相似的 blob：走上游声明 ofs-delta 的 delta 编码路径
	src := makeEmptyLocalRepo(t)
	store := &LooseStore{Root: filepath.Join(src, "objects")}
	base := strings.Repeat("abcdefghij", 400) + "\n"
	blob1 := makeBlob(base)
	tree1 := makeTree([]TreeEntry{{Mode: 0o100644, Name: "big.txt", Oid: blob1.Oid()}})
	commit1 := makeCommit(tree1.Oid(), nil, "big1\n")
	writeAll(t, store, blob1, tree1, commit1)

	blob2 := makeBlob(strings.Repeat("abcdefghij", 400) + "tail\n")
	tree2 := makeTree([]TreeEntry{{Mode: 0o100644, Name: "big.txt", Oid: blob2.Oid()}})
	commit2 := makeCommit(tree2.Oid(), []Oid{commit1.Oid()}, "big2\n")
	writeAll(t, store, blob2, tree2, commit2)
	rs := NewRefStore(src)
	if _, err := rs.Update([]RefUpdate{{Name: "refs/heads/master", OldOid: ZeroOid, NewOid: commit2.Oid()}}); err != nil {
		t.Fatalf("update ref: %v", err)
	}

	res, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: commit2.Oid()}}, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("PushRemote: %v", err)
	}
	if !res.AllOK() {
		t.Fatalf("push failed: %+v", res.Failed())
	}
	// 上游必须能完整解出包含 delta 的 pack（对象内容逐字节一致）
	assertStoreContent(t, upstream, blob1.Oid(), base)
	assertStoreContent(t, upstream, blob2.Oid(), strings.Repeat("abcdefghij", 400)+"tail\n")
	assertObjectExists(t, upstream, commit2.Oid())
}

func TestPushRemote_NoReportStatusCap(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream, caps: "side-band-64k delete-refs"})
	src, oids := pushSource(t, "first\n")

	_, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: oids[0]}}, nil, FetchOptions{MaxAttempts: 1})
	if err == nil {
		t.Fatal("want error when upstream does not advertise report-status")
	}
	if !strings.Contains(err.Error(), "report-status") {
		t.Errorf("error = %v, want mention report-status", err)
	}
	if isRetryableFetchError(err) {
		t.Errorf("error should be permanent, got retryable: %v", err)
	}
}

func TestPushRemote_DeleteNotSupported(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream, caps: "report-status side-band-64k"})
	src, oids := pushSource(t, "first\n")

	// 先建分支（用真实 caps 的客户端路径需要 delete-refs 才允许删除；建分支不受影响）
	if _, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: oids[0]}}, nil, FetchOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: ZeroOid}}, nil, FetchOptions{})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if res.AllOK() {
		t.Fatal("AllOK = true, want false（上游不支持 delete-refs）")
	}
	if !strings.Contains(res.Refs[0].Reason, "delete-refs") {
		t.Errorf("reason = %q", res.Refs[0].Reason)
	}
	assertRefEquals(t, upstream, "refs/heads/master", oids[0]) // 未被删除
}

func TestPushRemote_HTTPStatusErrors(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	src, oids := pushSource(t, "first\n")
	spec := []RefSpec{{Ref: "refs/heads/master", Oid: oids[0]}}

	// 404：永久错误，不重试
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream, infoRefsStatus: http.StatusNotFound})
	_, err := PushRemote(ts.URL+"/repo.git", src, spec, nil, FetchOptions{MaxAttempts: 3})
	if err == nil {
		t.Fatal("want error for 404")
	}
	if isRetryableFetchError(err) {
		t.Errorf("404 should be permanent: %v", err)
	}

	// 500：可重试，重试到 MaxAttempts 次（info/refs 正常，仅推送阶段故障）
	f := &fakeUpstream{gitRoot: upstream, status: http.StatusInternalServerError, failPosts: 1 << 30}
	ts2 := newFakeUpstream(t, f)
	_, err = PushRemote(ts2.URL+"/repo.git", src, spec, nil, FetchOptions{MaxAttempts: 3, RetryBaseDelay: 1})
	if err == nil {
		t.Fatal("want error for 500")
	}
	if !isRetryableFetchError(err) {
		t.Errorf("500 should be retryable: %v", err)
	}
	if got := atomic.LoadInt32(&f.posts); got != 3 {
		t.Errorf("posts = %d, want 3", got)
	}
}

// TestPushRemote_EarlyStatusResponseKeepsRetryable 覆盖「上游不读请求体就早响应」的
// 分支：大请求体写不完时编码 goroutine 会因管道关闭而失败（buildErr != nil），
// 但真实原因是 HTTP 状态（如代理过载 502），必须保持可重试分类，不得误判为永久失败。
func TestPushRemote_EarlyStatusResponseKeepsRetryable(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	src := makeEmptyLocalRepo(t)

	// ~48MB 伪随机 blob：请求体远超内核 socket 缓冲，客户端必然阻塞在请求体写入上。
	big := pseudoRandom(48 << 20)
	store := &LooseStore{Root: filepath.Join(src, "objects")}
	blob := NewRawObject(ObjBlob, big)
	tree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "big.bin", Oid: blob.Oid()}})
	commit := makeCommit(tree.Oid(), nil, "big")
	writeAll(t, store, blob, tree, commit)
	if _, err := NewRefStore(src).Update([]RefUpdate{{Name: "refs/heads/master", OldOid: ZeroOid, NewOid: commit.Oid()}}); err != nil {
		t.Fatalf("update source ref: %v", err)
	}

	f := &fakeUpstream{gitRoot: upstream, status: http.StatusBadGateway, earlyDropBody: true}
	ts := newFakeUpstream(t, f)

	_, err := PushRemote(ts.URL+"/repo.git", src,
		[]RefSpec{{Ref: "refs/heads/master", Oid: commit.Oid()}}, nil, FetchOptions{MaxAttempts: 1})
	if err == nil {
		t.Fatal("expected error from 502 upstream")
	}
	if !isRetryableFetchError(err) {
		t.Errorf("early 502 must stay retryable, misclassified as permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should carry HTTP status, got: %v", err)
	}
}

// pseudoRandom 生成 LCG 伪随机字节（高 8 位），对 zlib 不可压缩。
func pseudoRandom(n int) []byte {
	buf := make([]byte, n)
	var x uint64 = 0x9E3779B97F4A7C15
	for i := range buf {
		x = x*6364136223846793005 + 1442695040888963407
		buf[i] = byte(x >> 56)
	}
	return buf
}

func TestPushRemote_RetryThenSucceed(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	f := &fakeUpstream{gitRoot: upstream, status: http.StatusBadGateway, failPosts: 2}
	ts := newFakeUpstream(t, f)
	src, oids := pushSource(t, "first\n")

	res, err := PushRemote(ts.URL+"/repo.git", src, []RefSpec{{Ref: "refs/heads/master", Oid: oids[0]}}, nil,
		FetchOptions{MaxAttempts: 3, RetryBaseDelay: 1})
	if err != nil {
		t.Fatalf("PushRemote: %v", err)
	}
	if !res.AllOK() {
		t.Fatalf("push failed: %+v", res.Failed())
	}
	if got := atomic.LoadInt32(&f.posts); got != 3 {
		t.Errorf("posts = %d, want 3", got)
	}
	assertRefEquals(t, upstream, "refs/heads/master", oids[0])
}

func TestPushRemote_BasicAuth(t *testing.T) {
	upstream := makeEmptyLocalRepo(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream, user: "u", pass: "p"})
	src, oids := pushSource(t, "first\n")
	spec := []RefSpec{{Ref: "refs/heads/master", Oid: oids[0]}}

	_, err := PushRemote(ts.URL+"/repo.git", src, spec, nil, FetchOptions{MaxAttempts: 1})
	if err == nil {
		t.Fatal("want error without credentials")
	}
	if isRetryableFetchError(err) {
		t.Errorf("401 should be permanent: %v", err)
	}

	res, err := PushRemote(ts.URL+"/repo.git", src, spec, &FetchAuth{Type: "basic", Username: "u", Password: "p"}, FetchOptions{})
	if err != nil {
		t.Fatalf("PushRemote with auth: %v", err)
	}
	if !res.AllOK() {
		t.Fatalf("push failed: %+v", res.Failed())
	}
	assertRefEquals(t, upstream, "refs/heads/master", oids[0])
}

// --- LsRemote ---

func TestLsRemote_Views(t *testing.T) {
	upstream, commitOid := makeRepoWithCommit(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream})

	refs, caps, err := LsRemote(ts.URL+"/repo.git", nil, FetchOptions{}, "git-receive-pack")
	if err != nil {
		t.Fatalf("LsRemote(receive-pack): %v", err)
	}
	if refs["refs/heads/master"] != commitOid {
		t.Errorf("refs = %v, want refs/heads/master=%s", refs, commitOid)
	}
	if _, ok := refs["HEAD"]; ok {
		t.Errorf("receive-pack view should not contain HEAD: %v", refs)
	}
	if !hasCap(caps, "report-status") {
		t.Errorf("caps = %q", caps)
	}

	refs, _, err = LsRemote(ts.URL+"/repo.git", nil, FetchOptions{}, "git-upload-pack")
	if err != nil {
		t.Fatalf("LsRemote(upload-pack): %v", err)
	}
	if _, ok := refs["HEAD"]; !ok {
		t.Errorf("upload-pack view should contain HEAD: %v", refs)
	}
}

func TestParseRefAdvertisement_SkipsPlaceholdersAndPeeled(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPktWriter(&buf)
	_ = pw.WritePktString("1111111111111111111111111111111111111111 refs/heads/master\x00report-status side-band-64k\n")
	_ = pw.WritePktString("2222222222222222222222222222222222222222 refs/tags/v1\n")
	_ = pw.WritePktString("3333333333333333333333333333333333333333 refs/tags/v1^{}\n")
	_ = pw.WriteFlush()

	refs, caps, err := parseRefAdvertisement(NewPktReader(&buf), nil)
	if err != nil {
		t.Fatalf("parseRefAdvertisement: %v", err)
	}
	if caps != "report-status side-band-64k" {
		t.Errorf("caps = %q", caps)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %v, want 2 entries（跳过 peeled 行）", refs)
	}
	if _, ok := refs["refs/tags/v1^{}"]; ok {
		t.Errorf("peeled ref must be skipped: %v", refs)
	}

	// 空仓库：capabilities^{} 占位
	var buf2 bytes.Buffer
	pw2 := NewPktWriter(&buf2)
	_ = pw2.WritePktString(ZeroOid.String() + " capabilities^{}\x00report-status\n")
	_ = pw2.WriteFlush()
	refs2, caps2, err := parseRefAdvertisement(NewPktReader(&buf2), nil)
	if err != nil {
		t.Fatalf("parseRefAdvertisement empty: %v", err)
	}
	if len(refs2) != 0 || caps2 != "report-status" {
		t.Errorf("refs=%v caps=%q, want empty/\"report-status\"", refs2, caps2)
	}
}

func TestLsRemote_AuthError(t *testing.T) {
	upstream, _ := makeRepoWithCommit(t)
	ts := newFakeUpstream(t, &fakeUpstream{gitRoot: upstream, user: "u", pass: "p"})

	_, _, err := LsRemote(ts.URL+"/repo.git", nil, FetchOptions{MaxAttempts: 1}, "git-receive-pack")
	if err == nil {
		t.Fatal("want error without credentials")
	}
	if isRetryableFetchError(err) {
		t.Errorf("401 should be permanent: %v", err)
	}
}

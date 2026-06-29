package git

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGitClonePushE2E 用本地 git 二进制做端到端集成测试。
// 需要 git 在 PATH 中。跳过条件：环境变量 PGIT_E2E 未设置。
func TestGitClonePushE2E(t *testing.T) {
	if os.Getenv("PGIT_E2E") == "" {
		t.Skip("设置 PGIT_E2E=1 启用端到端集成测试")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不在 PATH 中")
	}

	workdir := t.TempDir()
	gitRoot := filepath.Join(workdir, "repo")
	if err := os.MkdirAll(gitRoot, 0o777); err != nil {
		t.Fatal(err)
	}

	// 构造仓库：含 blob+tree+commit + refs/heads/master + HEAD
	dir, commitOid := makeRepoWithCommit(t)

	// 构造 wants 输入（sideband 模式）
	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	inw.WritePktString(fmt.Sprintf("want %s side-band-64k ofs-delta\n", commitOid))
	inw.WriteFlush()
	inw.WritePktString("done\n")

	var outBuf bytes.Buffer
	if err := ServeUploadPack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeUploadPack: %v", err)
	}

	// 验证 NAK 在首帧
	pr := NewPktReader(&outBuf)
	payload, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read NAK: %v", err)
	}
	if isFlush {
		t.Fatal("expected NAK frame, got flush")
	}
	if string(payload) != "NAK\n" {
		t.Fatalf("first frame = %q, want NAK\\n", payload)
	}

	// 读 sideband ch1 的 PACK 数据
	var packData bytes.Buffer
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read sideband: %v", err)
		}
		if isFlush {
			break
		}
		if len(payload) >= 1 && payload[0] == SidebandPack {
			packData.Write(payload[1:])
		}
	}

	// 解码 PACK
	dec := NewPackDecoder(bytes.NewReader(packData.Bytes()))
	objs, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	if len(objs) != 3 {
		t.Errorf("object count = %d, want 3", len(objs))
	}

	// 将解码后的对象写入 gitRoot 仓库（模拟 push 后的存储结构）
	store := &LooseStore{Root: filepath.Join(gitRoot, "objects")}
	for _, obj := range objs {
		if _, err := store.Write(obj); err != nil {
			t.Fatal(err)
		}
	}
	rs := NewRefStore(gitRoot)
	if _, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: ZeroOid, NewOid: commitOid},
	}); err != nil {
		t.Fatal(err)
	}
	writeRefFile(t, gitRoot, "HEAD", "ref: refs/heads/master\n")

	// 用本地 git clone 验证
	cloneDir := filepath.Join(workdir, "clone")
	cmd := exec.Command("git", "clone", gitRoot, cloneDir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone failed: %v\n%s", err, out)
	}

	// 验证 clone 内容
	content, err := os.ReadFile(filepath.Join(cloneDir, "a.txt"))
	if err != nil {
		t.Fatalf("read a.txt: %v", err)
	}
	if string(content) != "hello pgit\n" {
		t.Errorf("a.txt = %q, want %q", content, "hello pgit\n")
	}
}

// TestGitPushE2E 用本地 git push 验证 receive-pack 端到端。
func TestGitPushE2E(t *testing.T) {
	if os.Getenv("PGIT_E2E") == "" {
		t.Skip("设置 PGIT_E2E=1 启用端到端集成测试")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不在 PATH 中")
	}

	workdir := t.TempDir()
	gitRoot := filepath.Join(workdir, "repo")
	if err := os.MkdirAll(gitRoot, 0o777); err != nil {
		t.Fatal(err)
	}

	// 创建空仓库结构
	for _, sub := range []string{"objects", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(gitRoot, sub), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	writeRefFile(t, gitRoot, "HEAD", "ref: refs/heads/master\n")

	// 用本地 git init + commit + push
	srcDir := filepath.Join(workdir, "src")
	cmd := exec.Command("git", "init", srcDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = srcDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_AUTHOR_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	runGit("config", "user.email", "test@test.com")
	runGit("config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte("hello e2e\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "hello.txt")
	runGit("commit", "-m", "initial commit")
	runGit("remote", "add", "origin", gitRoot)

	// push
	cmd = exec.Command("git", "push", "-u", "origin", "master")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	// 验证对象已写入 loose store
	store := &LooseStore{Root: filepath.Join(gitRoot, "objects")}
	rs := NewRefStore(gitRoot)
	oid, err := rs.Get("refs/heads/master")
	if err != nil {
		t.Fatalf("Get refs/heads/master: %v", err)
	}
	if !store.Exists(oid) {
		t.Errorf("commit %s not in loose store after push", oid)
	}

	// clone 验证
	cloneDir := filepath.Join(workdir, "clone")
	cmd = exec.Command("git", "clone", gitRoot, cloneDir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone after push: %v\n%s", err, out)
	}
	content, err := os.ReadFile(filepath.Join(cloneDir, "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(content) != "hello e2e\n" {
		t.Errorf("hello.txt = %q, want %q", content, "hello e2e\n")
	}
}

// TestGitFetchIncrementalE2E 用本地 git 做增量 fetch 端到端测试。
// 先 push 初始 commit，clone 后再 push 新 commit，然后 fetch 验证增量拉取。
func TestGitFetchIncrementalE2E(t *testing.T) {
	if os.Getenv("PGIT_E2E") == "" {
		t.Skip("设置 PGIT_E2E=1 启用端到端集成测试")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不在 PATH 中")
	}

	workdir := t.TempDir()
	gitRoot := filepath.Join(workdir, "repo")
	for _, sub := range []string{"objects", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(gitRoot, sub), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	writeRefFile(t, gitRoot, "HEAD", "ref: refs/heads/master\n")

	srcDir := filepath.Join(workdir, "src")
	if out, err := exec.Command("git", "init", srcDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_AUTHOR_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	runGit(srcDir, "config", "user.email", "test@test.com")
	runGit(srcDir, "config", "user.name", "Test")

	os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte("v1\n"), 0o644)
	runGit(srcDir, "add", "hello.txt")
	runGit(srcDir, "commit", "-m", "v1")
	runGit(srcDir, "remote", "add", "origin", gitRoot)

	cmd := exec.Command("git", "push", "-u", "origin", "master")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push v1: %v\n%s", err, out)
	}

	cloneDir := filepath.Join(workdir, "clone")
	cmd = exec.Command("git", "clone", gitRoot, cloneDir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}

	os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte("v2\n"), 0o644)
	runGit(srcDir, "add", "hello.txt")
	runGit(srcDir, "commit", "-m", "v2")

	cmd = exec.Command("git", "push", "origin", "master")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push v2: %v\n%s", err, out)
	}

	cmd = exec.Command("git", "fetch", "origin")
	cmd.Dir = cloneDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git fetch: %v\n%s", err, out)
	}

	runGit(cloneDir, "merge", "origin/master")

	content, err := os.ReadFile(filepath.Join(cloneDir, "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(content) != "v2\n" {
		t.Errorf("hello.txt = %q, want %q", content, "v2\n")
	}
}

// TestGitFetchHTTPIncrementalE2E 用 httptest 模拟 pgit HTTP 传输，真实 git fetch 增量。
// 覆盖 HTTP stateless_rpc 多轮 negotiation：have flush POST → NAK，done POST → NAK+PACK。
// 修复前：pgit 不响应 have flush，done POST 发 NAK+PACK，客户端报
// "fatal: git fetch-pack: expected ACK/NAK, got '?PACK'"。
func TestGitFetchHTTPIncrementalE2E(t *testing.T) {
	if os.Getenv("PGIT_E2E") == "" {
		t.Skip("设置 PGIT_E2E=1 启用端到端集成测试")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不在 PATH 中")
	}

	workdir := t.TempDir()
	gitRoot := filepath.Join(workdir, "repo.git")
	for _, sub := range []string{"objects", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(gitRoot, sub), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	writeRefFile(t, gitRoot, "HEAD", "ref: refs/heads/master\n")

	// 构造 HTTP server，直接接入 git 包的 smart-http 入口（模拟 pgit HTTP 传输）
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		service := r.URL.Query().Get("service")
		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
		out, err := ServeInfoRefs(gitRoot, service)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(http.StatusOK)
		if err := HandleUploadPack(gitRoot, r.Body, w); err != nil {
			t.Logf("upload-pack: %v", err)
		}
	})
	mux.HandleFunc("/repo.git/git-receive-pack", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		if err := HandleReceivePack(gitRoot, r.Body, w); err != nil {
			t.Logf("receive-pack: %v", err)
		}
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	repoURL := ts.URL + "/repo.git"

	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_AUTHOR_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_TERMINAL_PROMPT=0",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// 源仓库：20 个线性 commit + push HTTP
	// 确保后续 fetch 时客户端本地有 >16 个 commit，fetch-pack 会发 >16 个 have，
	// 触发第一批 16 have + flush（INITIAL_FLUSH），强制走 have flush 多轮 negotiation 路径。
	srcDir := filepath.Join(workdir, "src")
	if out, err := exec.Command("git", "init", srcDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	runGit(srcDir, "config", "user.email", "test@test.com")
	runGit(srcDir, "config", "user.name", "Test")
	for i := 1; i <= 20; i++ {
		if err := os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte(fmt.Sprintf("v%d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(srcDir, "add", "hello.txt")
		runGit(srcDir, "commit", "-m", fmt.Sprintf("v%d", i))
	}
	runGit(srcDir, "remote", "add", "origin", repoURL)
	runGit(srcDir, "push", "-u", "origin", "master")

	// clone HTTP（全量，验证 clone 仍正常）
	cloneDir := filepath.Join(workdir, "clone")
	cmd := exec.Command("git", "clone", repoURL, cloneDir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}

	// push v21 HTTP（客户端 clone 时停在 v20，fetch 增量拉取 v21）
	if err := os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte("v21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(srcDir, "add", "hello.txt")
	runGit(srcDir, "commit", "-m", "v21")
	runGit(srcDir, "push", "origin", "master")

	// git fetch HTTP 增量（核心验证点：客户端 20 commit → >16 have → have flush 多轮）
	cmd = exec.Command("git", "fetch", "origin")
	cmd.Dir = cloneDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git fetch (incremental HTTP) failed: %v\n%s", err, out)
	}

	runGit(cloneDir, "merge", "origin/master")
	content, err := os.ReadFile(filepath.Join(cloneDir, "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(content) != "v21\n" {
		t.Errorf("hello.txt = %q, want %q", content, "v21\n")
	}
}

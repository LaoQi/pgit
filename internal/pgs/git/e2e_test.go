package git

import (
	"bytes"
	"fmt"
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

package server

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pgit/internal/pgs"
	"pgit/internal/pgs/git"

	"golang.org/x/crypto/ssh"
)

// --- 仓库构造辅助（基于 git 包公开 API） ---

func oidToBin(t *testing.T, o git.Oid) []byte {
	t.Helper()
	b, err := hex.DecodeString(string(o))
	if err != nil {
		t.Fatalf("oidToBin %s: %v", o, err)
	}
	return b
}

func makeBlobObj(content string) *git.RawObject {
	return git.NewRawObject(git.ObjBlob, []byte(content))
}

func makeTreeObj(t *testing.T, entries [][2]string) *git.RawObject {
	t.Helper()
	var buf []byte
	for _, e := range entries { // [mode+" "+name, oid]
		buf = append(buf, []byte(e[0])...)
		buf = append(buf, 0)
		buf = append(buf, oidToBin(t, git.Oid(e[1]))...)
	}
	return git.NewRawObject(git.ObjTree, buf)
}

func makeCommitObj(t *testing.T, tree git.Oid) *git.RawObject {
	t.Helper()
	content := fmt.Sprintf("tree %s\n", tree)
	content += "author Test <test@pgit.dev> 1700000000 +0800\n"
	content += "committer Test <test@pgit.dev> 1700000000 +0800\n\n"
	content += "ssh test commit\n"
	return git.NewRawObject(git.ObjCommit, []byte(content))
}

// writeRepo 构造含一个 commit 的仓库（objects + refs/heads/master + HEAD + pgit.json），返回 commit oid。
func writeRepo(t *testing.T, gitRoot, name string) git.Oid {
	t.Helper()
	repoDir := filepath.Join(gitRoot, name+".git")
	for _, sub := range []string{"objects", "refs/heads"} {
		if err := os.MkdirAll(filepath.Join(repoDir, sub), 0o777); err != nil {
			t.Fatal(err)
		}
	}

	store := &git.LooseStore{Root: filepath.Join(repoDir, "objects")}
	blob := makeBlobObj("hello ssh\n")
	tree := makeTreeObj(t, [][2]string{{"100644 a.txt", string(blob.Oid())}})
	commit := makeCommitObj(t, tree.Oid())
	for _, obj := range []*git.RawObject{blob, tree, commit} {
		if _, err := store.Write(obj); err != nil {
			t.Fatalf("write object: %v", err)
		}
	}

	rs := git.NewRefStore(repoDir)
	if _, err := rs.Update([]git.RefUpdate{
		{Name: "refs/heads/master", OldOid: git.ZeroOid, NewOid: commit.Oid()},
	}); err != nil {
		t.Fatalf("update ref: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "HEAD"), []byte("ref: refs/heads/master\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writePGitMeta(t, repoDir)
	return commit.Oid()
}

// writeEmptyRepo 构造空仓库骨架（objects/refs/HEAD/pgit.json）。
func writeEmptyRepo(t *testing.T, gitRoot, name string) string {
	t.Helper()
	repoDir := filepath.Join(gitRoot, name+".git")
	for _, sub := range []string{"objects", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(repoDir, sub), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "HEAD"), []byte("ref: refs/heads/master\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writePGitMeta(t, repoDir)
	return repoDir
}

// writePGitMeta 写最小 pgit.json（name/aliases 由目录名推导）。
func writePGitMeta(t *testing.T, repoDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoDir, "pgit.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- SSH 服务端启动 / 客户端连接辅助 ---

func startSSH(t *testing.T, gitRoot string) (*SSHHandler, string) {
	t.Helper()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: gitRoot})

	hostKeyPath := filepath.Join(t.TempDir(), "hostkey")
	handler, err := NewSSHHandler(hostKeyPath, pgs.ReposManager)
	if err != nil {
		t.Fatalf("NewSSHHandler: %v", err)
	}
	if handler.HostKey == nil {
		t.Fatal("HostKey not loaded")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handler.HandleConn(conn)
		}
	}()
	return handler, ln.Addr().String()
}

func dialSSH(t *testing.T, addr string) *ssh.Client {
	t.Helper()
	cfg := &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.Password("anything")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// openExec 打开 session 并 Start exec 命令，返回 stdin/stdout 通道与 session。
func openExec(t *testing.T, client *ssh.Client, cmd string) (io.WriteCloser, *git.PktReader, *ssh.Session, *bytes.Buffer) {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	var stderr bytes.Buffer
	session.Stderr = &stderr
	if err := session.Start(cmd); err != nil {
		t.Fatalf("exec %s: %v", cmd, err)
	}
	return stdin, git.NewPktReader(stdout), session, &stderr
}

// drainFlush 连续读 pkt 直到 flush 为止。
func drainFlush(t *testing.T, pr *git.PktReader) []string {
	t.Helper()
	var lines []string
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read pkt: %v", err)
		}
		if isFlush {
			return lines
		}
		lines = append(lines, string(payload))
	}
}

// --- SSH upload-pack（clone）集成测试 ---

func TestSSHUploadPack(t *testing.T) {
	gitRoot := t.TempDir()
	commitOid := writeRepo(t, gitRoot, "test")
	_, addr := startSSH(t, gitRoot)
	client := dialSSH(t, addr)

	stdin, pr, session, _ := openExec(t, client, "git-upload-pack /test.git")

	// 1. 读 advertisement（SSH 直发 AdvertiseRefs，无 "# service" 包装）
	adv := drainFlush(t, pr)
	if len(adv) < 2 {
		t.Fatalf("advertisement lines = %d, want >= 2: %v", len(adv), adv)
	}
	if !strings.Contains(adv[0], string(commitOid)) {
		t.Errorf("advertised first line missing commit %s: %q", commitOid, adv[0])
	}

	// 2. 发 want + flush + done
	pw := git.NewPktWriter(stdin)
	if err := pw.WritePktString(fmt.Sprintf("want %s side-band-64k ofs-delta\n", commitOid)); err != nil {
		t.Fatal(err)
	}
	if err := pw.WriteFlush(); err != nil {
		t.Fatal(err)
	}
	if err := pw.WritePktString("done\n"); err != nil {
		t.Fatal(err)
	}

	// 3. 首帧 NAK
	payload, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read NAK: %v", err)
	}
	if isFlush || string(payload) != "NAK\n" {
		t.Fatalf("first frame = %q flush=%v, want NAK\\n", payload, isFlush)
	}

	// 4. 收 sideband ch1 的 PACK
	var packData bytes.Buffer
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read sideband: %v", err)
		}
		if isFlush {
			break
		}
		if len(payload) >= 1 && payload[0] == git.SidebandPack {
			packData.Write(payload[1:])
		}
	}

	// 5. 解码 PACK，验证对象
	dec := git.NewPackDecoder(bytes.NewReader(packData.Bytes()))
	objs, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	found := false
	for _, obj := range objs {
		if obj.Oid() == commitOid && obj.Type == git.ObjCommit {
			found = true
		}
	}
	if !found {
		t.Errorf("pack missing commit %s (got %d objects)", commitOid, len(objs))
	}

	if err := session.Wait(); err != nil {
		t.Errorf("session exit: %v", err)
	}
}

// --- SSH receive-pack（push）集成测试 ---

func TestSSHReceivePack(t *testing.T) {
	gitRoot := t.TempDir()
	repoDir := writeEmptyRepo(t, gitRoot, "test")
	_, addr := startSSH(t, gitRoot)
	client := dialSSH(t, addr)

	stdin, pr, session, _ := openExec(t, client, "git-receive-pack /test.git")

	// 1. 读 advertisement（receive-pack 能力 + refs + flush）
	drainFlush(t, pr)

	// 2. 构造 push 的 commit 对象
	store := &git.LooseStore{Root: filepath.Join(repoDir, "objects")}
	blob := makeBlobObj("hello push\n")
	tree := makeTreeObj(t, [][2]string{{"100644 b.txt", string(blob.Oid())}})
	commit := makeCommitObj(t, tree.Oid())

	// 3. 发 update 命令（report-status，非 sideband）+ flush + packfile
	cmd := fmt.Sprintf("%s %s refs/heads/master\x00report-status", git.ZeroOid, commit.Oid())
	if _, err := stdin.Write([]byte(fmt.Sprintf("%04x%s", len(cmd)+4, cmd))); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte(git.PktFlush)); err != nil {
		t.Fatal(err)
	}

	var packBuf bytes.Buffer
	enc := git.NewPackEncoder(&packBuf)
	if err := enc.WriteHeader(3); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []*git.RawObject{blob, tree, commit} {
		if err := enc.WriteObject(obj); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.WriteTrailer(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write(packBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	// receive-pack 读 pack 依赖 stdin EOF，模拟客户端发送完毕即关闭写端。
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}

	// 4. 收 report-status：unpack ok + ok refs/heads/master + flush
	status := drainFlush(t, pr)
	joined := strings.Join(status, "")
	if !strings.Contains(joined, "unpack ok\n") {
		t.Errorf("report-status missing 'unpack ok', got %q", status)
	}
	if !strings.Contains(joined, "ok refs/heads/master\n") {
		t.Errorf("report-status missing ref ok, got %q", status)
	}

	if err := session.Wait(); err != nil {
		t.Errorf("session exit: %v", err)
	}

	// 5. 验证 ref 与对象落盘
	rs := git.NewRefStore(repoDir)
	got, err := rs.Get("refs/heads/master")
	if err != nil {
		t.Fatalf("refs/heads/master: %v", err)
	}
	if got != commit.Oid() {
		t.Errorf("master = %s, want %s", got, commit.Oid())
	}
	for _, obj := range []*git.RawObject{blob, tree, commit} {
		if !store.Exists(obj.Oid()) {
			t.Errorf("object %s not in loose store after push", obj.Oid())
		}
	}
}

// --- mirror 仓库禁止 push ---

func TestSSHReceivePackMirrorDenied(t *testing.T) {
	gitRoot := t.TempDir()
	repoDir := writeEmptyRepo(t, gitRoot, "test")
	if err := os.WriteFile(filepath.Join(repoDir, "pgit.json"),
		[]byte(`{"name":"test","aliases":["test"],"mirror":{"remoteUrl":"http://example.com/x.git"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, addr := startSSH(t, gitRoot)
	client := dialSSH(t, addr)

	_, _, session, stderr := openExec(t, client, "git-receive-pack /test.git")
	if err := session.Wait(); err == nil {
		t.Fatal("expected push to mirror to fail")
	} else {
		t.Logf("exit: %v", err)
	}
	if !strings.Contains(stderr.String(), "mirror repository: push disabled") {
		t.Errorf("stderr = %q, want mirror push disabled", stderr.String())
	}
}

// --- 真实 git 客户端 SSH clone/push e2e（PGIT_E2E=1 + git/ssh 二进制） ---

func TestSSHClonePushE2E(t *testing.T) {
	if os.Getenv("PGIT_E2E") == "" {
		t.Skip("设置 PGIT_E2E=1 启用端到端集成测试")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不在 PATH 中")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh 不在 PATH 中")
	}

	gitRoot := t.TempDir()
	initialOid := writeRepo(t, gitRoot, "test")
	_, addr := startSSH(t, gitRoot)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}

	// 用默认算法即可连（无需 +ssh-rsa），-F /dev/null 忽略用户 ssh config。
	sshCmd := fmt.Sprintf("ssh -F /dev/null -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -p %s", port)
	env := append(os.Environ(),
		"GIT_SSH_COMMAND="+sshCmd,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)

	url := fmt.Sprintf("ssh://git@%s/test.git", host)

	cloneDir := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("git", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "clone", url, cloneDir)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone (ssh): %v\n%s", err, out)
	}
	content, err := os.ReadFile(filepath.Join(cloneDir, "a.txt"))
	if err != nil {
		t.Fatalf("read a.txt: %v", err)
	}
	if string(content) != "hello ssh\n" {
		t.Errorf("a.txt = %q, want %q", content, "hello ssh\n")
	}

	// 修改 + commit + push over SSH
	if err := os.WriteFile(filepath.Join(cloneDir, "a.txt"), []byte("hello ssh v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-c", "commit.gpgsign=false", "commit", "-am", "ssh push v2")
	cmd.Dir = cloneDir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-c", "commit.gpgsign=false", "push", "origin", "master")
	cmd.Dir = cloneDir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push (ssh): %v\n%s", err, out)
	}

	// 验证服务器端 ref 与对象已更新
	repoDir := filepath.Join(gitRoot, "test.git")
	rs := git.NewRefStore(repoDir)
	got, err := rs.Get("refs/heads/master")
	if err != nil {
		t.Fatalf("refs/heads/master: %v", err)
	}
	if got == "" || got == initialOid {
		t.Errorf("master ref not advanced after ssh push: %s", got)
	}
	store := &git.LooseStore{Root: filepath.Join(repoDir, "objects")}
	if !store.Exists(got) {
		t.Errorf("pushed commit %s not in loose store", got)
	}
}

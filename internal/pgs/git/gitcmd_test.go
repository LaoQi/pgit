package git

import "os/exec"

// gitTestArgs 是测试中调用真实 git 时的统一前缀：禁用提交/标签签名。
// 否则用户的全局 commit.gpgsign=true 会让 `git commit` 等待 GPG 口令（挂起直至超时）。
func gitTestArgs(args ...string) []string {
	return append([]string{
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
		"-c", "gpg.format=openpgp",
	}, args...)
}

// newGitCmd 构造一个禁用了签名的 git 命令。
func newGitCmd(args ...string) *exec.Cmd {
	return exec.Command("git", gitTestArgs(args...)...)
}

// gitGlobalTestArgs 供 -C <dir> 形式使用（-c 须在子命令之前，-C 同理）。
func newGitCmdIn(dir string, args ...string) *exec.Cmd {
	full := append([]string{"-C", dir}, gitTestArgs(args...)...)
	return exec.Command("git", full...)
}

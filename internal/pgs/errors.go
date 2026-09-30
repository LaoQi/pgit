package pgs

import "errors"

// 哨兵错误：供上层（HTTP/SSH）判定响应状态码，避免用字符串匹配错误信息。
var (
	// ErrRepoNotFound 仓库不存在。
	ErrRepoNotFound = errors.New("repository not found")
	// ErrAliasNotFound 别名不存在。
	ErrAliasNotFound = errors.New("repository alias not found")
	// ErrNotMirror 目标仓库不是镜像仓库。
	ErrNotMirror = errors.New("repository is not a mirror")
	// ErrNotRelay 目标仓库不是中转仓库。
	ErrNotRelay = errors.New("repository is not a relay")
	// ErrSyncInProgress 已有同步在跑（或在队列中排队）。
	ErrSyncInProgress = errors.New("sync already in progress")
	// ErrRepoExist 同名仓库已存在。
	ErrRepoExist = errors.New("repository already exists")
	// ErrRefConflict 目标 ref（仓库名或别名）已被其它仓库占用（唯一性冲突）。
	ErrRefConflict = errors.New("ref already in use")
	// ErrQueueFull 任务队列已满（任务被丢弃，稍后重试）。
	ErrQueueFull = errors.New("task queue is full")
	// ErrQueueClosed 任务队列已停止。
	ErrQueueClosed = errors.New("task queue is closed")
	// ErrGithubNotFound 目标 GitHub 账号（用户或组织）不存在或不可见。
	ErrGithubNotFound = errors.New("github owner not found")
	// ErrGithubRateLimited GitHub API 触发速率限制。
	ErrGithubRateLimited = errors.New("github api rate limit exceeded")
	// ErrGithubUpstream GitHub API 不可达或返回无法解析的响应（网络/代理/上游故障）。
	ErrGithubUpstream = errors.New("github api upstream error")
)

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
	// ErrSyncInProgress 已有同步在跑。
	ErrSyncInProgress = errors.New("sync already in progress")
	// ErrRepoExist 同名仓库已存在。
	ErrRepoExist = errors.New("repository already exists")
)

# AGENTS.md

Go 编写的个人 git 服务器。模块名 `pgit`，`go 1.26.4`。单端口多路复用 HTTP+SSH，路径映射解耦访问 URL 与存储目录，无 Web 前端（纯 API）。

## 构建状态

- `go build ./...`、`go vet ./...`、`go test ./...` 全部通过。
- 依赖 `git` 二进制在 `PATH` 中；仓库操作 shell 调用 `git`（ls-tree、cat-file、for-each-ref、archive、upload-pack/receive-pack）。`pgs.InitBare` **不调用 `git init --bare`**，手工创建裸仓库目录结构 + config + HEAD + pgit.json。

## 包结构

```
cmd/pgit/main.go              入口：flag 解析 + 配置加载 + InitReposManager + 启动单端口 mux

internal/pgs/                 业务核心包
  config.go                   Setting 结构体 + 默认值 + Reload/Output；全局 Settings 单例
  repository.go               Repository/Ref/TreeNode 模型 + git CLI 封装 + InitBare + SaveMetadata
  manager.go                  RepositoriesManager：双索引(byName/byAlias) + 扫描迁移 + CRUD + alias 增删
  task.go / task_manager.go   任务系统：状态机 + cron 调度 + 回调（有测试）
  util.go                     FileExist/GenerateKey/KeyEncode

internal/pgs/server/          网络服务层
  mux.go                      协议探测分发：peek 前缀 SSH- → SSH 否则 HTTP；peekConn 回放缓冲
  http.go                     chi 路由(/api/v1/* + alias.git 兜底) + 管理 API handler + git smart-http 传输 + Basic Auth
  ssh.go                      SSHHandler：连接级 handleConn + exec payload 解析 alias → repo
```

## 核心数据模型

```go
type Repository struct {
    Name        string    `json:"name"`        // 唯一标识 + 存储目录名，创建后不可变
    Description string    `json:"description"`
    Aliases     []string  `json:"aliases"`     // git 访问路径，不含 .git；Name 自动为首个
    CreatedAt   time.Time `json:"createdAt"`
}
```

- **存储**：`<GitRoot>/<name>.git/`（手工创建）
- **元数据**：`<GitRoot>/<name>.git/pgit.json`（name/aliases/description/createdAt）
- **启动扫描**：遍历 `<GitRoot>/*.git/pgit.json` 重建双索引(`byName`/`byAlias`)；缺 pgit.json 的旧目录自动迁移补齐（name=目录名、aliases=[目录名]）
- **alias 规则**：Name 是默认 alias 不可删；全局唯一；禁止 `/` 开头、`..`、空段、`api` 前缀
- **name 规则**：禁止 `/`、`..`、以 `.` 开头、`api`

## 端口多路复用

- 单一 `net.Listener`，Accept 后 `bufio.Reader.Peek(4)`：前缀 `SSH-` → SSH（需 `EnableSSH`），否则 → HTTP。peek 阶段 10s 读超时防空连接。
- `peekConn` 包装 conn，首次 Read 回放 peek 的字节再透传底层。
- HTTP 侧用 `singleConnListener` 包装单连接喂给 `http.Server.Serve`。
- SSH 侧直接 `ssh.NewServerConn(peekedConn, config)`。
- **SSH 认证是全放行桩**（`PasswordCallback`/`PublicKeyCallback` 都返回 nil）—— 不要假设认证被强制执行。

## API 路由

管理 API（`/api/v1/`，`HttpAuth=true` 时加 Basic Auth）：
- `GET/POST /api/v1/repos`、`GET/DELETE /api/v1/repos/{name}`
- `POST/DELETE /api/v1/repos/{name}/aliases[/{alias}]`
- `GET /api/v1/repos/{name}/{tree|blob|archive}/{ref}[/*]`

Git 传输（`/{alias}.git/`，alias 可含斜杠，受 `HttpAuth` 鉴权）：
- `GET /{alias}.git/info/refs`、`POST /{alias}.git/git-{command}`

> chi v4 路由：`/api/v1/*` 先注册优先匹配；alias.git 走 `r.NotFound` 兜底，handler 内找 `.git/` 分割 alias 与子路径。

## 配置与运行

- 生成默认配置：`pgit -d > config.json`；运行：`pgit -c config.json`。无配置以 `ConfigError` 退出。
- 配置字段：`listen`（单一监听地址，默认 `0.0.0.0:3000`）、`enableSSH`、`gitRoot`、`httpAuth`、`credentials`、`sshHostKey`/`sshPublicKey`、`sshAuthType`。无分离端口字段。
- SSH host key 缺失时自动生成到配置路径。

## 运行时布局

- `GitRoot` 默认 `./repo`（gitignored）。`static/`、`repo/`、`pgit` 二进制均被 gitignore。

## 测试与质量

- `internal/pgs` 有真实测试：`repository_test.go`（InitBare 生成 pgit.json 验证、Manager 双索引、alias 增删、扫描恢复、name/alias 校验）、`task_test.go`（约 6 秒，任务调度）。`go test ./...` 通过。
- 无 linter/formatter/CI 配置。用 `go vet ./...` 和 `go build` 验证。

## 工作流

- 默认分支 `master`（稳定）；`develop` 为重构分支。远程 `https://github.com/LaoQi/pgit.git`。
- 提交较随意（多为 `WIP`）；不强制 conventional-commits。
- 提交签名规则见全局 `~/.config/opencode/AGENTS.md`（以 LaoQi 身份提交时需 GPG 签名）。

# AGENTS.md

Go 编写的个人 git 服务器。模块名 `pgit`，`go 1.26.4`。单端口多路复用 HTTP+SSH，路径映射解耦访问 URL 与存储目录，内置简易 WebUI（embed 嵌入，可导出至磁盘自定义）。

本文件只描述**当前**架构、约束与用法；变更历史、修复记录与演进决策见 `CHANGELOG.md`。

## 构建状态

- `go build ./...`、`go vet ./...`、`go test ./...` 全部通过。
- **不依赖 `git` 二进制**：git 传输（HTTP smart-http + SSH exec 的 upload-pack/receive-pack）与浏览 API（Tree/Blob/Archive/ForEachRef）均由 `internal/pgs/git` 纯 Go 实现；浏览 API 基于 `browse.go`（`ResolveTreeIsh`/`TreeAt`/`BlobAt`/`ForEachRefs`）+ 标准库 `archive/zip`，对象经 `LooseStore` 读取。运行时无需 `git` 在 `PATH`。
- `pgs.InitBare` 手工创建裸仓库目录结构 + config + HEAD + pgit.json，支持指定默认分支（`defaultBranch` 参数，空值默认 `master`）。默认分支可经 `POST /api/v1/repos/{name}/default-branch` 切换（要求分支已存在）；`DefaultBranch()` 读 HEAD symref；浏览 API 空 ref 时用仓库默认分支。
- **对象仅 loose 存储**：仓库对象来自 pgit 自身 receive-pack（HTTP/SSH push，pack 自动解包为 loose）。不支持外部 `git` 导入的含 packfile 仓库直读（`LooseStore` 只读 loose）。
- **镜像仓库**：纯 Go fetch 客户端（`fetch.go`）从远程 HTTP/HTTPS smart-http 仓库全量镜像所有 refs；定时自动同步（`SyncManager` per-repo goroutine）+ 手动同步（API）；同步日志 JSONL（`pgit-sync.jsonl`）。

## 包结构

```
cmd/pgit/main.go              入口：flag 解析（-c/-v/-d/-w）+ 配置加载 + InitReposManager + 启动单端口 mux

internal/pgs/                 业务核心包
  config.go                   Setting 结构体（含 webuiPrefix/webuiAssets）+ 默认值 + Reload/Output；全局 Settings 单例
  repository.go               Repository/Ref/TreeNode/MirrorConfig 模型 + 浏览 API（Tree/Blob/Archive/ForEachRef，接入 git 包）+ InitBare（支持指定默认分支）+ SaveMetadata（原子写 tmp+rename）+ DefaultBranch/SetDefaultBranch + IsMirror
  manager.go                  RepositoriesManager：双索引(byName/byAlias) + 扫描迁移 + CRUD（支持指定默认分支）+ alias 增删 + CreateMirrorRepository + SyncRepository（调 FetchRemote）
  sync_log.go                 SyncLogEntry + AppendSyncLog（JSONL 追加写）+ ReadSyncLog（最新 N 条倒序）
  sync_manager.go             SyncManager：per-repo goroutine 定时调度（1-10s 错峰+initial+ticker scheduled）+ 并发保护（syncing 防重入）+ SyncNow（手动同步返回 SyncLogEntry）+ Stop
  task.go / task_manager.go   任务系统：状态机 + cron 调度 + 回调（有测试）
  util.go                     FileExist

internal/pgs/git/             纯 Go git wire protocol v0 服务端（无第三方依赖）
  oid.go object.go            ObjectID（SHA1 hex/bytes 互转）+ Object 类型常量 + RawObject.Oid() lazy 缓存
  loose.go                    松散对象读写：zlib 压缩落盘 + 逐对象 SHA1 重算校验
  parse.go                    松散对象内容解析（header + body）
  refs.go                     RefStore：loose + packed-refs 合并视图；per-ref lock+rename 原子写；CAS/symref；SetHead（原子写 HEAD symref）
  pktline.go                  pkt-line 读写器（含 flush/delim）
  delta.go                    delta 应用（ApplyDelta）+ 生成（EncodeDelta，固定窗口滚动hash，桶扫描限制≤64 position + 死亡桶淘汰）+ 收益预检（deltaPrecheck 采样命中）+ varintLE
  pack_encode.go              packfile 编码：full 对象 + 出向 OFS_DELTA（偏移追踪）；zlib BestSpeed 压缩
  pack_decode.go              packfile 解码 + 逐对象校验；REF_DELTA base 不在 pack 内时回查 LooseStore
  reach.go                    可达性遍历（BFS 去重，跳过 gitlink）
  browse.go                   浏览 API 高层：ResolveTreeIsh/TreeAt/BlobAt/ForEachRefs/CommitLog（基于 LooseStore+RefStore）
  protocol.go                 v0 状态机：negotiation + pack 交换（出向 delta 配对 + 预检跳过）+ sideband-64k + report-status + 操作日志（logLevel=detail 逐条 want/have/object/delta）+ 阶段计时（negotiate/reach/plan/encode）+ force-push 审计标记（isFastForward BFS，仅日志不拒绝）；LogLevel/SetLogLevel 由 pgs 配置注入（避免 pgs/git → pgs 循环依赖）
  service.go                  对外入口：ServeInfoRefs/HandleUploadPack/HandleReceivePack/HandleSSHSession
  fetch.go                    纯 Go fetch 客户端：FetchRemote（HTTP smart-http upload-pack 客户端）+ FetchAuth + FetchResult；复用 PktReader/PackDecoder/LooseStore/RefStore；sideband demux + ref 镜像更新（CAS 含删除）+ HEAD best-effort 更新；done 后首帧接受 NAK 或 ACK <oid>（cgit 基本模式兼容）

internal/pgs/server/          网络服务层
  mux.go                      协议探测分发：peek 前缀 SSH- → SSH 否则 HTTP；peekConn 回放缓冲
  http.go                     chi 路由(/api/v1/* + /{webuiPrefix}/* + alias.git 兜底) + 管理 API handler + git smart-http 传输（接入 git 包，Content-Encoding: gzip 自动解压）+ Basic Auth + 请求日志中间件（方法/路径/状态码/耗时/用户/远程地址）
  ssh.go                      SSHHandler：host key 支持 ed25519（生成）/RSA（兼容旧 PKCS1）+ exec payload 解析 alias（剥离前导 `/`）→ repo（接入 git 包）；env 请求明确 Reply(false) 拒绝 GIT_PROTOCOL v2，客户端确定性降级 v0
  web.go                      WebUI：embed 嵌入 web/ 资源 + ExportWebUI 导出 + serveWebUI（静态资源 + SPA fallback + 前缀注入）
  apidocs.go                  API 文档端点：GET /api/v1/ 返回 13 个管理 API 的结构化描述 JSON
  web/                        embed 源：index.html（含 __WEBUI_PREFIX__ 占位符）+ assets/（app.js/style.css/favicon.svg）
```

## 核心数据模型

```go
type Repository struct {
    Name        string        `json:"name"`        // 唯一标识 + 存储目录名，创建后不可变
    Description string        `json:"description"`
    Aliases     []string      `json:"aliases"`     // git 访问路径，不含 .git；Name 自动为首个
    CreatedAt   time.Time     `json:"createdAt"`
    Mirror      *MirrorConfig `json:"mirror,omitempty"` // nil=普通仓库；非 nil=镜像仓库
}

type MirrorConfig struct {
    RemoteURL    string    `json:"remoteUrl"`
    SyncInterval int       `json:"syncInterval"`  // 秒，0=仅手动
    AuthType     string    `json:"authType"`      // "none" | "basic"
    Username     string    `json:"username,omitempty"`
    Password     string    `json:"password,omitempty"`
    Proxy        string    `json:"proxy,omitempty"` // HTTP 代理 URL（含 userinfo 自动代理认证），空=直连
    LastSync     time.Time `json:"lastSync,omitempty"`
    LastError    string    `json:"lastError,omitempty"`
}
```

- **存储**：`<GitRoot>/<name>.git/`（手工创建）
- **元数据**：`<GitRoot>/<name>.git/pgit.json`（name/aliases/description/createdAt/mirror），SaveMetadata 原子写（tmp+rename）
- **同步日志**：`<GitRoot>/<name>.git/pgit-sync.jsonl`（JSONL 追加写，仅镜像仓库）
- **启动扫描**：遍历 `<GitRoot>/*.git/pgit.json` 重建双索引(`byName`/`byAlias`)；缺 pgit.json 的旧目录自动迁移补齐（name=目录名、aliases=[目录名]）
- **镜像仓库**：Mirror 非 nil 时启动自动注册 SyncManager（SyncInterval>0 时定时同步）；**禁止 push**（HTTP/SSH 入口拦截 receive-pack，详见协议层说明）
- **alias 规则**：Name 是默认 alias 不可删；全局唯一；禁止 `/` 开头、`..`、空段、`api` 前缀
- **name 规则**：禁止 `/`、`..`、以 `.` 开头、`api`

## 自研 git 协议层（internal/pgs/git）

纯 Go 实现 git wire protocol v0 服务端（实施取舍见 `CHANGELOG.md`）：

- 协议 v0 only（不广告 v2，客户端自动降级）；启用 sideband-64k（pack 走 ch1，进度走 ch2）
- upload-pack 广告 caps 不含 `multi_ack_detailed`（基本模式 v0 多轮 negotiation）：wants+flush → haves 分批（每批 flush 处回 NAK，不带 flush pkt）→ done → NAK+PACK+flush。HTTP stateless_rpc 下每个 POST 是一次 `ServeUploadPack` 调用：have 批 flush 后请求体 EOF 即 `return`（仅已发 NAK），含 done 的 POST 才发 NAK+PACK+flush；SSH 流式下 have flush 后 continue。
- upload-pack 支持 have 过滤增量 fetch：`CollectReachable` 接收 `haveOids` 可变参数，从 have 出发 BFS 标记排除集，want 可达但 have 也可达的对象不发送；want 全部被 have 覆盖时仅发 NAK+flush 不发 PACK
- push 安全仅 old-oid CAS，不限制 force-push，无大小上限；receive-pack 日志中通过 `isFastForward` BFS 检测非快进推送并标记 `[force-push]`（仅审计日志，不拒绝）
- **mirror 仓库禁止 push**：HTTP `gitTransport`（`http.go`）与 SSH `handleSession`（`ssh.go`）在协议入口最外层拦截 `git-receive-pack`（HTTP 同时拦截 `info/refs?service=git-receive-pack` 广告阶段），`repo.IsMirror()` 为真时返回 403 / SSH stderr `fatal: mirror repository: push disabled` + exit 1。upload-pack（clone/fetch）不受影响
- receive-pack 空命令列表请求（body 仅 flush-pkt，无 ref 更新、无 packfile）容忍并返回空 report-status（unpack ok + flush-pkt）
- 对象完整性逐对象 SHA1 重算校验，不做可达性检查
- REF_DELTA base 优先在 pack 内查找，fallback 回查 LooseStore（push 时 base 常是仓库已有对象）
- ref 原子性 per-ref（lock file + rename）；packed-refs 只读合并视图，写入只 loose
- 存储策略全 loose（不落盘 pack、不 repack）
- 出向 delta（clone 编码）：仅 blob 配对，单层 OFS_DELTA，固定窗口滚动hash；负收益回退（deltaLen ≥ target 一半则退 full）。性能保护：采样收益预检（`deltaPrecheck`，低相似/随机对跳过走 full，避免大 blob 白算）+ 桶扫描限制（单桶 ≤64 position，首个 ≥16 匹配贪心采用，全桶无匹配死亡桶淘汰）
- 明确不做：protocol v2 / multi_ack 与 multi_ack_detailed 交互式 ACK 状态机 / shallow / partial clone / thin pack / packfile 落盘 / repack-gc / dumb HTTP / reflog / alternates

## 端口多路复用

- 单一 `net.Listener`，Accept 后 `bufio.Reader.Peek(4)`：前缀 `SSH-` → SSH（需 `EnableSSH`），否则 → HTTP。peek 阶段 10s 读超时防空连接。
- `peekConn` 包装 conn，首次 Read 回放 peek 的字节再透传底层。
- HTTP 侧用 `singleConnListener` 包装单连接喂给 `http.Server.Serve`。
- SSH 侧直接 `ssh.NewServerConn(peekedConn, config)`。
- **SSH 认证是全放行桩**（`PasswordCallback`/`PublicKeyCallback` 都返回 nil）—— 不要假设认证被强制执行。
- **SSH host key**：默认生成 **ed25519** 密钥（PKCS8 PEM 写盘）；旧的 RSA hostkey（PKCS1 + `PRIVATE KEY` Type）兼容解析。RSA signer 自动通告 `rsa-sha2-256/512`，现代 OpenSSH 客户端（≥8.8 默认禁用 ssh-rsa）免额外配置即可连接（历史原因见 `CHANGELOG.md`）。
- **SSH exec 路径解析**：git 客户端 exec 参数形如 `git-upload-pack /alias.git`，alias 解析先 `TrimPrefix("/")` 再 `TrimSuffix(".git")`，与 HTTP alias 一致（不含 `/`）。

## API 路由

管理 API（`/api/v1/`，`HttpAuth=true` 时加 Basic Auth）：
- `GET /api/v1/`（API 文档 JSON）、`GET/POST /api/v1/repos`、`GET/DELETE /api/v1/repos/{name}`
- `POST /api/v1/repos/{name}`（创建仓库，`mirrorUrl` 表单字段存在时创建镜像仓库，支持 `mirrorInterval`/`mirrorAuthType`/`mirrorUsername`/`mirrorPassword`/`mirrorProxy`）
- `POST/DELETE /api/v1/repos/{name}/aliases[/{alias}]`
- `POST /api/v1/repos/{name}/default-branch`（设置默认分支，要求分支已存在）
- `POST /api/v1/repos/{name}/settings`（更新 description 与镜像配置：`mirrorRemoteUrl`/`mirrorInterval`/`mirrorAuthType`/`mirrorUsername`/`mirrorPassword`/`mirrorProxy`；密码留空=保留原值；改 interval 重新注册定时调度）
- `GET /api/v1/repos/{name}/{tree|blob|archive}/{ref}[/*]`
- `GET /api/v1/repos/{name}/commits/{ref}`（列出最近 commits，支持 `?limit=N`，默认 20）
- `POST /api/v1/repos/{name}/sync`（手动同步镜像仓库，返回 SyncLogEntry）
- `GET /api/v1/repos/{name}/sync-log`（查询同步日志，`?limit=N` 默认 50，最新在前）

WebUI（`/{webuiPrefix}/`，默认 `__webui`，受 `HttpAuth` 鉴权）：
- `GET /` → 302 重定向至 `/{webuiPrefix}/`
- `GET /{webuiPrefix}` 或 `GET /{webuiPrefix}/*` → `serveWebUI`：embed/磁盘静态资源 + SPA fallback（非文件请求回退 index.html）
- index.html 含 `__WEBUI_PREFIX__` 占位符，per-request 替换为实际前缀注入 `<base>` 标签
- 前端 History API 路由（非 hash）：`/` 仓库列表、`/repo/{name}` 详情、`/repo/{name}/tree/{ref}` 文件树、`/api` API 文档页

Git 传输（`/{alias}.git/`，alias 可含斜杠，受 `HttpAuth` 鉴权）：
- `GET /{alias}.git/info/refs`、`POST /{alias}.git/git-{command}`

> chi v4 路由优先级：`/api/v1/*` 与 `/{webuiPrefix}/*` 显式注册先匹配；alias.git 走 `r.NotFound` 兜底，handler 内找 `.git/` 分割 alias 与子路径。`webuiPrefix` 默认 `__webui`，可配置为多段（如 `custom/ui`）。

## 配置与运行

- 生成默认配置：`pgit -d > config.json`；运行：`pgit -c config.json`。无配置以 `ConfigError` 退出。
- 导出 WebUI 资源：`pgit -w ./webui`（将 embed 的 web/ 写到磁盘，可自定义修改后通过 `webuiAssets` 加载）。
- 配置字段：`listen`（单一监听地址，默认 `0.0.0.0:3000`）、`enableSSH`、`gitRoot`、`httpAuth`、`credentials`、`sshHostKey`/`sshPublicKey`、`sshAuthType`、`webuiPrefix`（默认 `__webui`）、`webuiAssets`（默认空=用 embed，非空=从磁盘目录读）、`logLevel`（默认空=`off` 仅汇总行；`detail` 逐条 want/have/object/delta；由 `Reload` 注入 `git.SetLogLevel`）。无分离端口字段。
- `webuiPrefix` 校验：非空、不为 `api`、不含 `..`；可含多段斜杠（如 `custom/ui`）。
- `webuiAssets` 校验：非空时目录必须存在且可访问。
- SSH host key 缺失时自动生成到配置路径。

## 运行时布局

- `GitRoot` 默认 `./repo`（gitignored）。`repo/`、根目录 `pgit` 二进制、`config.json`（含凭证）均被 gitignore。
- WebUI 资源源码 `internal/pgs/server/web/` 进 git（embed 要求源码树可见）；运行时从 embed 或 `webuiAssets` 指定磁盘目录读取。

## 测试与质量

- `internal/pgs`：`repository_test.go`（InitBare 与 pgit.json/自定义默认分支、Manager 双索引与扫描恢复、alias 增删与校验、SetDefaultBranch、CreateMirrorRepository、MirrorBackwardCompat、URL 校验）、`repository_browse_test.go`（Tree/Blob/Archive/ForEachRef 端到端，构造 loose 对象）、`sync_log_test.go`、`sync_manager_test.go`、`task_test.go`（约 6 秒）。
- `internal/pgs/server`：`ssh_test.go` 走真实 TCP + x/crypto 客户端（upload-pack clone 全量交换验证 pack 对象、receive-pack push 验证 ref+loose 落盘、mirror 仓库 push 拒绝 stderr），无需 git/ssh 二进制；`TestSSHClonePushE2E` 需 `PGIT_E2E=1` + git/ssh 二进制。
- `internal/pgs/git`：`loose_test`/`delta_test`/`pack_test`/`refs_test`/`reach_test`/`browse_test`/`protocol_test`/`fetch_test`/`e2e_test`，覆盖 delta 应用与生成 roundtrip、deltaPrecheck 预检、桶扫描限制、pack 编解码（与真实 git pack、index-pack 互验）、ofs-delta 回环、ref CAS/symref/packed-refs、可达性 BFS 与 have 差量过滤、treeIsh/tree/blob/ForEachRefs、v0 状态机 + sideband、增量 fetch（have flush/多 POST/无 done 等）、fetch 客户端（initial/incremental/up-to-date/empty/basic auth/ref 删除/ACK 响应，httptest + 自身协议当远程，无需外部 git）；e2e 集成需 `PGIT_E2E=1`。`go test ./...` 通过。
- 无 linter/formatter/CI 配置。用 `go vet ./...` 和 `go build` 验证。

## 工作流

- 默认分支 `master`（稳定）；`develop` 为重构分支。远程 `https://github.com/LaoQi/pgit.git`。
- 提交较随意（多为 `WIP`）；不强制 conventional-commits。
- 提交签名规则见全局 `~/.config/opencode/AGENTS.md`（以 LaoQi 身份提交时需 GPG 签名）。

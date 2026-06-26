# AGENTS.md

Go 编写的个人 git 服务器。模块名 `pgit`，`go 1.26.4`。单端口多路复用 HTTP+SSH，路径映射解耦访问 URL 与存储目录，内置简易 WebUI（embed 嵌入，可导出至磁盘自定义）。

## 构建状态

- `go build ./...`、`go vet ./...`、`go test ./...` 全部通过。
- **整个服务已不依赖 `git` 二进制**：git 传输（clone/push，HTTP smart-http + SSH exec 的 upload-pack/receive-pack）与浏览 API（Tree/Blob/Archive/ForEachRef）均由 `internal/pgs/git` 包纯 Go 实现。运行时无需 `git` 在 `PATH`。
- 浏览 API 基于 `internal/pgs/git/browse.go`（`ResolveTreeIsh`/`TreeAt`/`BlobAt`/`ForEachRefs`）+ 标准库 `archive/zip`，对象经 `LooseStore` 读取。
- `pgs.InitBare` **不调用 `git init --bare`**，手工创建裸仓库目录结构 + config + HEAD + pgit.json。支持指定默认分支（`defaultBranch` 参数，空值默认 `master`）。
- 仓库默认分支：创建时指定（预设 `master`），可通过 API 切换（`POST /api/v1/repos/{name}/default-branch`，要求分支已存在）。`DefaultBranch()` 从 HEAD symref 读取。浏览 API（tree/blob/archive）空 ref 时使用仓库默认分支而非硬编码 `master`。
- **仓库导入仅限 pgit 自身的 receive-pack**（HTTP/SSH push）：导入时 pack 自动解包为 loose（`protocol.go`），从源头保证对象全 loose。**不支持外部 `git` 导入的含 packfile 仓库直读**（`LooseStore` 只读 loose，不读 packfile）。

## 包结构

```
cmd/pgit/main.go              入口：flag 解析（-c/-v/-d/-w）+ 配置加载 + InitReposManager + 启动单端口 mux

internal/pgs/                 业务核心包
  config.go                   Setting 结构体（含 webuiPrefix/webuiAssets）+ 默认值 + Reload/Output；全局 Settings 单例
  repository.go               Repository/Ref/TreeNode 模型 + 浏览 API（Tree/Blob/Archive/ForEachRef，接入 git 包）+ InitBare（支持指定默认分支）+ SaveMetadata + DefaultBranch/SetDefaultBranch
  manager.go                  RepositoriesManager：双索引(byName/byAlias) + 扫描迁移 + CRUD（支持指定默认分支）+ alias 增删
  task.go / task_manager.go   任务系统：状态机 + cron 调度 + 回调（有测试）
  util.go                     FileExist/GenerateKey/KeyEncode

internal/pgs/git/             纯 Go git wire protocol v0 服务端（无第三方依赖）
  oid.go object.go            ObjectID（SHA1 hex/bytes 互转）+ Object 类型常量
  loose.go                    松散对象读写：zlib 压缩落盘 + 逐对象 SHA1 重算校验
  parse.go                    松散对象内容解析（header + body）
  refs.go                     RefStore：loose + packed-refs 合并视图；per-ref lock+rename 原子写；CAS/symref；SetHead（原子写 HEAD symref）
  pktline.go                  pkt-line 读写器（含 flush/delim）
  delta.go                    delta 应用（ApplyDelta）+ 生成（EncodeDelta，固定窗口滚动hash）+ varintLE
  pack_encode.go              packfile 编码：full 对象 + 出向 OFS_DELTA（偏移追踪）
  pack_decode.go              packfile 解码 + 逐对象校验；REF_DELTA base 不在 pack 内时回查 LooseStore
  reach.go                    可达性遍历（BFS 去重，跳过 gitlink）
  browse.go                   浏览 API 高层：ResolveTreeIsh/TreeAt/BlobAt/ForEachRefs（基于 LooseStore+RefStore）
  protocol.go                 v0 状态机：negotiation + pack 交换（upload-pack 出向 delta 配对）+ sideband-64k + report-status
  service.go                  对外入口：ServeInfoRefs/HandleUploadPack/HandleReceivePack/HandleSSHSession

internal/pgs/server/          网络服务层
  mux.go                      协议探测分发：peek 前缀 SSH- → SSH 否则 HTTP；peekConn 回放缓冲
  http.go                     chi 路由(/api/v1/* + /{webuiPrefix}/* + alias.git 兜底) + 管理 API handler + git smart-http 传输（接入 git 包）+ Basic Auth
  ssh.go                      SSHHandler：连接级 handleConn + exec payload 解析 alias → repo（接入 git 包）
  web.go                      WebUI：embed 嵌入 web/ 资源 + ExportWebUI 导出 + serveWebUI（静态资源 + SPA fallback + 前缀注入）
  apidocs.go                  API 文档端点：GET /api/v1/ 返回 10 个管理 API 的结构化描述 JSON
  web/                        embed 源：index.html（含 __WEBUI_PREFIX__ 占位符）+ assets/（app.js/style.css/favicon.svg）
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

## 自研 git 协议层（internal/pgs/git）

纯 Go 实现 git wire protocol v0 服务端，消除 clone/push 对 `git` 二进制的依赖。设计决策（详见 `todos.md`）：

- 协议 v0 only（不广告 v2，客户端自动降级）；启用 sideband-64k（pack 走 ch1，进度走 ch2）
- upload-pack 广告 caps 不含 `multi_ack_detailed`（仅实现基本模式单轮 negotiation：wants+flush → haves → done → 单发 NAK + PACK）；广告该 cap 会导致 fetch-pack 在 have+flush 后阻塞等 ACK 而不同步，报 `expected ACK/NAK, got '?PACK'`
- upload-pack 支持 have 过滤增量 fetch：`CollectReachable` 接收 `haveOids` 可变参数，从 have 出发 BFS 标记排除集，want 可达但 have 也可达的对象不发送；want 全部被 have 覆盖时仅发 NAK+flush 不发 PACK
- push 安全仅 old-oid CAS，不限制 force-push，无大小上限
- receive-pack 空命令列表请求（body 仅 flush-pkt，无 ref 更新、无 packfile）容忍并返回空 report-status（unpack ok + flush-pkt），与 cgit 一致
- 对象完整性逐对象 SHA1 重算校验，不做可达性检查
- REF_DELTA base 优先在 pack 内查找，fallback 回查 LooseStore（push 时 base 常是仓库已有对象）
- ref 原子性 per-ref（lock file + rename）；packed-refs 只读合并视图，写入只 loose
- 存储初始版全 loose（不落盘 pack、不 repack）
- 出向 delta（clone 编码）：仅 blob 配对，单层 OFS_DELTA，固定窗口滚动hash；负收益回退（deltaLen ≥ target 一半则退 full）
- 明确不做：protocol v2 / 多轮 negotiation / shallow / partial clone / thin pack / packfile 落盘 / repack-gc / dumb HTTP / reflog / alternates

## 端口多路复用

- 单一 `net.Listener`，Accept 后 `bufio.Reader.Peek(4)`：前缀 `SSH-` → SSH（需 `EnableSSH`），否则 → HTTP。peek 阶段 10s 读超时防空连接。
- `peekConn` 包装 conn，首次 Read 回放 peek 的字节再透传底层。
- HTTP 侧用 `singleConnListener` 包装单连接喂给 `http.Server.Serve`。
- SSH 侧直接 `ssh.NewServerConn(peekedConn, config)`。
- **SSH 认证是全放行桩**（`PasswordCallback`/`PublicKeyCallback` 都返回 nil）—— 不要假设认证被强制执行。

## API 路由

管理 API（`/api/v1/`，`HttpAuth=true` 时加 Basic Auth）：
- `GET /api/v1/`（API 文档 JSON）、`GET/POST /api/v1/repos`、`GET/DELETE /api/v1/repos/{name}`
- `POST/DELETE /api/v1/repos/{name}/aliases[/{alias}]`
- `POST /api/v1/repos/{name}/default-branch`（设置默认分支，要求分支已存在）
- `GET /api/v1/repos/{name}/{tree|blob|archive}/{ref}[/*]`

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
- 配置字段：`listen`（单一监听地址，默认 `0.0.0.0:3000`）、`enableSSH`、`gitRoot`、`httpAuth`、`credentials`、`sshHostKey`/`sshPublicKey`、`sshAuthType`、`webuiPrefix`（默认 `__webui`）、`webuiAssets`（默认空=用 embed，非空=从磁盘目录读）。无分离端口字段。
- `webuiPrefix` 校验：非空、不为 `api`、不含 `..`；可含多段斜杠（如 `custom/ui`）。
- `webuiAssets` 校验：非空时目录必须存在且可访问。
- SSH host key 缺失时自动生成到配置路径。

## 运行时布局

- `GitRoot` 默认 `./repo`（gitignored）。`repo/`、根目录 `pgit` 二进制、`config.json`（含凭证）均被 gitignore。
- WebUI 资源源码 `internal/pgs/server/web/` 进 git（embed 要求源码树可见）；运行时从 embed 或 `webuiAssets` 指定磁盘目录读取。

## 测试与质量

- `internal/pgs` 有真实测试：`repository_test.go`（InitBare 生成 pgit.json 验证、InitBare 自定义默认分支、Manager 双索引、alias 增删、扫描恢复、name/alias 校验、SetDefaultBranch 存在/不存在/非法名校验）、`repository_browse_test.go`（Tree/Blob/Archive/ForEachRef 端到端，构造 loose 对象验证）、`task_test.go`（约 6 秒，任务调度）。
- `internal/pgs/git` 覆盖完整：`loose_test`/`delta_test`/`pack_test`/`refs_test`/`reach_test`/`browse_test`/`protocol_test`/`e2e_test`（基础读写、delta 应用+生成 roundtrip、pack 编解码与真实 git pack 互验、ofs-delta 编码回环+git index-pack 互验、ref CAS/symref/packed-refs、SetHead 原子写、可达性 BFS+have 差量过滤（完全覆盖/部分覆盖/共享 tree/无覆盖/线性链/缺失 oid/ZeroOid）、treeIsh 解析/tree 遍历/blob 读取/ForEachRefs、v0 状态机+sideband+空仓库回环、upload-pack delta 配对端到端、NAK 首帧独立验证、增量 fetch（have 过滤：基本增量/have==want 无 PACK/多 have/have 间 flush/无关分支/非 sideband 增量）、e2e 集成测试（需 `PGIT_E2E=1`，clone/push/fetch 增量拉取端到端验证））。`go test ./...` 通过。
- 无 linter/formatter/CI 配置。用 `go vet ./...` 和 `go build` 验证。

## 工作流

- 默认分支 `master`（稳定）；`develop` 为重构分支。远程 `https://github.com/LaoQi/pgit.git`。
- 提交较随意（多为 `WIP`）；不强制 conventional-commits。
- 提交签名规则见全局 `~/.config/opencode/AGENTS.md`（以 LaoQi 身份提交时需 GPG 签名）。

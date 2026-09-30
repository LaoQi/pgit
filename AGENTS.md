# AGENTS.md

Go 编写的个人 git 服务器。模块名 `pgit`，`go 1.26.4`。单端口多路复用 HTTP+SSH，路径映射解耦访问 URL 与存储目录，内置简易 WebUI（embed 嵌入，可导出至磁盘自定义）。

仓库有三种形态：**普通**（本地权威，下游 push）、**镜像**（上游权威，定时拉取）、**中转 relay**（上游为基线，下游快进 push 准入后自动转发上游）。后两者共用 `MirrorConfig` 远端配置与拉取/日志/状态设施，差异只在 `mode`。

本文件只描述**当前**架构、约束与用法；变更历史与演进决策见 `CHANGELOG.md`，架构评估见 `docs/architecture-review.md`，后续计划见 `docs/roadmap.md`，安全策略见 `docs/security-policy.md`。

## 构建状态

- `go build ./...`、`go vet ./...`、`go test ./...` 全部通过。
- **不依赖 `git` 二进制**：git 传输（HTTP smart-http + SSH exec 的 upload-pack/receive-pack）与浏览 API（Tree/Blob/Archive/ForEachRef）均由 `internal/pgs/git` 纯 Go 实现；浏览 API 基于 `browse.go`（`ResolveTreeIsh`/`TreeAt`/`BlobAt`/`ForEachRefs`）+ 标准库 `archive/zip`，对象经 `git.ObjectStore` 接口读取（当前唯一实现 `LooseStore`）。运行时无需 `git` 在 `PATH`。
- `pgs.InitBare` 手工创建裸仓库目录结构 + config + HEAD + pgit.json，支持指定默认分支（`defaultBranch` 参数，空值默认 `master`）。默认分支可经 `POST /api/v1/repos/default-branch`（`ref` + `branch`）切换（要求分支已存在）；`DefaultBranch()` 读 HEAD symref；浏览 API 空 ref 时用仓库默认分支。
- **对象仅 loose 存储**：仓库对象来自 pgit 自身 receive-pack（HTTP/SSH push，pack 自动解包为 loose）。不支持外部 `git` 导入的含 packfile 仓库直读（`LooseStore` 只读 loose、不读 packfile）。
- **传输流式化**：push/fetch 侧 `PackDecoder` 逐对象流式解析（`DecodeTo` 直接落盘，不驻留全量对象）；clone 侧 `WalkReachable` 只读对象头部（`ObjectStore.Stat`）+ 单遍 `encodePack` 编码。资源上限：`maxPushBytes`（默认 2GiB）、`maxConcurrentPacks`（默认 4）。
- **镜像仓库**：纯 Go fetch 客户端（`fetch.go`）从远程 HTTP/HTTPS smart-http 仓库全量镜像所有 refs；定时自动同步（`SyncManager` per-repo goroutine）+ 手动同步（API）；同步日志 JSONL（`pgit-sync.jsonl`）。
- **中转仓库（relay）**：`MirrorConfig.Mode == "relay"` 的镜像仓库。拉取方向与镜像一致（`pendingRefs` 受 `FetchOptions.ProtectRefs` 保护，不被 pull 回退）；写方向新增两道机制：① **push 准入**（`RelayManager.Admit`，接入 receive-pack 的 `PreRefs` 回调）要求每个 ref 与**实时 `ls-remote` 基线**一致（`old == 上游 oid`）且为快进/新建/删除，否则 ng 拒绝、本地不更新；② **转发**（`git/push.go` 的纯 Go receive-pack 客户端）在 push 成功后异步把 ref 推上游，待转发集合落盘 `pendingRefs`，网络失败退避重试、上游明确拒绝则放弃重试并记 `lastPushError`，转发日志 `pgit-relay.jsonl`。因为准入保证了「本地 = 上游基线 或 pending」，转发永远是快进推送，不需要 force、不产生分叉；代价是下游必须**先 fetch 再 push**（落后即 `stale base` 拒绝）。
- **GitHub 导入**：`GET /api/v1/github/repos`（只读发现账号仓库，标注本地命名建议与冲突）+ `POST /api/v1/github/import`（勾选后逐仓库生成镜像仓库）。**无账号实体**：账号信息只在导入时使用一次，Token 落到各仓库的 `MirrorConfig` 基本认证字段，产物与手工创建的镜像仓库完全一致。
- **同步任务队列**：所有同步（首次/定时/手工/导入）统一提交到有界任务队列（`task_queue.go`），worker 数 = `mirrorMaxConcurrentSyncs`（默认 5，可热加载），容量固定 1024。队列满时定时任务丢弃并计数（不阻塞不重试），手工同步立刻报错（503）。

## 包结构

```
cmd/pgit/main.go              入口：flag 解析（-c/-v/-d/-w）+ 配置加载 + InitReposManager + 启动单端口 mux

internal/pgs/                 业务核心包
  config.go                   Setting 结构体（含 webuiPrefix/webuiAssets/logLevel/logFormat/传输上限）+ RWMutex + 默认值 + Reload（初始化）/HotReload（SIGHUP 热加载）+ Snapshot/SettingView + LimitPushBytes/LimitConcurrentPacks；全局 Settings 单例
  repository.go               Repository/Ref/TreeNode/MirrorConfig 模型（Repository 自包含 root，Path()/Root() 不依赖全局）+ 浏览 API（Tree/Blob/Archive/ForEachRef，接入 git 包）+ InitBare(gitRoot,...)（支持指定默认分支）+ SaveMetadata（原子写 tmp+rename）+ Snapshot（并发安全快照）+ DefaultBranch/SetDefaultBranch + IsMirror
  manager.go                  RepositoriesManager：RWMutex 保护的 byName/byAlias 双索引（对外方法返回 Repository 快照）+ root()（Config.GitRoot）+ 扫描迁移（跳过 pgit.deleted 软删除标记目录）+ CRUD（支持指定默认分支；建仓前目录预检，软删遗留目录给出可行动提示）+ DeleteRepository（软删除：写标记不删数据）+ alias 增删 + CreateMirrorRepository + SyncRepository（锁内取快照→fetch→锁内回写状态）
  sync_manager.go             SyncManager（NewSyncManager(manager) 注入）：per-repo goroutine 定时调度（initial+ticker scheduled）+ inflight 判重（入队即占位）+ SyncNow（手动同步等待结果，排队上限 30s）+ SyncAsync（异步触发，供导入用）+ Status/Statuses（调度状态视图，含 queued/syncing）+ SetConcurrency（热加载调整）+ QueueStats + 间隔变更重建调度器 + Stop（WaitGroup + 队列停止，可重复调用）
  relay_config.go             中转仓库配置面：MirrorMode 常量（pull/relay）、IsRelay、AllowsDelete、ForwardRefPrefixes、AcceptsRef、validateRelayRefs；默认白名单 refs/heads/ + refs/tags/
  relay_base.go               上游基线内存视图 relayBase：Set（校准后整体替换）/Advance/Remove/Oid/Calibrated/Snapshot/Len；准入判定的唯一权威来源
  relay_manager.go            RelayManager（NewRelayManager(manager) 注入）：Admit（receive-pack 准入 gate：实时 ls-remote + 逐 ref 快进/stale/白名单/删除判定）/ OnRefsUpdated（push 成功后落盘 pending + 入队）/ RelayNow（手动，先 reconcile 本地领先）/ AlignToUpstream（危险出口：强制对齐基线并丢弃 pending）/ Bootstrap（重启补推 + 异步校准）/ Status/Statuses / SetConcurrency / QueueStats / Stop；任务队列 + 每轮最多 3 轮合并 + 退避重试（relayPendingRetryIntervalSec）+ 上游 ng 即放弃重试；applyRound 经 UpdateMirrorRuntime 回写 pending/lastPush/lastPushError
  relay_log.go                RelayLogEntry + AppendRelayLog/ReadRelayLog（pgit-relay.jsonl，复用 appendJSONL/readJSONL 泛型实现）
  sync_log.go                 SyncLogEntry + AppendSyncLog/ReadSyncLog（JSONL 追加写）+ appendJSONL/readJSONL 泛型公共件（sync 与 relay 日志共用）
  task_queue.go               通用有界并发任务队列 TaskQueue：Submit（满→ErrQueueFull，触发任务 Dropped 回调）/ SetWorkers（热调整，收缩时 worker 自退）/ Stats / Stop（丢弃未开始 + 等运行中收尾）；单任务执行时长由任务自身超时兜住，无公平性保证
  github.go                   GitHub 发现（只读）：DiscoverGithubRepos（用户/组织入口选择、Link 分页、fork/archived/disabled 过滤、限流与未找到分类）+ FetchGithubRepo（单仓库兜底）+ 客户端/重试/代理
  github_import.go            GitHub 导入：ImportGithubMirrors（发现 → 逐个 CreateMirrorRepositoryWithBranch + AddAlias + Register/SyncAsync → 逐仓库结果）、AnnotateGithubRepos（本地命名与冲突标注）、conflictCode/sameRemoteURL、选择清洗与上限校验
  log.go                      SetupLogging：slog（text/json）初始化 + 标准库 log 重定向（既有 log.Printf 自动结构化）；级别 debug(兼容 detail)/info/warn/error
  metrics.go                  自研指标表（counter/gauge + Prometheus 文本格式，无第三方依赖）+ 采集辅助（HTTP/git/pack/镜像/仓库）
  task.go / task_manager.go

internal/pgs/git/             纯 Go git wire protocol v0 服务端（无第三方依赖）
  oid.go object.go            ObjectID（SHA1 hex/bytes 互转）+ Object 类型常量 + RawObject.Oid() lazy 缓存
  loose.go                    ObjectStore 接口（Read/Exists/Write/Stat）+ NewObjectStore(repoRoot)；LooseStore 松散对象读写：zlib 压缩落盘 + 逐对象 SHA1 重算校验；Stat 只解压头部取 type/size
  parse.go                    松散对象内容解析（header + body）
  refs.go                     RefStore：loose + packed-refs 合并视图（Update 一次解析并复用视图）；per-ref lock+rename 原子写；CAS/symref；SetHead（原子写 HEAD symref）
  pktline.go                  pkt-line 读写器（含 flush/delim）
  delta.go                    delta 应用（ApplyDelta，边界检查 + tgtSize 上限）+ 生成（EncodeDelta，固定窗口滚动hash，桶扫描限制≤64 position + 死亡桶淘汰）+ 收益预检（deltaPrecheck 采样命中）+ varintLE
  pack_encode.go              packfile 编码：full 对象 + 出向 OFS_DELTA（偏移追踪）；zlib BestSpeed 压缩
  pack_decode.go              packfile 流式解码（DecodeTo 逐对象落盘 / Decode 收集）+ 逐对象校验 + trailer SHA1 边读边校验；OFS/REF_DELTA base 经偏移→oid 回查 ObjectStore
  reach.go                    可达性遍历：WalkReachable（只 Stat 读头部，clone 用）/ CollectReachable（内容全量驻留，测试与小仓库用）；BFS 去重、跳过 gitlink、have 排除集
  browse.go                   浏览 API 高层：ResolveTreeIsh/TreeAt/BlobAt/ForEachRefs（refs 指纹缓存 + InvalidateRefsCache）/CommitLog（基于 ObjectStore+RefStore）
  protocol.go                 v0 状态机：negotiation + pack 交换（upload-pack 单遍 encodePack，blob 按 size 降序配对 + 采样预检 + 负收益回退；receive-pack 流式落盘，pack 被拒回 report-status）+ sideband-64k + report-status + 操作日志（logLevel=detail）+ 阶段计时（negotiate/reach/encode）+ force-push 审计标记（isFastForward BFS，仅日志不拒绝）；LogLevel/SetLogLevel 与 SetMaxReceivePackBytes 由 pgs 配置注入
  service.go                  对外入口：ServeInfoRefs/HandleUploadPack/HandleReceivePack/HandleSSHSession
  fetch.go                    纯 Go fetch 客户端：FetchRemote/FetchRemoteWithOptions（HTTP smart-http upload-pack 客户端）+ FetchAuth/FetchOptions/FetchResult；分层超时（Dial/TLS/ResponseHeader/IdleConn + stallReader 停滞看门狗）取代整体超时 + 指数退避重试（isRetryableFetchError 分类）；复用 PktReader/PackDecoder/LooseStore/RefStore；sideband demux + ref 镜像更新（CAS 含删除）+ HEAD best-effort；done 后首帧接受 NAK 或 ACK <oid>；FetchOptions.ProtectRefs 列出不参与本地更新的 ref（中转仓库保护待转发 ref）
  push.go                     纯 Go push 客户端（receive-pack over smart-http）：PushRemote/RefSpec/PushResult/PushRefResult；命令 old 取广告值（CAS）+ 待发对象 = WalkReachable(new, haves=上游 refs+命令 old) + io.Pipe 流式请求体（命令 pkt-line + flush + pack）+ report-status 解析（sideband 经 SidebandReader 重组）+ parseReportStatus；上游无 ofs-delta 时退化全量编码（encodePackForPush）；部分 ref 被拒不算函数错误（PushResult.Failed/AllOK）
  lsremote.go                 LsRemote：只读远端 ref 广告（不传对象），service 决定 upload-pack / receive-pack 视图；中转仓库准入与对齐用它校准上游基线
  remote.go                   远端 smart-http 公共件：remoteSession（分层超时 http.Client + 停滞看门狗 ctx + 代理/认证 + Close）+ newRemoteRequest + fetchRefAdvertisement/parseRefAdvertisement（跳 capabilities^{} 与 peeled ^{} 行）+ hasCap；fetch 与 push 共用
  pktline.go                  pkt-line 读写器（含 flush/delim）+ SidebandWriter（出向分帧）+ SidebandReader（入向把 sideband-64k 重组为 ch1 字节流，ch2 进度丢弃/ch3 报错）

internal/pgs/server/          网络服务层
  mux.go                      协议探测分发（peek 前缀 SSH- → SSH 否则 HTTP）+ 共享 http.Server（ReadHeaderTimeout/IdleTimeout）+ connChanListener 投递连接 + peekConn 回放缓冲 + Shutdown 优雅关闭（等活动中请求/SSH 会话，超时强断，幂等）
  http.go                     路由（标准库 http.ServeMux，Go 1.22+ 方法/通配符模式：/api/v1/* + /{webuiPrefix}/* + alias.git 走 "/" 兜底，HTTPHandler 持有 Manager/Settings/Sync）+ 管理 API handler + git smart-http 传输（接入 git 包，Content-Encoding: gzip 自动解压）+ Basic Auth + 请求日志/请求 ID 中间件（responseStatusWriter 透传 Flush/Hijack/Push/ReadFrom/Unwrap）
  ssh.go                      SSHHandler（NewSSHHandler 注入 RelayManager，relay 仓库 receive-pack 同样走准入+转发接线，漏注入会整体绕过准入——ssh_test 有回归用例）：host key 支持 ed25519（生成）/RSA（兼容旧 PKCS1）+ exec payload 解析 alias（剥离前导 `/`）→ repo（仓库路径用 repo.Path()）；env 请求明确 Reply(false) 拒绝 GIT_PROTOCOL v2，客户端确定性降级 v0
  web.go                      WebUI：embed 嵌入 web/ 资源 + ExportWebUI 导出 + serveWebUI（静态资源 + SPA fallback + 前缀注入）
  apidocs.go                  API 文档端点：GET /api/v1/ 返回 21 个管理 API 的结构化描述 JSON
  web/                        embed 源：index.html（含 __WEBUI_PREFIX__ 占位符）+ assets/（app.js/style.css/favicon.svg）
```

## 核心数据模型

```go
type Repository struct {
    Name        string        `json:"name"`        // 唯一标识 + 存储目录名，创建后不可变
    Description string        `json:"description"`
    Aliases     []string      `json:"aliases"`     // git 访问路径，不含 .git；Name 自动包含其中；**末位为首选展示 ref**（首页展示与 clone 提示，新增别名即成新首选）
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

    Mode string `json:"mode,omitempty"` // ""/"pull"=镜像（默认，老元数据缺省即此值）；"relay"=中转仓库

    // 以下仅 relay 使用
    Refs          []string  `json:"refs,omitempty"`          // 准入/转发 ref 前缀白名单，空=refs/heads/ + refs/tags/
    AllowDelete   *bool     `json:"allowDelete,omitempty"`   // 是否允许删除上游 ref，nil=允许
    LastPush      time.Time `json:"lastPush,omitempty"`      // 最近一次转发结果（与拉取侧 LastSync/LastError 分开）
    LastPushError string    `json:"lastPushError,omitempty"`
    PendingRefs   []string  `json:"pendingRefs,omitempty"`   // 已准入但尚未转发成功的 ref（落盘，重启据此补推）
}
```

- **存储**：`<GitRoot>/<name>.git/`（手工创建）
- **元数据**：`<GitRoot>/<name>.git/pgit.json`（name/aliases/description/createdAt/mirror），SaveMetadata 原子写（tmp+rename）
- **同步日志**：`<GitRoot>/<name>.git/pgit-sync.jsonl`（JSONL 追加写，仅镜像仓库）
- **转发日志**：`<GitRoot>/<name>.git/pgit-relay.jsonl`（JSONL 追加写，仅中转仓库）
- **启动扫描**：遍历 `<GitRoot>/*.git/pgit.json` 重建索引（`byName`/`byAlias` + 唯一性用的 `byRefFolded`）；目录内存在 `pgit.deleted` 标记（软删除遗留）则整目录跳过（不进索引、不参与 ref 唯一性，删标记重启即可恢复）；缺 pgit.json 的旧目录自动迁移补齐（name=目录名、aliases=[目录名]）；ref 冲突时涉及仓库全部禁用（见下）
- **镜像仓库**：Mirror 非 nil 时启动自动注册 SyncManager（SyncInterval>0 时定时同步）；**禁止 push**（HTTP/SSH 入口拦截 receive-pack）——**中转仓库例外**（见下）
- **中转仓库**：`IsRelay()` = `Mirror.Mode == "relay"`。允许 push，但每个 ref 必须先通过准入（`RelayManager.Admit`）：与实时上游基线一致（`old == 上游 oid`）且为快进/新建/允许的删除，否则 ng 拒绝且不更新本地；通过准入且更新成功的 ref 落盘 `PendingRefs` 并异步转发上游（详见「中转仓库（relay）」段）。`PendingRefs` 是「尚未转发成功」的权威集合：`SyncRepository` 以 `ProtectRefs` 保护它们不被 pull 回退，转发成功后按 ref 移除并推进内存基线
- **GitHub 导入的命名与访问**：本地 `Name = {namePrefix}{owner}_{repo}`（owner 不含 `_`，故可逆且账号内唯一），额外 alias `{owner}/{repo}`（AddAlias 追加在末位，即首选展示 ref，首页与 clone 提示直接显示 `{owner}/{repo}`）→ `git clone http://host/{owner}/{repo}.git`；远端取 GitHub `clone_url`（可用 `cloneBase` 覆盖），`description`/默认分支取自 GitHub，Token 仅在仓库为 **private** 时存 `MirrorConfig`（`AuthType=basic`、`Username=x-access-token`），public 仓库匿名同步、不落盘 token（转私后可经 settings 接口补）。同名仓库/alias 冲突一律跳过且不覆盖，单次导入上限 200
- **ref 模型**：`Name` 是唯一标识（单段、创建后不可变、同时是默认 ref 不可删）；alias 是指向该仓库的映射，与 Name **可互换使用**（git 访问路径、管理 API 的 `ref` 参数）
- **ref 唯一性**：`Name ∪ alias` 是全局唯一命名空间，比较**不区分大小写**（解析仍大小写敏感）；建仓拒绝与既有 alias 同名，加别名拒绝与既有 Name/alias 冲突（HTTP 409 `ErrRefConflict`）；删仓库/删别名释放 ref
- **ref 规则（白名单）**：字符 `A-Za-z0-9_-.` + 段分隔 `/`，段首尾必须是字母/数字/下划线；段 ≤64、总长 ≤100、段数 ≤8（Name 固定 1 段）；禁 `.git` 结尾；禁保留字 `api` 及其子树、`{webuiPrefix}` 及其子树、`healthz`、`metrics`（大小写不敏感，避免被更具体路由遮蔽）
- **扫描期冲突**：某 ref 被多个仓库声明时，**涉及冲突的仓库全部不进入索引**（全部不可用，git 与管理 API 都 404），逐条记 ERROR 日志等待人工修元数据；同仓库内重复 ref 记 WARN 去重；元数据缺 Name 时只在内存补齐 ref（不写盘）
- **分支名校验**：与 ref 规则解耦（`ValidateDefaultBranch` 保持宽松），避免收紧 ref 连带限制分支名（`feature/x`、`user@host` 等仍可用于默认分支设置）

## 自研 git 协议层（internal/pgs/git）

纯 Go 实现 git wire protocol v0 服务端（实施取舍见 `CHANGELOG.md`）：

- 协议 v0 only（不广告 v2，客户端自动降级）；启用 sideband-64k（pack 走 ch1，进度走 ch2）
- upload-pack 广告 caps 不含 `multi_ack_detailed`（基本模式 v0 多轮 negotiation）：wants+flush → haves 分批（每批 flush 处回 NAK，不带 flush pkt）→ done → NAK+PACK+flush。HTTP stateless_rpc 下每个 POST 是一次 `ServeUploadPack` 调用：have 批 flush 后请求体 EOF 即 `return`（仅已发 NAK），含 done 的 POST 才发 NAK+PACK+flush；SSH 流式下 have flush 后 continue。
- upload-pack 支持 have 过滤增量 fetch：`CollectReachable` 接收 `haveOids` 可变参数，从 have 出发 BFS 标记排除集，want 可达但 have 也可达的对象不发送；want 全部被 have 覆盖时仅发 NAK+flush 不发 PACK
- push 安全仅 old-oid CAS，不限制 force-push；请求体受 `maxPushBytes` 上限（默认 2GiB）与 `maxConcurrentPacks`（默认 4）约束；receive-pack 日志中通过 `isFastForward` BFS 检测非快进推送并标记 `[force-push]`（仅审计日志，不拒绝）
- **mirror 仓库禁止 push（relay 例外）**：HTTP `gitTransport`（`http.go`）与 SSH `handleSession`（`ssh.go`）在协议入口最外层拦截 `git-receive-pack`（HTTP 同时拦截 `info/refs?service=git-receive-pack` 广告阶段），条件为 `repo.IsMirror() && !repo.IsRelay()`，命中返回 403 / SSH stderr `fatal: mirror repository: push disabled` + exit 1。upload-pack（clone/fetch）不受影响
- **receive-pack 准入 hook**：`ServeReceivePack(repoRoot, in, out, opts...) ([]RefUpdateResult, error)` 接受 `ReceivePackOptions.PreRefs`，在对象落盘后、ref 更新前回调（返回与 updates 等长的 `[]*RefUpdateResult`，nil=放行，非 nil=ng 且该 ref 不更新）。`HandleReceivePack`/`HandleSSHSession` 透传 opts 并返回逐 ref 结果，传输层据此触发中转转发（`RelayManager.OnRefsUpdated`）
- **命令行 `+` 前缀**：`parseUpdateLine` 剥离行首 `+`（git 的 force 标记）。不剥离会把 old oid 解析成 `+<hex>`，CAS 必然失败，使真实客户端的 force push 被误判为冲突拒绝
- **畸形输入加固**：`WalkReachable`/`refsOf`/`isFastForward` 只接受形状合法（40 hex）的非零 oid，`LooseStore.Path` 对非法 oid 返回不存在的路径而非 panic——畸形对象（如 `parent <空>`）与畸形命令行都不再能触发 panic
- receive-pack 空命令列表请求（body 仅 flush-pkt，无 ref 更新、无 packfile）容忍并返回空 report-status（unpack ok + flush-pkt）
- 对象完整性逐对象 SHA1 重算校验，不做可达性检查
- REF_DELTA base 优先在 pack 内查找，fallback 回查 LooseStore（push 时 base 常是仓库已有对象）
- ref 原子性 per-ref（lock file + rename）；packed-refs 只读合并视图，写入只 loose
- 存储策略全 loose（不落盘 pack、不 repack）
- 出向 delta（clone 编码）：单遍编码，仅 blob 配对（size 降序相邻），单层 OFS_DELTA，固定窗口滚动hash；负收益回退（deltaLen ≥ target 一半则退 full）。性能保护：采样收益预检（`deltaPrecheck`）+ 桶扫描限制（单桶 ≤64 position，首个 ≥16 匹配贪心采用，全桶无匹配死亡桶淘汰）
- **push 客户端（出向 receive-pack）**：`PushRemote(remoteURL, repoRoot, specs, auth, opts)`（`push.go`）——广告解析复用 `remote.go`；命令 old 取广告值（CAS），无变化的 ref 记为 no-op；对象用 `WalkReachable(newOids, haves=上游全部 ref oid + 命令 old oid)` 增量计算（本地缺失的上游对象由 `Exists` 自动跳过，故无需先 fetch 上游）；请求体 `io.Pipe` 流式（命令 pkt-line + flush + pack），上游有 `ofs-delta` 用出向 delta 编码、否则退化全量；响应 `report-status` 经 `SidebandReader` 重组后解析；缺 `report-status`/`delete-refs` 能力时分别报错/逐 ref 记 ng；部分 ref 被上游拒绝不算函数错误（看 `PushResult.Failed()`），只有传输/协议失败才返回 error（沿用 fetch 的可重试分类）
- 明确不做：protocol v2 / multi_ack 与 multi_ack_detailed 交互式 ACK 状态机 / shallow / partial clone / thin pack / packfile 落盘 / repack-gc / dumb HTTP / reflog / alternates / push 到 SSH 远端（中转发往上游仅 HTTP(S)）

## 端口多路复用

- 单一 `net.Listener`，Accept 后 `bufio.Reader.Peek(4)`：前缀 `SSH-` → SSH（需 `EnableSSH`），否则 → HTTP。peek 阶段 10s 读超时防空连接。
- `peekConn` 包装 conn，首次 Read 回放 peek 的字节再透传底层。
- HTTP 侧用 `singleConnListener` 包装单连接喂给 `http.Server.Serve`。
- SSH 侧直接 `ssh.NewServerConn(peekedConn, config)`。
- **连接生命周期**：所有 HTTP 连接共用 MuxServer 内置的单个 `http.Server`（超时：ReadHeader 15s / Idle 120s；协议探测 peek 10s）；`MuxServer.Shutdown(ctx)` 停止 Accept、等待活动中请求与 SSH 会话、超时强制断开。`main.go` 的 SIGINT/SIGTERM 走优雅关闭（30s 上限）后再退出。
- 错误分类用哨兵错误（`pgs.ErrRepoNotFound` 等）+ `errors.Is`，不要字符串匹配 `err.Error()`。
- **SSH 认证是全放行桩**（`PasswordCallback`/`PublicKeyCallback` 都返回 nil，且不读用户名）—— 不要假设认证被强制执行。HTTP 侧仅全局 Basic 凭据（明文比对，`httpAuth` 默认 false）。无 per-repo 权限、无 TLS。TLS 不实现（HTTPS 交由外层反向代理）。目标策略与决策见 `docs/security-policy.md`（**仅记录，暂不实施**）。
- **SSH host key**：默认生成 **ed25519** 密钥（PKCS8 PEM 写盘）；旧的 RSA hostkey（PKCS1 + `PRIVATE KEY` Type）兼容解析。RSA signer 自动通告 `rsa-sha2-256/512`，现代 OpenSSH 客户端（≥8.8 默认禁用 ssh-rsa）免额外配置即可连接（历史原因见 `CHANGELOG.md`）。
- **SSH exec 路径解析**：git 客户端 exec 参数形如 `git-upload-pack /alias.git`，alias 解析先 `TrimPrefix("/")` 再 `TrimSuffix(".git")`，与 HTTP alias 一致（不含 `/`）。

## 中转仓库（relay）

以上游为基线、把下游的快进推送转发给上游。与镜像共用远端配置、定时拉取、日志与状态设施，因此 `mirror-status`/`sync-log`/`POST /repos/sync` 对 relay 同样适用。

- **准入（写路径的闸门）**：每个 receive-pack 请求在对象落盘后、ref 更新前由 `PreRefs` 回调 `RelayManager.Admit` 校验；该校验**每次实时 `LsRemote` 校准上游基线**（一次 info/refs，不传对象）：
  - 上游无该 ref 且 `old = 0` → 放行（新分支/新 tag）
  - `old != 上游 oid`（含上游无该 ref 而 `old != 0`）→ `relay: stale base (upstream <ref> is <oid>); fetch and retry`
  - `old == 上游 oid` 且 `new` 是其后代 → 放行；非快进 → `relay: non-fast-forward push rejected`
  - 删除（`new = 0`）：`old == 上游 oid` 且 `allowDelete` 为真（默认）→ 放行，否则拒绝
  - ref 不在白名单（默认 `refs/heads/` + `refs/tags/`）→ 拒绝；上游不可达/认证失败 → 整批拒绝（`upstream base unavailable`）
  - 被拒绝的 ref 不更新本地、不进入转发队列，客户端在 report-status 看到原因
- **转发（异步）**：`OnRefsUpdated` 把成功更新的 ref 并入 `PendingRefs`（落盘）并入队；`RelayManager` 队列语义与 `SyncManager` 对称（容量 1024、worker=`relayMaxConcurrentPushes`、同仓库去重、一轮任务最多 3 轮合并并发 push）。每轮以**本地当前 ref 值**构造命令（本地已无该 ref → 删除命令），`PushRemote` 推送；成功者移出 pending 并推进内存基线，上游明确拒绝（ng）或策略拒绝则移出 pending 并记 `lastPushError`（不再重试，避免无限循环），网络类失败保留 pending 并按 `relayPendingRetryIntervalSec` 退避重试（`<=0` 或负值表示关闭后台重试）。启动时 `Bootstrap` 立即补推残留 pending 并异步校准基线
- **手动与运维**：`RelayNow`（手动转发，先把「本地领先基线」的 ref 纳入 pending）、`AlignToUpstream`（危险出口：本地 refs 强制对齐上游基线并清空 pending，用于上游永久拒绝后的恢复；对象保留在磁盘）
- **日志**：`<GitRoot>/<name>.git/pgit-relay.jsonl`（JSONL，逐次尝试：trigger/耗时/对象/pack/逐 ref 结果）；指标 `pgit_relay_*`（admit/push/refs/pending/queue）
- **拉取保护**：relay 的 `SyncRepository` 传 `FetchOptions.ProtectRefs = PendingRefs`，这些 ref 完全不参与本地更新（其余 ref 仍严格镜像，含上游删除）

## API 路由

管理 API（`/api/v1/`，`HttpAuth=true` 时加 Basic Auth）。**仓库引用一律通过 `ref` 参数传递（仓库名或别名），不占路径段**，因此 `owner/repo` 这类多段别名与含斜杠的别名无需百分号编码：
- `GET /api/v1/`（API 文档 JSON，21 个端点）、`GET /api/v1/repos`（列表）、`POST /api/v1/repos`（创建；form：`name`(必填) + `description`/`defaultBranch` + `mirrorUrl`/`mirrorInterval`/`mirrorAuthType`/`mirrorUsername`/`mirrorPassword`/`mirrorProxy` + `mirrorMode`(`pull`|`relay`) + relay 专用 `relayRefs`（逗号/空白分隔的前缀列表）/`relayAllowDelete`）
- `GET|DELETE /api/v1/repos/info`（详情 / 删除；`ref`；删除需 `confirm` == canonical name。删除是**软删除**：目录内写 `pgit.deleted` 标记 + 注销索引与定时同步，git 数据全部保留；彻底删除手动移除目录，恢复=删标记文件重启）
- `POST|DELETE /api/v1/repos/aliases`（加/删别名；`ref` + `alias`，别名可含斜杠）
- `POST /api/v1/repos/default-branch`（`ref` + `branch`，要求分支已存在）
- `POST /api/v1/repos/settings`（`ref` + description 与镜像配置 `mirrorRemoteUrl`/`mirrorInterval`/`mirrorAuthType`/`mirrorUsername`/`mirrorPassword`/`mirrorProxy` + `mirrorMode`（未提供=保持原模式，避免改描述把 relay 降级）+ relay 专用 `relayRefs`/`relayAllowDelete`（未提供=保留原值，显式提供才覆盖；重置默认需显式传 `refs/heads/, refs/tags/`）；密码留空=保留原值；`lastSync`/`lastError`/`lastPush`/`lastPushError`/`pendingRefs` 属服务运行态，设置更新不覆盖；改 interval 重新注册定时调度）
- `GET /api/v1/repos/tree/{path...}`（`ref` + `treeish`，缺省用仓库默认分支；`path` 是尾随通配，根目录时可省略）
- `GET /api/v1/repos/blob/{path...}`（`ref` + `treeish`，返回 text/plain 原文）
- `GET /api/v1/repos/archive`（`ref` + `treeish`，ZIP 下载）
- `GET /api/v1/repos/commits`（`ref` + `treeish` + `limit`，默认 20）
- `POST /api/v1/repos/sync`（`ref`，手动同步镜像仓库，返回 SyncLogEntry）
- `GET /api/v1/repos/sync-log`（`ref` + `limit`，默认 50，最新在前）
- `GET /api/v1/repos/mirror-status`（`ref`，镜像调度/同步状态：scheduled/intervalSec/queued/syncing/lastSync/lastError/nextScheduled；relay 仓库同样适用）
- `POST /api/v1/repos/relay/push`（`ref`，中转仓库手动转发：先 reconcile「本地领先基线」的 ref 再等待结果，返回本次转发日志条目；非 relay 400，已有任务在跑 409，上游不可达 502）
- `GET /api/v1/repos/relay-status`（`ref`，中转状态：upstream/queued/pushing/pendingRefs/lastPush/lastPushError/baseRefs/baseAt/differ/nextRetry；基线未校准时返回空 differ 并异步校准）
- `GET /api/v1/repos/relay-log`（`ref` + `limit`，默认 50，转发日志最新在前）
- `POST /api/v1/repos/relay/align`（`ref` + `confirm` == canonical name，危险操作：本地 refs 强制对齐上游基线并丢弃 pending）
- `GET /api/v1/github/repos`（发现 GitHub 账号仓库，只读；query：`owner`(必填)/`token`/`apiBase`/`proxy`/`includeForks`/`includeArchived`(默认 true)/`namePrefix`；Token 也可用 `X-Github-Token` 头传，避免进 URL；返回 `localName` 与 `conflict`）
- `POST /api/v1/github/import`（按勾选生成镜像仓库；form：`owner`(必填)、重复的 `repos`(必填，也接受 `owner/repo` 与逗号分隔)、`token`/`apiBase`/`cloneBase`/`proxy`/`namePrefix`/`syncInterval`；返回 `{ok,created,failed,results[]}`，逐仓库独立成败，单次上限 200）

运维端点（**不挂 BasicAuth**，供探针/抓取）：
- `GET /healthz`（gitRoot 可读性 + syncManager 就绪 + 仓库数；异常 503 且 status=degraded）
- `GET /metrics`（Prometheus 文本格式 version=0.0.4）

WebUI（`/{webuiPrefix}/`，默认 `__webui`，受 `HttpAuth` 鉴权）：
- `GET /` → 302 重定向至 `/{webuiPrefix}/`
- `GET /{webuiPrefix}` 或 `GET /{webuiPrefix}/*` → `serveWebUI`：embed/磁盘静态资源 + SPA fallback（非文件请求回退 index.html）
- index.html 含 `__WEBUI_PREFIX__` 占位符，per-request 替换为实际前缀注入 `<base>` 标签
- 前端 History API 路由（非 hash，与 API 同构）：`/` 仓库列表、`/repo/info?ref=<name|alias>` 详情、`/repo/tree/{path...}?ref=&treeish=` 文件树、`/import` GitHub 导入页（账号表单 → 加载仓库 → 勾选 → 创建 + 结果）、`/api` API 文档页；旧的 `/repo/{name}` 形态已移除（不做兼容）；页面内导航统一使用 canonical name（别名只在入口解析）

Git 传输（`/{alias}.git/`，alias 可含斜杠，受 `HttpAuth` 鉴权）：
- `GET /{alias}.git/info/refs`、`POST /{alias}.git/git-{command}`

> 路由用标准库 `http.ServeMux`（Go 1.22+ 模式路由，无第三方依赖）：`/api/v1/` 与 `/{webuiPrefix}/` 以精确模式注册，`/{alias}.git/` 走 `HandleFunc("/", h.gitTransport)` 兜底；ServeMux 的「更具体模式优先」保证精确路由压过 `/` 兜底。管理 API 只有 `tree`/`blob` 保留尾随通配参数 `{path...}`（`r.PathValue("path")`，根目录时两种模式都注册以避免尾斜杠陷阱），仓库引用与 `treeish` 都是普通参数，用 `r.FormValue` 读取（查询串与表单体都接受）。所有参数值**已解码**——不要再做 `url.QueryUnescape`。`webuiPrefix` 默认 `__webui`，可配置为多段（如 `custom/ui`）。未注册方法返回 405（stdlib 语义）。

## 配置与运行

- 生成默认配置：`pgit -d > config.json`；运行：`pgit -c config.json`。无配置以 `ConfigError` 退出。
- 热加载：向进程发 `SIGHUP` 重读配置文件。可热加载字段：`logLevel`/`logFormat`/`maxPushBytes`/`maxConcurrentPacks`/`credentials`/`mirrorStallTimeoutSec`/`mirrorRetryAttempts`/`mirrorRetryBaseDelaySec`/`mirrorMaxConcurrentSyncs`/`relayMaxConcurrentPushes`/`relayPendingRetryIntervalSec`；需重启字段：`listen`/`enableSSH`/`gitRoot`/`httpAuth`/`sshAuthType`/`sshHostKey`/`webuiPrefix`/`webuiAssets`（改动被忽略并记 WARN）。配置非法时拒绝且不半应用。
- 导出 WebUI 资源：`pgit -w ./webui`（将 embed 的 web/ 写到磁盘，可自定义修改后通过 `webuiAssets` 加载）。
- 配置字段：`listen`（单一监听地址，默认 `0.0.0.0:3000`）、`enableSSH`、`gitRoot`、`httpAuth`、`credentials`、`sshHostKey`/`sshPublicKey`、`sshAuthType`、`webuiPrefix`（默认 `__webui`）、`webuiAssets`（默认空=用 embed，非空=从磁盘目录读）、`logLevel`（`debug`(兼容旧 `detail`)/`info`/`warn`/`error`，空=`info`）、`logFormat`（`text`/`json`）、`maxPushBytes`（默认 0=2GiB）、`maxConcurrentPacks`（默认 0=4）、`mirrorStallTimeoutSec`（默认 0=120）、`mirrorRetryAttempts`（默认 0=3）、`mirrorRetryBaseDelaySec`（默认 0=1）、`mirrorMaxConcurrentSyncs`（镜像同步任务并发度，默认 0=5）、`relayMaxConcurrentPushes`（中转转发任务并发度，默认 0=5）、`relayPendingRetryIntervalSec`（待转发 ref 的后台重试间隔秒数，0=默认 60，负值=关闭后台重试）。中转转发的远端超时/重试复用 `mirrorStallTimeoutSec`/`mirrorRetryAttempts`/`mirrorRetryBaseDelaySec`。无分离端口字段。
- `webuiPrefix` 校验：非空、不为 `api`、不含 `..`；可含多段斜杠（如 `custom/ui`）。
- `webuiAssets` 校验：非空时目录必须存在且可访问。
- SSH host key 缺失时自动生成到配置路径。

## 运行时布局

- `GitRoot` 默认 `./repo`（gitignored）。`repo/`、根目录 `pgit` 二进制、`config.json`（含凭证）均被 gitignore。
- WebUI 资源源码 `internal/pgs/server/web/` 进 git（embed 要求源码树可见）；运行时从 embed 或 `webuiAssets` 指定磁盘目录读取。

## 测试与质量

- `internal/pgs`：`errors.go` 哨兵错误；`naming_test.go`（ref 白名单矩阵（含保留字/长度/段数）、建仓撞别名/大小写变体、加别名撞 Name 与别名、删仓/删别名释放 ref、扫描冲突涉及仓库全部禁用、元数据缺 Name 时内存补齐且不写盘、同仓库内重复 ref 去重、分支名规则解耦）；`hardening2_test.go`（哨兵错误/InitBare 回滚/权限位）、`softdelete_test.go`（软删除：标记文件/数据保留/索引注销/同名重建被拒并提示、重扫跳过、删标记恢复、无标记遗留目录报 already exists）、`log_test.go`（级别/格式解析与标准库 log 重定向）、`metrics_test.go`（文本格式/标签转义/并发采集）、`config_hotreload_test.go`（热加载生效/需重启回报/非法拒绝/并发无竞态）、`concurrency_test.go`、`repository_test.go`（InitBare 与 pgit.json/自定义默认分支、Manager 双索引与扫描恢复、alias 增删与校验、SetDefaultBranch、CreateMirrorRepository、MirrorBackwardCompat、URL 校验）、`repository_browse_test.go`（Tree/Blob/Archive/ForEachRef 端到端，构造 loose 对象）、`concurrency_test.go`（Manager 并发读写无崩溃、快照隔离、sync 注册回归、sync 与设置更新并发）、`sync_log_test.go`、`sync_manager_test.go`、`sync_queue_test.go`（排队去重/队列关闭快速失败/日志字段）、`task_queue_test.go`（并发度实测、SetWorkers 收缩、满队列丢弃、Stop 语义、panic 恢复）、`task_test.go`（约 6 秒）。
- `internal/pgs`：`relay_test.go`（ACceptsRef/AllowsDelete/IsRelay 语义、mode 与 ref 白名单校验、非 relay 清空 relay 字段、创建后 pgit.json 持久化 mode 并重启恢复、设置更新不覆盖运行态、Snapshot 深拷贝、**准入矩阵**（新分支/tag/快进/stale base/上游无该 ref 而 old!=0/非快进/白名单外/删除与 allowDelete=false/上游不可达））、`relay_forward_test.go`（下游用 `git.PushRemote` 走完整 gate 链路：接受并异步转发上游 + 对象完整性 + pending 清空 + 日志 + status；stale base 端到端拒绝且本地不被污染；删除与 tag 转发；上游 5xx 保留 pending 后 `RelayNow` 恢复；上游 ng 丢弃 pending 并记错误；`RelayNow` reconcile 本地领先；`Bootstrap` 补推；pull 的 ProtectRefs 保护 pending）、`github_test.go`（假 GitHub API：分页 Link、本人 `/user/repos`、用户→组织回退、fork/archived 过滤、限流/未找到/无效 Token 分类、单仓库兜底）、`github_import_test.go`（命名与 alias、Token 落盘、default_branch、二次导入冲突、与非镜像/alias 冲突、cloneBase、校验与上限、并发导入只成功一次、列表缺项兜底、冲突标注）。
- `internal/pgs/server`：`api_ref_test.go`（按 name 与多段别名访问 info/tree/blob/commits/archive 结果等价且 Content-Type 正确、写操作（settings/default-branch/别名增删）按别名寻址、删除 confirm 必须等于 canonical name、未知 ref 404 与缺 ref 400、树/文件路径通配与 root blob 缺 path 的边界）、`router_test.go`（路由表命中与 405/404 语义、缺 ref 400）、`mux_test.go`（并发连接/关闭回收连接/Shutdown 幂等与等待进行中请求/Serve 返回）、`health_test.go`（含 relayQueue 检查）、`limit_test.go`、`relay_api_test.go`（relay 创建/设置/状态/日志/对齐端点与错误码、HTTP push 端到端转发上游、mirror 仍 403）、`ssh_test.go` 走真实 TCP + x/crypto 客户端（upload-pack clone 全量交换验证 pack 对象、receive-pack push 验证 ref+loose 落盘、mirror 仓库 push 拒绝 stderr、relay 仓库 SSH push 过准入 gate：stale base 拒绝+新分支放行并异步转发上游），无需 git/ssh 二进制；`TestSSHClonePushE2E` 需 `PGIT_E2E=1` + git/ssh 二进制。
- `internal/pgs/git`：`push_test.go`（假上游=pgit 自身 receive-pack：新建/更新/删除/tag、up-to-date no-op、上游 ng 解析、force 前缀、sideband 关闭与 ofs-delta 编码两条路径、缺 report-status/delete-refs、401/403/404/5xx 分类与重试、LsRemote 视图与 peeled 行跳过）、`lsremote.go`/`push.go` 相关用例、`loose_test`/`store_test`（内存 ObjectStore 驱动浏览 API/可达性/REF_DELTA 回查）/`stream_test`（流式解码内存对比、WalkReachable 只 Stat、单遍 encodePack 等价性）/`hardening_test`（畸形输入回归 + fuzz）/`delta_test`/`pack_test`/`refs_test`/`reach_test`/`browse_test`/`protocol_test`/`fetch_test`/`e2e_test`，覆盖 delta 应用与生成 roundtrip、deltaPrecheck 预检、桶扫描限制、pack 编解码（与真实 git pack、index-pack 互验）、ofs-delta 回环、ref CAS/symref/packed-refs、可达性 BFS 与 have 差量过滤、treeIsh/tree/blob/ForEachRefs、v0 状态机 + sideband、增量 fetch（have flush/多 POST/无 done 等）、fetch 客户端（initial/incremental/up-to-date/empty/basic auth/ref 删除/ACK 响应，httptest + 自身协议当远程，无需外部 git）；e2e 集成需 `PGIT_E2E=1`。`go test ./...` 通过。
- 无 linter/formatter/CI 配置。用 `go vet ./...` 和 `go build` 验证。路由层无第三方依赖（`http.ServeMux`）。
- 测试中调用真实 `git` 时必须注入 `-c commit.gpgsign=false`（`internal/pgs/git/gitcmd_test.go` 的 `newGitCmd`、`internal/pgs/testmain_test.go` 调低重试次数）（见 `internal/pgs/git/gitcmd_test.go` 的 `newGitCmd`）：否则用户的全局 `commit.gpgsign=true` 会让 `git commit` 等待 GPG 口令直至超时。

## 工作流

- 默认分支 `master`（稳定）；`develop` 为重构分支。远程 `https://github.com/LaoQi/pgit.git`。
- 提交较随意（多为 `WIP`）；不强制 conventional-commits。

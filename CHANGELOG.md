# Changelog

pgit 变更历史。`AGENTS.md` 只描述**当前**架构、约束与用法；变更过程、缺陷修复、
性能调优与历史决策收录于此。条目按时间倒序，括注提交短 hash。

## 2026-09-24

**refactor(server): 阶段 4 接入层生命周期**（408f394）
- 共享 `http.Server` + 连接通道 listener：`MuxServer` 内置单个 server
  （`ReadHeaderTimeout` 15s、`IdleTimeout` 120s），由 `connChanListener` 投递探测后的连接，
  取代「每连接新建 http.Server + 共享字段」；`connChanListener.Close` 关闭投递通道使
  `Accept` 立即返回 —— 修复 `singleConnListener.Close` 不关底层连接导致的 keep-alive 连接滞留
- 优雅关闭：`MuxServer.Shutdown(ctx)`（停止 Accept → `http.Server.Shutdown` 等活动中请求 →
  等 SSH 会话/超时强断 → 关投递通道，幂等）；`main.go` 的 SIGINT/SIGTERM 改走
  `Shutdown(30s)` + `SyncMgr.Stop()`，替换 `os.Exit` 硬切
- 哨兵错误：`ErrRepoNotFound`/`ErrAliasNotFound`/`ErrNotMirror`/`ErrSyncInProgress`/`ErrRepoExist`，
  HTTP 层改用 `errors.Is` 判定状态码（不再字符串匹配错误信息）
- git 传输路径改 `LastIndex(".git/")` 切分，修复含 `.git/` 段的 alias 不可访问
- `InitBare` 失败回滚半成品目录；权限位收紧为目录 `0o750` / 文件 `0o640`
- 测试加固：真实 git 调用统一注入 `-c commit.gpgsign=false`
  （此前受用户全局 GPG 配置影响，测试内 `git commit` 会等待口令直至超时失败）


**feat(ops): 阶段 5-3/5-4 配置热加载与镜像状态**（875c80d）
- SIGHUP 热加载：`logLevel`/`logFormat`/`maxPushBytes`/`maxConcurrentPacks`/`credentials` 即时生效；
  `listen`/`enableSSH`/`gitRoot`/`httpAuth`/`sshAuthType`/`sshHostKey`/`webuiPrefix`/`webuiAssets`
  需重启（改动被忽略并记录告警）；配置非法时拒绝且不半应用
- `Setting` 加 `RWMutex` + `SettingView` 只读快照 + `CredentialsCopy`/`WebUIConf`；
  `basicAuth` 每请求取凭据副本 → 热加载后新凭据立即生效
- 修复 `reloadLocked` 内调用取读锁方法导致的 RWMutex 自死锁
- `SyncManager` 新增 `Status`/`Statuses`（调度状态/间隔/同步中/最近错误/下次触发）；
  `Register` 间隔变更时重建调度器（不再沿用旧 ticker）；新增
  `GET /api/v1/repos/{name}/mirror-status`

**feat(metrics): 阶段 5-2 健康检查与 Prometheus 指标**（16d7f50）
- 新增 `/healthz`（gitRoot/syncManager/仓库数，异常 503 + degraded）与 `/metrics`
  （Prometheus 文本格式，无第三方依赖）；两者不挂 BasicAuth
- 采集：HTTP 请求（方法/状态/结果 + 累计耗时）、git 操作结果、pack 字节与进行中数、
  镜像同步（次数/对象/耗时/最近成功时间戳）、仓库总数
- `buildRouter` 拆为两个 chi Group（探针组无鉴权），满足 chi 中间件声明顺序约束

**feat(log): 阶段 5-1 结构化日志**（6647da7）
- `SetupLogging` 初始化 slog（text/json）并把标准库 log 重定向到同一 handler，
  既有 `log.Printf` 无需逐个改造即结构化
- 级别 `debug`(兼容旧 `detail`)/`info`/`warn`/`error`；新增 `logFormat`；旧值空与 `off` 视为 `info`
- HTTP `X-Request-Id` 透传/生成（上下文 + 响应头）；git 逐对象日志降为 DEBUG
- 清理死代码 `planPackEntries`/`packEntry`


**docs: 安全策略单独记录（暂不实施）**（b2fd288、后续调整 b2fd288+）
- 决策 C 调整为：**pgit 不实现 TLS**（不引入 `tlsCert`/`tlsKey`/ACME），HTTPS 交由外层反向代理终止；
  该条已从实施顺序移入「明确不做」
- 新增 `docs/security-policy.md`：现状、目标策略、三条已定决策（SSH 忽略用户名只认密钥 /
  `sshAuthType` 默认 none / TLS 仅自带证书）、实施顺序与 URL 兼容性说明
- 阶段 4 范围收敛为接入层生命周期，安全项仅记录


**perf(git): 阶段 3-3 clone 流式化**（40ba493）
- `ObjectStore` 新增 `Stat(oid)`（只解压头部取 type/size），新增 `WalkReachable`：可达性遍历只 `Stat`，
  内存与仓库体积无关，blob 不再被解压
- upload-pack 改**单遍编码** `encodePack`：非 blob 按 BFS 顺序、blob 按 size 降序两两配对（base 先写），
  每个对象只读一次
- 实测（vistty/1537 对象）：峰值 RSS 150.4MB → 110.1MB（**−27%**），client 1.94s → 2.49s（+22%）；
  产物 HEAD 与 .git 体积完全一致。时间代价源于对象不再常驻内存、编码时重新解压

**refactor(git)!: 阶段 3-2 push/fetch 流式化与资源上限**（b14e4ca）
- `PackDecoder` 重写为流式：逐对象解析即交出（`DecodeTo` 落盘 / `Decode` 收集），
  不再 `io.ReadAll` + 全量驻留；trailer SHA1 边读边校验（依赖 `io.ByteReader` 精确消费定位 offset）
- OFS_DELTA 改为「偏移 → oid → Store 回读」；`DecodeTo` 与 `Decode` 对同一 pack 结果一致
- receive-pack 流式落盘；pack 被拒（超限/损坏）回 `report-status`（unpack error + ng），
  客户端立刻得到明确错误而非挂断
- fetch：sideband 经 `io.Pipe` 流式喂入解码器；非 sideband 直接读（peek 判定 up-to-date）
- 新增 `Setting.maxPushBytes`（默认 2GiB）与 `Setting.maxConcurrentPacks`（默认 4）；
  HTTP 侧用 `MaxBytesReader` + pack 传输信号量（排队 / ctx 取消返回 503），git 侧 `SetMaxReceivePackBytes` 兜底
- 实测：8.4MiB 未压缩内容峰值堆 18.3MB → 1.09MB

**fix(git): 阶段 3-1 解析层边界加固**（40a31de）
- `readObject`/`readOfsDelta`/`ApplyDelta`/`PktReader` 补边界检查（此前畸形输入可 panic）；
  `ApplyDelta` 增 `tgtSize` 上限 1GiB 且预分配封顶 4MiB
- 新增 `hardening_test.go`（评估文档附录 A-2/A-3 转回归）+ 4 个 fuzz 目标（`ApplyDelta`/`PackDecoder`/`readOfsDelta`/`PktReader`）

**refactor(arch): 阶段 2 存储抽象与依赖注入**（0f25c29）
- 2-1 存储接口化：新增 `git.ObjectStore`（`Read/Exists/Write`）与 `git.NewObjectStore(repoRoot)`；
  `CollectReachable`/`TreeAt`/`BlobAt`/`CommitLog`/`derefToTree`/`isFastForward` 签名收敛到接口；
  `PackDecoder` 的 `ObjectReader` 收编为 `ObjectStore`。新增 `git/store_test.go`（内存 ObjectStore
  驱动浏览 API/可达性遍历/REF_DELTA 回查），证明协议与浏览层已与松散对象文件存储解耦
- 2-2 `Repository` 自包含：新增 `root` 字段（`json:"-"`），`Root()`/`Path()` 不再依赖全局 `GitRoot`；
  `InitBare` 显式接收 `gitRoot`；manager 统一走 `Config.GitRoot` 并注入 `root`；`SSHHandler` 去掉自带
  `GitRoot`，改用 `repo.Path()`
- 2-3 依赖注入：去掉包级 `pgs.SyncMgr`/`InitSyncManager`，改为 `NewSyncManager(manager)`，
  `HTTPHandler` 持有 `Sync` 实例；`git.logLevel` 改为 `atomic.Int32`


**refactor(concurrency): 阶段 1 并发地基**（8769cb0）
- `RepositoriesManager` 引入 `RWMutex`，对外方法返回 `Repository` 快照（`Snapshot()`），元数据落盘统一在锁内
  - 修复：并发 map 读写导致的 `fatal error: concurrent map read and map write`（进程级退出，recover 拦不住）
  - 修复：`Aliases`/`Mirror` 字段竞态与 `SaveMetadata` 全量覆盖导致的元数据丢更新
- `SyncRepository` 改为「锁内取配置快照 → 无锁 fetch → 锁内回写 `LastSync/LastError`」
- `UpdateRepositorySettings` 返回旧 `SyncInterval`（供调用方判断是否重建调度），且不再修改调用方入参
- `SyncManager` 重构：修复「手动同步过的镜像仓库此后 Register 静默失效、定时同步永不启动」；
  per-repo `inflight` 判重取代 scheduler 占位；`Stop()` 加 WaitGroup 且可重复调用；合并 `doSync`/`SyncNow` 重复逻辑
- `HTTPHandler` 去掉每连接共享的 `server` 字段（其自身即 data race）
- task 系统：`status` 改由 `GetStatus`/`SetStatus` 保护；启动前置 Running 防止重复派发；
  失败任务回调后移除（原实现每秒重复回调且不移除）
- 新增 `internal/pgs/concurrency_test.go`：并发读写、快照隔离、sync 注册回归、sync 与设置更新并发

## 2026-08-19

**perf(git): upload-pack 加速**（ecea7a2）
- 出向 delta 增加收益预检（`deltaPrecheck` 采样命中）：低相似/随机对大 blob 直接走 full，避免白算
- 滚动 hash 桶扫描加限制：单桶 ≤64 position、首个 ≥16 匹配即贪心采用、全桶无匹配则死亡桶淘汰
- packfile 编码改用 zlib BestSpeed（clone 速度优先）；`RawObject.Oid()` lazy 缓存
- 效果：vistty 样例仓库 clone 的 upload-pack 处理耗时 ~21s → ~3s

**fix(ssh): 修复 SSH clone 失败并建立 SSH 测试链路**（df6b349）
- host key：`golang.org/x/crypto` 升级至 v0.55.0，服务端对 RSA signer 自动通告 `rsa-sha2-256/512`（此前仅 SHA-1 `ssh-rsa`），OpenSSH ≥8.8 默认可连，无需 `-o HostKeyAlgorithms=+ssh-rsa`
- SSH exec alias 解析修正：先剥离前导 `/` 再去 `.git`，与 HTTP alias 语义一致
- 新增 `internal/pgs/server/ssh_test.go`（真实 TCP + x/crypto 客户端，无需 git/ssh 二进制）与 `TestSSHClonePushE2E`

## 2026-08-06

**fix: mirror 仓库禁止推送**（8876d26）
- HTTP（含 `info/refs?service=git-receive-pack` 广告阶段）与 SSH 入口最外层拦截 `git-receive-pack`
- `repo.IsMirror()` 为真时 HTTP 返回 403、SSH 输出 `fatal: mirror repository: push disabled` + exit 1；upload-pack（clone/fetch）不受影响

## 2026-07-10

**fix(git): fetch 客户端接受 cgit 基本模式 ACK 响应**（3eab76f）
- `done` 之后首帧除 `NAK`（无共同 commit）外，也接受 `ACK <oid>`（有共同 commit）

**feat: WebUI 镜像代理配置 + 仓库设置编辑**（e981840）
- `POST /api/v1/repos/{name}/settings` 更新 description 与镜像配置；密码留空表示保留原值；修改 interval 重新注册定时调度

**feat: 镜像同步支持 HTTP 代理**（aba9cfd）
- per-repo `mirrorProxy`（配置字段 `Proxy`），URL userinfo 自动用于代理认证

**feat: 镜像仓库功能**（401f2c7）
- 纯 Go fetch 客户端 + `SyncManager` 定时同步 + JSONL 同步日志 + WebUI

## 2026-07-03

**feat(git): upload-pack 详细日志可配置**（eacbe76）
- 配置 `logLevel`：默认空=`off`（仅汇总行），`detail` 逐条输出 want/have/object/delta 配对

## 2026-06-30

**fix(http): smart-http 请求体 gzip 自动解压**（2f28cc8）
- 处理带 `Content-Encoding: gzip` 的请求体

## 2026-06-29

**feat: 请求日志 + force-push 审计标记**（7c397c3）
- 请求日志中间件（方法/路径/状态码/耗时/用户/远程地址）
- receive-pack 通过 `isFastForward` BFS 检测非快进推送并标记 `[force-push]`（仅审计，不拒绝）

**feat: commits 列表 API 与 WebUI 展示**（beb5253）

**fix(git): upload-pack have flush 回 NAK**（0fb5c87）
- 修复 HTTP 增量 fetch 报 `expected ACK/NAK, got '?PACK'`
- NAK 计数与 fetch-pack 客户端 `get_ack` 调用次数自洽（对齐 fetch-pack.c:619-628：`if(retval!=0) flushes++` + `while(flushes)`）

## 2026-06-26

**feat(git): upload-pack have 过滤增量 fetch**（d506310）
- `CollectReachable` 增加 `haveOids` 可变参数，从 have 出发 BFS 构建排除集；want 全被 have 覆盖时仅回 NAK+flush 不发 PACK
- 配套 14 个增量拉取测试

**fix(git): 不广告 multi_ack_detailed**（f42b4ba）
- 修复 fetch 报 `expected ACK/NAK, got '?PACK'`；改为基本模式 v0 多轮 negotiation
- 取舍原因：`multi_ack_detailed` 需实现 `ok_to_give_up` 可达性判断与 `ACK common/ready` 状态机；基本模式 + have flush 回 NAK 已覆盖 HTTP/SSH 场景的增量 fetch

**test(git): NAK 首帧独立验证 + e2e 集成测试**（53b4b10）
- e2e 需 `PGIT_E2E=1`

## 2026-06-25

**feat(git): upload-pack 出向 delta 编码**（d7c35d2）
- 仅 blob 配对、单层 OFS_DELTA；负收益回退 full（deltaLen ≥ target 一半）

**fix: receive-pack REF_DELTA base 回查 LooseStore**（65fa8fe）
- 修复增量 push 报 base not found

**fix: 前端 tree API 请求补全尾部斜杠**（229a982）
- 修复 chi `/*` 路由 404

**fix: 仓库面板与 clone URL 样式**（8d4f908）
- 面板最小宽度提至 400px、clone URL 防换行截断

**feat: 仓库支持指定默认分支**（cce8fdf）
- 新增 `POST /api/v1/repos/{name}/default-branch` 与 WebUI 设置入口

**fix: receive-pack 容忍空命令列表请求**（faf9122）
- 修正 report-status flush

## 2026-06-24

**feat: 内置简易 WebUI**（218e90b）
- embed 嵌入、可导出磁盘自定义，新增 API 文档页

**feat: 浏览 API 去 git 二进制依赖**（9fc5ca5）
- Tree/Blob/Archive/ForEachRef 改为纯 Go 实现

**feat: 自研纯 Go git wire protocol v0 协议层**（8670392）
- clone/push 去 git 二进制依赖
- 取舍：协议 v0 only（不广告 v2，客户端自动降级）；启用 sideband-64k；对象仅 loose 存储（不落盘 pack、不 repack）

**refactor: 重构为 internal/pgs 架构**（e0b70ec）
- 单端口 HTTP/SSH 多路复用、路径映射解耦访问 URL 与存储、移除 Web 前端

## 2019-03 ~ 2020-06（早期）

- 2019-03：项目初始化（repo handler、仓库列表/创建页、HTTP 状态码、LICENSE、readme）
- 2019-04：HTTP 浏览（git ls-tree、README 展示、download zip、分支页面）与 SSH 支持、host key 生成
- 2019-05：settings 序列化与重构、依赖 dep → go modules
- 2019-12、2020-06：`WIP`

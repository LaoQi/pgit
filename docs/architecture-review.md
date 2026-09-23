# pgit 架构评估与改造计划

> 评估日期：2026-09-23　评估对象：`internal/pgs`、`internal/pgs/git`、`internal/pgs/server`、`cmd/pgit`（生产代码约 5.7k 行）。
> 方法：全量阅读核心代码 + 4 组可复现实验（见附录 A）。本文档只记录结论与计划；变更历史见 `CHANGELOG.md`。

## 1. 总体判断

分层清晰：`server`（接入）→ `pgs`(业务/元数据) → `pgs/git`(协议+存储)。协议层纯 Go 自研无第三方依赖，
loose/refs/metadata 一律 tmp+rename 原子写，测试量与生产代码接近 1:1。

问题集中在三处，也是后续扩展的阻力来源：

| 类别 | 表现 | 影响 |
|------|------|------|
| 并发模型 | 全局共享状态（Manager/Settings/Mirror）无锁 | 随并发请求直接崩溃、元数据静默丢失 |
| 资源边界 | 全内存处理、无大小/并发上限、解析无边界检查 | 大仓库不可用、可被单请求打崩 |
| 抽象缺失 | 存储用具体类型硬编码、全局单例、无接口 | 加 pack/gc/多租户/多实例需大范围重构 |

## 2. P0：会直接搞坏服务

### 2.1 RepositoriesManager 无锁 → 并发时进程级崩溃

- 证据：`byName`/`byAlias` 裸 map（`manager.go:22-23`），写点 `addRepository`(`:102`)、`AddAlias`(`:300`)、
  `RemoveAlias`(`:322`)、`DeleteRepository`(`:277`)，读点 `GetByAlias`(`:126`，git 传输路径 `http.go:489`)。
- 实测（附录 A-1）：`-race` 报 data race；**不带 `-race` 时直接 `fatal error: concurrent map read and map write`
  → 进程退出，recover 拦不住**。
- 触发条件：任意 git 传输（push/clone 查找 alias）与管理写成（建仓/删仓/增删 alias）并发。

### 2.2 Repository / MirrorConfig 字段并发读写；元数据整体覆盖

- `repo.Aliases` append(`manager.go:299`) vs `HasAlias`(`repository.go:59`) 实测 race（附录 A-1）。
- `repo.Mirror` 指针被 `updateSettings` 整体替换（`http.go:443`、`manager.go:235`），同步 goroutine 同时写
  `LastSync/LastError`（`manager.go:259-265`）。
- `SaveMetadata` 全量覆盖写、无锁、无 fsync（`repository.go:46-57`）→ 并发下后写者覆盖前写者，设置改动或同步状态静默丢失。

### 2.3 解析层对畸形输入 panic（阶段 3-1 已修，40a31de）

- 实测（附录 A-2/A-3）：`pack_decode.go:86` 与 `delta.go:30-57` 存在越界 panic
  （`index out of range [13] with length 13` / `[3] with length 3`）；攻击者可自算 SHA1 trailer 绕过校验到达此处。
- 后果：HTTP 侧被 net/http recover 成 500，SSH 侧被 mux recover 成断连——服务不死，但请求被毁、可被用作 DoS 与日志噪音。
- 同类风险：`readObject`/`readOfsDelta` 缺 `pos < len` 检查；`ApplyDelta` 按不可信 `tgtSize` 预分配。
- **阶段 3-1 已修**：补齐边界检查 + `tgtSize` 上限 + 预分配封顶；附录 A-2/A-3 转为回归测试，另加 4 个 fuzz 目标。

### 2.4 资源无边界（阶段 3-2/3-3 已修主体）

| 位置 | 阶段 3 之前 | 现状 |
|------|------|------|
| push 请求体 | `io.ReadAll(in)` 读整个 pack | 流式 `DecodeTo` 逐对象落盘；`maxPushBytes`（默认 2GiB）上限 |
| pack 解码 | `io.ReadAll` + 全部对象驻留 | 流式解码，峰值 ≈ 单个最大对象 + delta base |
| clone 出向 | `CollectReachable` 返回全部对象（含内容） | `WalkReachable` 只读头部 + 单遍编码；峰值 RSS −27% |
| 并发 | 每连接一 goroutine，无上限 | `maxConcurrentPacks`（默认 4）信号量，排队 / 503 |
| **仍存** | — | 每连接仍一 goroutine（无全局连接上限）；blob 浏览仍全量进内存；无连接/请求级超时 |

本地样例 `repo/vistty.git` 仅 36MB/1540 对象，故当前无感；GB 级仓库必然失败。

### 2.6 task 系统状态竞态与重复派发（阶段 1 `-race` 新发现）

`Task.Status` 由执行 goroutine 写、调度循环读（`task_manager.go:27` vs `task.go:49`），实测 race；
且 `TSOpen` 任务在派发时未先置 Running → 下一轮重复派发；`TSFailed` 分支不移除任务 → 每秒重复回调。
阶段 1 已修（`GetStatus/SetStatus` + 派发前置 Running + 终态移除），并加调度完结断言。

### 2.5 认证/授权几乎为零

`PasswordCallback`/`PublicKeyCallback` 一律放行（`ssh.go:86-92`）；HTTP 单一全局 Basic 凭据、明文比对
（`http.go:573-589`）；无 per-repo 权限、无 token、无 TLS（`main.go:85` 仅 `net.Listen`）。

## 3. P1：架构级限制（扩展主要阻力）

1. ~~**存储层无接口**：`*git.LooseStore` 具体类型散落业务层。~~ → **阶段 2 已修**：新增 `git.ObjectStore`
   接口与 `git.NewObjectStore(repoRoot)`，协议层/浏览层的对象读写全部走接口（`git/store_test.go` 用内存实现验证）。
   加 packfile、对象缓存、gc、配额现在只需新增一个 `ObjectStore` 实现。
2. **全 loose、无 pack/gc/缓存**：每对象一次 open+zlib；clone 需遍历解压全仓；删除仓库才回收。
3. **协议能力与实现不符**：`uploadPackCaps` 声明 `thin-pack include-tag`（`protocol.go:28`）但无实现；
   出向 delta 无条件发 OFS_DELTA，`clientCaps` 仅用于 sideband 判断（`protocol.go:277`）；
   只支持 v0，无 v2（无 `ls-refs`/`fetch` 分流，无 shallow/partial 基础）。
4. **SyncManager 生命周期缺陷**（实测，附录 A-4）：`SyncNow` 为未注册仓库塞入无 stop channel 的 scheduler
   （`sync_manager.go:140`），`Register` 见同名键即早退（`:35-38`）→「先手动同步、后配 interval」的镜像仓库定时同步静默失效。
   另有：`Stop()` 无 WaitGroup 等待（`:185-194`）；`doSync` 与 `SyncNow` 约 60 行重复；goroutine 直读
   `repo.Mirror.SyncInterval`（与改设置并发 → race + ticker 旧值）。
5. **fetch 客户端整体 5 分钟超时**（`fetch.go:38`，`http.Client.Timeout` 覆盖 body）→ 大仓库镜像必失败；
   无重试/退避；仅 HTTP(S)。
6. **全局单例与包级可变状态**：~~`Settings`、`GitRoot`、`ReposManager`、`SyncMgr`、`git.logLevel`~~
   → **阶段 2 部分修复**：`SyncMgr` 已改为注入（`NewSyncManager(manager)`）；`Repository` 自包含 root，
   `Path()` 不再读全局；`git.logLevel` 改 `atomic.Int32`。仍存：`pgs.GitRoot`（仅作兼容兜底）、
   `pgs.Settings`、`pgs.ReposManager` 全局（阶段 4/5 处理配置热加载与多实例）。
7. **接入层生命周期缺失**：`h.server` 每连接并发赋值（`http.go:74-77`，自身即 race）；每连接新建 `http.Server`；
   `singleConnListener.Close()` 不关底层连接（`http.go:91`）；无 `ReadHeaderTimeout/IdleTimeout`；
   `requestLogger` 包装的 ResponseWriter 无 `Flusher/ReaderFrom`；退出用 `os.Exit`（`main.go:111`）无优雅关闭。
8. **可观测性空白**：无 `/healthz`、无 metrics、日志为 `log.Printf` 拼串；`logLevel=detail` 逐对象打日志
   （`protocol.go:255`）；ROADMAP 的 mirror webhook 未实现。

## 4. P2：技术债清单

- 权限位过宽：`os.ModePerm`(`repository.go:140`)、`0o777`(`loose.go:82`、`refs.go:232,237`)。
- `parsePackedRefs` 每次调用全量重解析，更新 N 个 ref 时每 ref 一次（`refs.go:176,202,244`）→ O(n²) 磁盘读；
  `List()` 的 `filepath.Walk` 同理。
- `ForEachRefs` 每请求读+解析每个 ref 的对象（`browse.go:279`），`GET /repos/{name}` 每次都做，无缓存无分页；
  `listRepos` 调 `List()` 两次（`http.go:113-114`）。
- 错误分类靠 `strings.Contains(err.Error(), "not exist")`（`http.go:373-378`）。
- git URL 用**首个** `.git/` 切分（`http.go:477`）→ 含 `.git/` 段的 alias 不可访问，应改 `LastIndex`。
- `InitBare` 用 `os.Mkdir` 失败无回滚（`repository.go:140-162`），残留半成品目录被扫描静默跳过（`manager.go:84-87`）。
- `apidocs.go` 手写静态 JSON，与 chi 路由双份维护。
- 无 CI/lint/fuzz；测试零并发、零畸形输入用例（本次 race 与两个 panic 全在盲区）；e2e 需 `PGIT_E2E=1`。
- 依赖 `chi v4.0.2`（2019）。

## 5. 分阶段改造计划

每阶段独立可交付、独立提交；验收统一为 `go build ./... && go vet ./... && go test -race ./...`（含新增回归测试）。

### 阶段 1：并发地基（已完成，8769cb0）

- 目标：消除 race 与 fatal map 崩溃；元数据写入串行化；sync 注册状态自洽。
- 内容：
  1. `RepositoriesManager` 引入 `sync.RWMutex`，读路径（`List`/`GetRepository`/`GetByAlias`/`RepositoryExist`）走 `RLock`，
     写路径（CRUD/alias/设置/同步状态）走 `Lock`。
  2. 元数据变更集中在 manager 加锁方法内完成（含 `SaveMetadata`），避免「改动 + 覆盖写」交错。
  3. `SyncRepository` 的 `LastSync/LastError` 更新走锁内方法。
  4. `SyncManager`：修 `Register` 早退；scheduler 与 interval 纳入锁模型（goroutine 用局部快照）；
     `Stop()` 加 WaitGroup；合并 `doSync`/`SyncNow` 重复逻辑。
  5. `HTTPHandler.server` 改局部变量（每连接一个 Server），消除自身 race。
  6. 新增并发回归测试（3 个）覆盖上述竞态。
- 额外：修复 task 系统状态竞态/重复派发/失败任务重复回调（附录 A-5，`-race` 发现）。
- 验收：新增测试在 `-race` 下通过；`fatal error: concurrent map read and map write` 不再可复现（附录 A-1 场景）。
  `go build ./... && go vet ./... && go test -race ./...` 全绿。

### 阶段 2：存储抽象与去全局化（已完成，0f25c29）

- 2-1 抽 `ObjectStore`（`Read/Exists/Write`）+ `NewObjectStore(repoRoot)`，`LooseStore` 为首个实现，
  `PackDecoder` 复用同一接口；附内存实现测试证明解耦。
- 2-2 `Repository` 自包含 root（`Root()/Path()` 去全局），`InitBare` 显式接收 `gitRoot`，
  `SSHHandler` 去掉重复的 `GitRoot`。
- 2-3 `SyncManager` 依赖注入（`NewSyncManager(manager)`），`HTTPHandler` 持有实例；
  `git.logLevel` 改 `atomic.Int32`。
- 验收：`go build/vet` + `go test -race ./...` 全绿；真实实例端到端验证 HTTP clone/push、
  SSH clone、mirror 同步、mirror 拒绝 push、WebUI/tree/commits 均正常。

### 阶段 3：流式化与资源边界（已完成，40a31de / b14e4ca / 40ba493）

- 3-1（40a31de）：解析层边界检查 + `tgtSize`/预分配上限；A-2/A-3 转回归测试；4 个 fuzz 目标。
- 3-2（b14e4ca）：`PackDecoder` 流式化（`DecodeTo`）、receive-pack 流式落盘 + 拒绝回 report-status、
  fetch sideband 流式、`maxPushBytes`/`maxConcurrentPacks` 上限与信号量。
- 3-3（40ba493）：`ObjectStore.Stat` + `WalkReachable`（只读头部）+ 单遍 `encodePack`；
  clone 峰值 RSS −27%（产物与旧实现字节级一致）。
- 已取舍得证：单遍编码避免重复解压，代价（+22% 时间）来自对象不再常驻内存。

### 阶段 4：接入层安全与生命周期

- TLS + 用户/令牌/per-repo 读写权限；SSH authorized_keys 表替代放行桩；
- 修 `singleConnListener`（关闭底层连接）、加 `ReadHeaderTimeout/IdleTimeout`；
- 优雅关闭：`http.Server.Shutdown` + `SyncMgr` 等待，替换 `os.Exit`。

### 阶段 5：运维与可观测性

- `/healthz` + Prometheus 指标；`slog` 结构化日志（含请求 ID），detail 级日志降噪；
- 配置热加载（含 `gitRoot` 变更语义明确化）；补齐 P2 中 SyncManager 相关收尾。

### 阶段 6：功能扩展

- webhook（push / mirror 完成事件）；gc/repack 后台任务；
- 协议 v2（`ls-refs` + `fetch`，shallow/filter 前置）；API 分页与 ETag；apidocs 由路由生成。

## 附录 A：本次评估的复现方法

A-1 Manager 并发读写（阶段 1 前，`-race` 与不带 `-race` 各跑一次）：

```go
// internal/pgs/zz_audit_test.go（临时验证用，已删除）
func TestAuditManagerRace(t *testing.T) {
	dir := t.TempDir()
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	ReposManager.CreateRepository("r1", "", "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ { // 读：模拟 git 传输/列表请求
		wg.Add(1)
		go func() { defer wg.Done()
			for j := 0; j < 300; j++ { ReposManager.List(); ReposManager.GetByAlias("r1") }
		}()
	}
	for i := 0; i < 4; i++ { // 写：模拟管理 API
		wg.Add(1)
		go func(i int) { defer wg.Done()
			for j := 0; j < 60; j++ { ReposManager.AddAlias("r1", fmt.Sprintf("a%d-%d", i, j)) }
		}(i)
	}
	wg.Wait()
}
```

A-2 畸形 pack（size varint 截断）：

```go
body := []byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01")
body = append(body, 0x80) // type 首字节带续位，后续字节缺失
h := sha1.Sum(body)
pack := append(append([]byte{}, body...), h[:]...)
NewPackDecoder(bytes.NewReader(pack)).Decode() // panic: index out of range
```

A-3 畸形 delta（copy 指令字段截断）：

```go
ApplyDelta(nil, []byte{0x00, 0x01, 0x81}) // panic: index out of range [3] with length 3
```

A-4 手动同步后注册定时同步失效（阶段 1 前的 bug，已修并加回归测试 `TestSyncRegisterAfterManualSync`）：

```go
// 镜像仓库 m1（RemoteURL 指向不可达地址即可，SyncNow 失败也会占用 mirrors 槽位）
SyncMgr.SyncNow("m1")      // scheduler present=true stop=false
SyncMgr.Register(repo)     // interval=60，应启动 ticker
// 结果：map 已有同名键 → Register 早退，stop 仍为 nil（定时同步永不启动）
```

A-5 task 系统竞态（阶段 1 由 `go test -race` 发现）：

```
WARNING: DATA RACE
  Read at ... by goroutine 10: (*TaskManager).Run()   task_manager.go:27
  Previous write ... : (*Task).Process()              task.go:49
```

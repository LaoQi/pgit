# 中转仓库（relay）设计

> 目标：一个仓库同时是「上游的镜像」和「下游推送的转发站」——
> 下游 push 进来后自动推送到上游 GitHub，且**永远不与上游冲突**。
> 相关实现：`internal/pgs/relay_config.go`、`relay_manager.go`、`relay_base.go`、`relay_log.go`、
> `internal/pgs/git/push.go`、`lsremote.go`、`remote.go`。

## 1. 语义与不变量

| 类型 | 方向 | 数据权威 | 定时任务 |
|---|---|---|---|
| 普通仓库 | 下游 push | 本地 | — |
| 镜像仓库（`mode` 空 / `pull`） | 上游 → 本地 | 上游 | 定时 fetch |
| **中转仓库（`mode = relay`）** | 上游 → 本地（基线）+ 下游 → 上游（转发） | 上游基线 | 定时 fetch + 待转发重试 |

核心不变量：

> **本地 ref 要么等于上游基线，要么处于「已通过准入、尚未转发成功」的 pending 状态。**

因此转发必然是一次快进推送，不需要 force、不需要合并、不会产生分叉。代价是
**下游必须先 fetch 再 push**：本地落后于上游时，push 会被明确拒绝（`stale base`）。

中转仓库与镜像仓库共用同一份远端配置（`MirrorConfig`：URL/认证/代理/pull 间隔/日志），
因此定时拉取、`POST /api/v1/repos/sync`、sync-log、mirror-status 对 relay 天然可用。

## 2. Push 准入（核心）

接收下游 push 时，在**对象已落盘、ref 更新之前**插入准入校验
（`git.ReceivePackOptions.PreRefs` 回调，由 `RelayManager.Admit` 实现）：

1. 实时校准上游基线：`git.LsRemote(url, auth, opts, "git-receive-pack")`（一次 info/refs，不传对象）。
   校准失败（上游不可达/认证失败）→ **整批拒绝**（`relay: upstream base unavailable: ...`）。
2. 逐 ref 判定：

   | 情况 | 结果 |
   |---|---|
   | ref 不在白名单（默认 `refs/heads/`、`refs/tags/`） | `relay: ref not forwarded (allowed prefixes: ...)` |
   | 上游没有该 ref 且 `old = 0` | 放行（新分支 / 新 tag） |
   | 上游没有该 ref 但 `old != 0` | `relay: stale base (upstream has no <ref>); fetch and retry` |
   | `old != 上游 oid` | `relay: stale base (upstream <ref> is <oid>); fetch and retry` |
   | `old = 上游 oid` 且 `new` 是其后代 | 放行（快进） |
   | `old = 上游 oid` 但非快进 | `relay: non-fast-forward push rejected (upstream base is <oid>)` |
   | 删除（`new = 0`）且 `old = 上游 oid` | 放行，除非 `allowDelete=false` |

3. 被拒绝的 ref 不更新本地、不进入转发队列，客户端在 report-status 里看到 `ng <reason>`
   （真实 git 输出：`! [remote rejected] master -> master (relay: stale base (upstream refs/heads/master is 63631c4); fetch and retry)`）。

force push（命令行 `+`）在准入层被拒绝：它会破坏「可直通转发」的前提。

## 3. 转发

push 成功（ref 已更新）后由传输层调用 `RelayManager.OnRefsUpdated`：

1. 成功更新且在白名单内的 ref **立即落盘**进 `MirrorConfig.PendingRefs`
   （权威的「尚未转发成功」集合，重启后据此补推）。
2. 提交有界任务队列（容量 1024，worker = `relayMaxConcurrentPushes`，默认 5）；
   同一仓库已在队列/执行中时不重复入队（pending 已落盘，运行中的任务会看到）。
3. 每轮：读 `PendingRefs` → 以**本地当前 ref 值**为内容构造 `[]git.RefSpec`
   （本地已无该 ref → 删除命令）→ `git.PushRemote`。
4. 结果处理：
   - 成功 → 从 `PendingRefs` 移除、推进内存基线（`relayBase.Advance/Remove`）、写 `lastPush`；
   - 上游明确拒绝（`ng`，如 protected branch）或策略拒绝（`allowDelete=false`）→ 从 pending 移除并记 `lastPushError`（**不重试**，否则无限循环）；
   - 整包失败（网络/上游 5xx/超时）→ pending 保留，按 `relayPendingRetryIntervalSec`（默认 60s，指数退避至 10 min 上限，可配负值关闭）后台重试；
   - 一轮任务内最多连续 3 轮，把期间涌入的新 push 合并处理。
5. 每次尝试写 `pgit-relay.jsonl`（trigger ∈ push/manual/retry/startup，含耗时、对象数、pack 大小、逐 ref 结果）。

手动与运维出口：

- `POST /api/v1/repos/relay/push`：先刷新基线并把「本地领先于上游」的 ref 纳入 pending，再等待结果（`RelayNow`）。
- `POST /api/v1/repos/relay/align`（需 `confirm=仓库名`）：把本地 refs 强制对齐上游基线并丢弃 pending。
  用于转发被上游永久拒绝后的恢复；本地领先提交的 ref 指向被丢弃（对象仍在磁盘，无 GC）。

## 4. 拉取方向（镜像语义 + pending 保护）

`SyncRepository` 对 relay 仓库传入 `FetchOptions.ProtectRefs = PendingRefs`：
这些 ref 完全不参与本地更新，避免「本地领先、等待转发」的提交被一次拉取回退/删除；
其余 ref 保持严格镜像语义（本地 = 上游，含上游删除的 ref）。

## 5. 纯 Go push 客户端（`git/push.go`）

无 `git` 二进制、无第三方依赖的 smart-http receive-pack 客户端：

1. `GET info/refs?service=git-receive-pack` 解析广告（复用 `fetchRefAdvertisement`/`parseRefAdvertisement`，
   跳过 `capabilities^{}` 与 peeled `^{}` 行）。要求上游广告含 `report-status`，否则直接报错。
2. 命令 `old` 一律取广告值（CAS）；无变化的 ref 记为 ok 的 no-op，不产生命令。
3. 待发对象 = 目标 oid 可达、上游 refs 不可达的部分：
   `WalkReachable(store, roots, haves)`，`haves = 上游全部 ref oid + 各命令 old`
   （本地没有的上游对象由 `Exists` 检查自动跳过，因此无需先 fetch 上游）。
4. `POST git-receive-pack`：`io.Pipe` 边编码边发（命令 pkt-line + flush + pack），
   上游支持 `ofs-delta` 时复用 clone 的出向 delta 编码，否则退化为全量对象编码；
   分层超时（Dial/TLS/ResponseHeader/停滞看门狗）与重试沿用 fetch 的传输层。
5. 解析 `report-status`（sideband 时先经 `SidebandReader` 重组 ch1），逐 ref 返回 ok/ng。

## 6. 失败与竞态

| 场景 | 行为 |
|---|---|
| 准入 ls-remote 后上游又被改动 | 上游 CAS 返回 ng → pending 记入错误并移除（不再重试）；下一次 push 会因 stale base 被拒 |
| 转发网络失败 | pending 保留 + 后台退避重试；期间该 ref 被 `ProtectRefs` 保护 |
| 上游不可达时的下游 push | 拒绝（严格）：准入依赖实时基线 |
| 进程重启 | `Bootstrap`：pending 非空立即入队补推 + 异步校准基线 |
| 准入 ls-remote 的资源占用 | 已知取舍：receive-pack 全程持有 pack 并发槽（`maxConcurrentPacks`，默认 4），
  上游慢/不可达时每个 push 可占用一个槽直至 ls-remote 超时/重试结束；并发 push 到故障上游可能连带 clone 一起排队（503）。缓解：调低 `mirrorRetryAttempts`、保证 relay 上游可达 |
| 永久失败（保护分支 / token 权限不足） | pending 移除 + `lastPushError`；用 `relay/align` 恢复 |

## 7. 验证

- `internal/pgs/git/push_test.go`：假上游（pgit 自身 receive-pack）覆盖新建/更新/删除/tag/up-to-date、
  上游 ng、sideband 与非 sideband、ofs-delta 编码路径、401/403/404/5xx 分类、重试、缺 `report-status`/`delete-refs`。
- `internal/pgs/relay_test.go`：准入矩阵（表驱动）、配置校验与持久化、设置更新不覆盖运行态、快照深拷贝。
- `internal/pgs/relay_forward_test.go`：下游用 `git.PushRemote` 经完整 gate 链路 push →
  本地更新 → 异步转发上游（轮询校验）；stale base 拒绝且本地不被污染；删除与 tag 转发；
  上游故障保留 pending + `RelayNow` 恢复；上游 ng 丢弃 pending；`Bootstrap` 补推；pull 保护 pending。
- `internal/pgs/server/relay_api_test.go`：创建/设置/状态/日志/对齐端点与错误码；
  端到端 HTTP push 转发；mirror 仓库 push 仍 403。
- 手工端到端（两个 pgit 实例 + 真实 git 客户端）：seed → 基线拉取 → 下游 push 自动转发 →
  上游被他人推进后 `[remote rejected] ... (relay: stale base ...)` → `align` 恢复 → mirror 仍 403。

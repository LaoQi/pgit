# Changelog

pgit 变更历史。`AGENTS.md` 只描述**当前**架构、约束与用法；变更过程、缺陷修复、
性能调优与历史决策收录于此。条目按时间倒序，括注提交短 hash。

## 2026-09-23

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

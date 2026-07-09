# 镜像仓库功能 实施方案

## 概述

为 pgit 新增镜像仓库功能：从远程 HTTP/HTTPS git 仓库全量镜像所有 refs 到本地，支持定时自动同步和手动触发同步，记录每次同步日志。

- **实现方式**：纯 Go fetch 客户端（复用现有 PackDecoder/LooseStore/RefStore/PktReader），不依赖外部 git 二进制
- **远程协议**：仅 HTTP/HTTPS smart-http
- **同步触发**：定时（可配置间隔）+ 手动 API
- **镜像范围**：全量镜像所有 refs（分支+标签），本地有但远程无的 ref 删除
- **首次同步**：异步（API 立即返回，后台错峰执行）
- **同步日志**：JSONL 文件存储（`<repo>.git/pgit-sync.jsonl`），API 查询

## 实施阶段

### 阶段 1: Git fetch 客户端

- **状态**: 已审计
- **目标**: 在 `internal/pgs/git/fetch.go` 实现纯 Go 的客户端侧 upload-pack 协议，能从远程 HTTP smart-http 仓库 fetch 所有对象和 refs
- **实施内容**:
  - `FetchAuth` 结构体（Type/Username/Password）
  - `FetchResult` 结构体（ObjectsWritten/RefsUpdated/RefsDeleted/UpToDate）
  - `FetchRemote(remoteURL, repoRoot string, auth *FetchAuth) (*FetchResult, error)` 函数
  - 流程：HTTP GET info/refs -> 解析 ref advertisement -> 读本地 refs 构建 haves -> HTTP POST git-upload-pack(wants+haves+done) -> 接收 NAK + sideband pack -> PackDecoder 解码 -> LooseStore 写入 -> RefStore.Update（镜像全量 ref 更新含删除）-> HEAD best-effort 更新
  - 复用：PktReader/PktWriter、PackDecoder、LooseStore、RefStore、SidebandPack 常量、parseWantLine
  - sideband demux：ch1->pack、ch2->progress log、ch3->error
  - 非 sideband fallback：读剩余 body 去尾部 flush
  - ref 更新 CAS 策略：OldOid=当前本地值（存在）、ZeroOid（新建）；远程无本地有的 ref -> 删除
  - HEAD 更新：远程 HEAD oid 匹配 refs/heads/* 分支则 SetHead；本地 HEAD 指向分支不存在则取第一个分支
- **验证标准**: `go build ./...` + `go vet ./...` + `go test ./internal/pgs/git/` 通过
- **测试**: `fetch_test.go` 用 httptest + pgit 自身协议（ServeInfoRefs/HandleUploadPack）做远程服务器，无需外部 git
  - TestFetchRemote_InitialClone：空本地 -> fetch -> 验证 objects/refs/HEAD
  - TestFetchRemote_IncrementalSync：已有本地 -> 远程追加 commit -> fetch -> 验证增量
  - TestFetchRemote_UpToDate：无变化 -> UpToDate=true
  - TestFetchRemote_EmptyRemote：空远程 -> 无操作
  - TestFetchRemote_BasicAuth：带认证的服务器
  - TestFetchRemote_RefDeletion：远程删分支 -> 本地同步删除
- **审计结果**: ✅ 通过。build/vet/test 全绿，7 个 fetch 测试通过。代码风格与 protocol.go 一致，错误处理规范，无安全隐患。fetchInfoRefs 辅助函数分离清晰。

### 阶段 2: 数据模型 + Manager 扩展

- **状态**: 已审计
- **目标**: 扩展 Repository 数据模型支持镜像配置，Manager 新增创建镜像仓库和同步方法
- **实施内容**:
  - `repository.go`: MirrorConfig 结构体 + Repository.Mirror 字段(omitempty) + IsMirror() + SaveMetadata 原子写(tmp+rename)
  - `manager.go`: CreateMirrorRepository(name, desc, mirror) 含 URL(http|https)校验 + SyncRepository(name) 返回 *FetchResult
  - SyncRepository 出错也保存 LastError，返回原始 error
- **验证标准**: `go build ./...` + `go vet ./...` + `go test ./internal/pgs/` 通过
- **测试**: TestCreateMirrorRepository + TestLoadRepo_MirrorBackwardCompat + TestCreateMirrorRepository_Validation
- **审计结果**: ✅ 通过。build/vet/test 全绿，3 个新测试通过，现有测试无回归。SaveMetadata 原子写保留 MarshalIndent 格式。

### 阶段 3: 同步日志 + SyncManager

- **状态**: 已审计
- **目标**: 实现同步日志记录与查询，定时同步调度器
- **实施内容**:
  - `sync_log.go`: SyncLogEntry(9字段) + AppendSyncLog(O_APPEND JSONL) + ReadSyncLog(全量读+反转+limit截断)
  - `sync_manager.go`: SyncManager + mirrorScheduler(per-repo syncMu+syncing 防重入)
  - Register: SyncInterval>0 时启动 goroutine(1-10s 错峰+initial sync+ticker scheduled sync)
  - Unregister: stop!=nil 检查后 close
  - doSync: 防重入 -> SyncRepository -> 构建 entry -> AppendSyncLog
  - SyncNow: 临时创建 scheduler(无 stop) -> 防重入 -> 同步 -> 返回 *SyncLogEntry(trigger=manual)
  - Stop: 遍历 close all stop，清空 map
  - 全局单例 var SyncMgr *SyncManager + InitSyncManager()
- **验证标准**: `go build ./...` + `go vet ./...` + `go test ./internal/pgs/` 通过
- **测试**: sync_log_test.go(3) + sync_manager_test.go(2)
- **审计结果**: ✅ 通过。build/vet/test 全绿。syncing 防重入设计正确(per-scheduler mutex 不持锁执行同步)。SyncNow 临时 scheduler 兼容 Unregister 的 stop!=nil 检查。

### 阶段 4: API + 启动集成

- **状态**: 已审计
- **目标**: HTTP API 支持镜像仓库创建、手动同步、查询同步日志；启动时注册已有镜像仓库
- **实施内容**:
  - `http.go`: createRepo 镜像扩展(mirrorUrl 表单字段) + syncRepo handler(POST sync,错误码映射404/400/409/500) + syncLog handler(GET sync-log,limit默认50) + deleteRepo 注销 SyncMgr
  - `apidocs.go`: createRepo 新增 5 个 mirror 参数 + sync 端点 + sync-log 端点文档
  - `main.go`: 移除 checkEnv + InitSyncManager + 遍历注册镜像仓库 + 信号处理 Stop
- **验证标准**: `go build ./...` + `go vet ./...` + `go test ./...` 全部通过
- **审计结果**: ✅ 通过。build/vet/test 全绿。API 错误码映射完整，apidocs 同步更新，main.go 启动流程正确。

## 变更记录
| 时间 | 阶段 | 操作 | 备注 |
| 2026-07-10 | 阶段 1 | 实施完成 | fetch.go + fetch_test.go，7 测试通过 |
| 2026-07-10 | 阶段 2 | 实施完成 | repository.go + manager.go，3 新测试通过 |
| 2026-07-10 | 阶段 3 | 实施完成 | sync_log.go + sync_manager.go，5 新测试通过 |
| 2026-07-10 | 阶段 4 | 实施完成 | http.go + apidocs.go + main.go |
| 2026-07-10 | 阶段 4 | 审计通过 | build/vet/test 全绿 |
| 2026-07-10 | 最终 | 全部完成 | 4 阶段全部审计通过，83 测试通过，AGENTS.md 已更新 |

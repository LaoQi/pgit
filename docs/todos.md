# todos.md — 自研 git 协议层实施（历史记录）

> **历史实施记录**：本文记录 `internal/pgs/git/` 协议层从零实现时的阶段计划与决策，
> 反映当时的取舍。此后历经阶段 1–5 重构（并发/存储抽象/流式化/接入层/可观测性）与
> chi 移除等变更，**部分细节已不再准确**——当前架构与约束以 `AGENTS.md` 为准，
> 变更历史见 `CHANGELOG.md`。

## 目标

新增 `internal/pgs/git/` 内部包，纯 Go 实现 git wire protocol v0 服务端，支持 HTTP + SSH 的 clone/push，初始版仅松散对象存储，消除运行时对 `git` 二进制的依赖。

## 决策汇总

| 维度 | 选定 |
|------|------|
| 协议版本 | v0 only（不广告 v2 能力，客户端自动降级） |
| sideband | 启用 sideband-64k（pack 走 ch1，进度走 ch2） |
| push 安全 | 仅 old-oid CAS，不限制 force-push；大小上限 `maxPushBytes`（默认 2GiB，阶段 3-2 加入） |
| 对象完整性 | 逐对象 SHA1 重算校验，不做可达性检查 |
| ref 原子性 | per-ref（lock file + rename），与 git 一致 |
| packed-refs | 读取兼容（合并 loose + packed-refs 视图），写入只用 loose |
| 存储策略 | 初始版全 loose（不落盘 pack，不 repack） |

## 初始版明确不做

protocol v2 / 多轮 negotiation / shallow / partial clone / thin pack / packfile 落盘 / repack-gc / dumb HTTP / reflog / alternates / force-push 限制 / 可达性检查。（**大小限制已实现**：`maxPushBytes`，阶段 3-2）

## 阶段与状态

| # | 阶段 | 文件 | 状态 | 审计 |
|---|------|------|------|------|
| 1 | 基础层 | oid.go object.go loose.go parse.go | done | 5文件5测试通过，vet无警告 |
| 2 | refs 层 | refs.go | done | refs.go+13测试，CAS/symref/packed-refs 全覆盖 |
| 3 | pack 编解码 | pktline.go delta.go pack_encode.go pack_decode.go | done | 4文件+10测试，真实git pack互验，ofs-delta字节偏移正确；出向 delta 生成（EncodeDelta 固定窗口滚动hash + OFS_DELTA 编码 + ServeUploadPack 配对）已实现 |
| 4 | 遍历层 | reach.go | done | 88行+13测试，gitlink跳过正确，BFS去重 |
| 5 | 协议层 | protocol.go service.go | done | 387行+11测试，v0状态机+sideband+空仓库，回环测试通过 |
| 6 | 对接替换 | server/http.go server/ssh.go | done | http.go+ssh.go exec 全部移除，接入 git 包，build/vet/test 通过 |
| 7 | 集成测试 | 真实 git clone/push 端到端 | done | clone空仓库✓ push落盘✓ clone回环内容完整✓；report-status 尾部 flush-pkt 已补（含 sideband 外层 flush），客户端不再报挂断 |
| 8 | 浏览 API 去 git 化 | repository.go Tree/Blob/Archive/ForEachRef | done | 已改为纯 Go（`git.ForEachRefs`/`ResolveTreeIsh`/`TreeAt`/`BlobAt` + `archive/zip`），运行时不再调 git 二进制 |

## 验证基线

每阶段：`go build ./internal/pgs/git/...` + `go vet ./internal/pgs/git/...` + `go test ./internal/pgs/git/...` 通过。

# todos.md — 自研 git 协议层实施

## 目标

新增 `internal/pgs/git/` 内部包，纯 Go 实现 git wire protocol v0 服务端，支持 HTTP + SSH 的 clone/push，初始版仅松散对象存储，消除运行时对 `git` 二进制的依赖。

## 决策汇总

| 维度 | 选定 |
|------|------|
| 协议版本 | v0 only（不广告 v2 能力，客户端自动降级） |
| sideband | 启用 sideband-64k（pack 走 ch1，进度走 ch2） |
| push 安全 | 仅 old-oid CAS，不限制 force-push，无大小上限 |
| 对象完整性 | 逐对象 SHA1 重算校验，不做可达性检查 |
| ref 原子性 | per-ref（lock file + rename），与 git 一致 |
| packed-refs | 读取兼容（合并 loose + packed-refs 视图），写入只用 loose |
| 存储策略 | 初始版全 loose（不落盘 pack，不 repack） |

## 初始版明确不做

protocol v2 / 多轮 negotiation / delta 生成 / shallow / partial clone / thin pack / packfile 落盘 / repack-gc / dumb HTTP / reflog / alternates / 大小限制 / force-push 限制 / 可达性检查。

## 阶段与状态

| # | 阶段 | 文件 | 状态 | 审计 |
|---|------|------|------|------|
| 1 | 基础层 | oid.go object.go loose.go parse.go | done | 5文件5测试通过，vet无警告 |
| 2 | refs 层 | refs.go | done | refs.go+13测试，CAS/symref/packed-refs 全覆盖 |
| 3 | pack 编解码 | pktline.go delta.go pack_encode.go pack_decode.go | done | 4文件+10测试，真实git pack互验，ofs-delta字节偏移正确 |
| 4 | 遍历层 | reach.go | done | 88行+13测试，gitlink跳过正确，BFS去重 |
| 5 | 协议层 | protocol.go service.go | done | 387行+11测试，v0状态机+sideband+空仓库，回环测试通过 |
| 6 | 对接替换 | server/http.go server/ssh.go | done | http.go+ssh.go exec 全部移除，接入 git 包，build/vet/test 通过 |
| 7 | 集成测试 | 真实 git clone/push 端到端 | done | clone空仓库✓ push落盘✓ clone回环内容完整✓；report-status尾部帧致客户端报挂断(数据已正确写入,已知缺陷) |
| 8 | 浏览 API 去 git 化 | repository.go Tree/Blob/Archive/ForEachRef | deferred | 非阻塞优化，当前浏览 API 仍用 exec 调 git（不影响 clone/push）；后续按 ObjectStore/RefStore 改写 |

## 验证基线

每阶段：`go build ./internal/pgs/git/...` + `go vet ./internal/pgs/git/...` + `go test ./internal/pgs/git/...` 通过。

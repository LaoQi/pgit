# pgit 后续实施清单

> 状态：**待实施**（本文档只做计划与设计要点，不含已完成的改造）。
> 已完成的阶段 1–5 与 P1-5/B/C/D 见 `CHANGELOG.md`；架构评估见 `docs/architecture-review.md`；
> 安全策略见 `docs/security-policy.md`。最后更新：2026-09-24。

## 约定

- 每项独立可交付、独立提交；验收统一为 `go build ./... && go vet ./... && go test -race ./...` 全绿，
  并附真实实例端到端验证（HTTP/SSH 传输至少各一遍）。
- 新增配置项需同时更新 `pgit -d` 默认输出、`Reload`/`HotReload` 分类与 AGENTS.md 配置字段说明。

---

## E1. webhook 事件通知（优先级：高）

**动机**：ROADMAP 上唯一未兑现项；push/mirror 完成后触发外部自动化（CI、镜像级联、通知）。

**设计要点**

- 事件源：`receive-pack` 成功后（per-ref 更新结果）、镜像同步完成后（成功/失败）。
- 配置：per-repo `webhooks: [{url, secret, events[], active}]`，存于 `pgit.json`（与 `MirrorConfig` 同级新字段）。
- 投递：`POST` JSON body（事件类型、仓库、ref 变更/before-after oid、同步统计、时间戳），
  头 `X-Pgit-Event`、`X-Pgit-Delivery`（投递 ID）、`X-Pgit-Signature-256`（HMAC-SHA256 of body，密钥为 `secret`）。
- 可靠性：异步队列 + 有界并发；失败按指数退避重试（复用 fetch 的退避/分类思路）；
  投递记录 JSONL（`pgit-hooks.jsonl`）供查询；超时与响应体大小上限。
- 端点：`GET/POST/DELETE /api/v1/repos/{name}/webhooks`、`GET .../webhooks-log`。
- 注意：`receive-pack` 当前在 `protocol.go` 内完成 ref 更新，事件需在其后投递（避免阻塞传输主路径）。

## E2. gc / repack 后台任务（优先级：中）

**动机**：当前全 loose、无 pack、删仓才回收；长期运行后 loose 对象数量与目录项膨胀。

**设计要点**

- 复用已有 `task`/`task_manager`（状态机 + cron + 回调）。
- 内容：loose 对象打包成 packfile + 写入 `packed-refs`、清理不可达对象（保留期可配）、
  `pack` 目录与 `info/alternates`（明确不做 alternates）。
- 风险：需要 `ObjectStore` 的写入/迁移语义（当前接口只有 `Read/Exists/Write/Stat`）→ 先扩展接口再实现。
- 并发：与进行中的 push/clone 互斥（可复用 `MaxConcurrentPacks` 信号量思路）；
  打包期间的新写入需安全（tmp+rename 落盘，最后原子替换）。
- 配置：`gcEnabled`、`gcIntervalSec`、`gcGracePeriodSec`。

## E3. 协议 v2（优先级：低）

**动机**：v0 已有基本模式 negotiation 可用；v2 是 shallow/partial clone 与 `filter` 的前置。

**设计要点**

- `ls-refs` 命令（替代 advertisement，减少 refs 多的场景开销）+ `fetch` 命令（含 `want/have/done`、
  `filter`、`shallow`/`deepen`）。
- `GIT_PROTOCOL=version=2` env 协商：当前 SSH 侧**显式拒绝**（`Reply(false)`）、HTTP 侧未读取该头 → 需改造。
- 需先决定 shallow/partial 的存储语义（当前全 loose 且无 commit-graph，shallow 边界要额外文件）。
- 建议顺序：先 v2 的 `ls-refs`（收益确定）→ 再评估 shallow/filter。

## E4. 浏览 API 可用性（优先级：中）

- **分页**：`GET /api/v1/repos/{name}/commits/{ref}` 支持 `?cursor=`/`?limit=`（当前只有 limit）；
  大仓库 `GET /repos/{name}` 的 refs 列表同理。
- **ETag / 条件请求**：tree/blob/commits 响应加 ETag（基于 ref 指纹 + 路径），支持 `If-None-Match` 走 304。
  可复用 `ForEachRefs` 已有的 refs 指纹。
- **`List()` 每请求全量 `filepath.Walk`**：为仓库索引加目录 mtime 快照或定期扫描，避免每次列表都 walk。

## E5. 工程基建（优先级：中）

- **CI 门禁**：GitHub Actions 跑 `go build ./... && go vet ./... && go test -race ./...`；
  `PGIT_E2E=1` 的端到端作业单独跑（需 git/ssh 二进制）。
- **lint**：引入 `golangci-lint`（至少 `govet`/`staticcheck`/`errcheck`/`ineffassign`）。
  注意：测试中调用真实 git 必须注入 `-c commit.gpgsign=false`（见 AGENTS.md）。
- **依赖**：`chi v4.0.2`（2019）。评估升级到 v5 或换用标准库 `http.ServeMux`（1.22+ 已支持方法+通配符路由，
  可去掉 chi 依赖与手写 `NotFound` 兜底）。升级需回归路由优先级（`/api/v1/*`、`{webuiPrefix}/*`、alias 兜底）。

## E6. 其他零散项

- `apidocs.go` 手写静态 JSON 与 chi 路由双份维护 → 由路由表生成（需先在路由注册时收集元数据）。
- `DELETE /api/v1/repos/{name}` 的 `confirm` 只能走 query：`r.FormValue` 不解析 DELETE 的 body
  （net/http 仅对 POST/PUT/PATCH 解析表单）。apidocs 已正确标注 `In: "query"`，无需修改；
  但 WebUI/第三方客户端需注意勿用 body 传 `confirm`（否则 400）。
- 镜像 webhook 与 E1 合并；`mirror` 的失败告警（邮件/webhook）一并考虑。
- 日志：`logLevel=debug` 时每对象日志在大仓库下量大，可考虑采样或总量上限。

## 明确不做

- TLS / 证书 / ACME（交由外层反向代理，见 `docs/security-policy.md`）。
- thint pack 落盘、repack-gc 之外的存储优化（见评估文档"明确不做"）。
- OAuth/OIDC、per-branch 权限。

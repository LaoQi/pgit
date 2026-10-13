# pgit —— 自建 git 服务：接口与用法

> 内建文档：随二进制发布，`GET /api/v1/guide.md` 始终可读。内容变更即软件变更，需改
> `internal/pgs/server/guide.md` 并重新构建部署。
> 逐字段的结构化接口说明见 `GET /api/v1/`（JSON，21 个管理端点）。

本文面向「读文档干活」的 agent：先给最短上手路径，再给 git 访问方式、管理 API 分组速查与常用示例，
最后是限制、运行态字段与故障排查。文中 `<host>:<port>` 指本实例的监听地址（部署事实见部署方文档），
默认监听 `0.0.0.0:3000`。

## 1. 它是什么

纯 Go 实现的个人 git 服务器：

- **单端口多路复用**：同一个监听地址上同时提供 HTTP 与 SSH（按连接首字节 `SSH-` 自动分流），没有独立的 SSH 端口。
- **不依赖 `git` 二进制**：smart-http 的 upload-pack / receive-pack、SSH exec、浏览 API（tree/blob/archive/commits）
  全部由内置的纯 Go git 协议实现；运行时 `PATH` 里没有 git 也能 clone/push。
- **内置 WebUI**（可 embed 或从磁盘加载），以及一套管理 API。
- **三种仓库形态**：普通仓库、镜像仓库（pull）、中转仓库（relay）—— 见 §3。
- **无 TLS、无 per-repo 权限模型**、SSH 认证全放行（单用户内网场景）；HTTP 侧可选一组全局 Basic 凭据。

## 2. 最短上手（给 agent）

```bash
BASE=http://<host>:<port>

# 1) 有哪些仓库（按最近提交时间倒序，无提交的排最后）
curl -s $BASE/api/v1/repos

# 2) 某仓库的元数据 + 分支/标签（这是唯一能列出 refs 的接口）
curl -s "$BASE/api/v1/repos/info?ref=<name|alias>"

# 3) 克隆（HTTP；别名可含斜杠）
git clone $BASE/<alias>.git
# 开启 httpAuth 时用： git clone http://<user>:<pass>@<host>:<port>/<alias>.git

# 4) 建仓库（form 编码，不是 JSON body）
curl -s -X POST $BASE/api/v1/repos \
  -d "name=my-repo" -d "description=demo" -d "defaultBranch=main"

# 5) 加别名（别名可含斜杠；末位别名 = 首页展示与 clone 提示的首选 ref）
curl -s -X POST $BASE/api/v1/repos/aliases -d "ref=my-repo" -d "alias=team/my-repo"

# 6) 列目录 / 读文件 / 取 zip
curl -s "$BASE/api/v1/repos/tree?ref=my-repo"
curl -s "$BASE/api/v1/repos/blob/README.md?ref=my-repo&treeish=main"
curl -sOJ "$BASE/api/v1/repos/archive?ref=my-repo&treeish=main"
```

## 3. 概念速查

### ref（仓库引用）

- 每个仓库有唯一的 **Name**：建仓时指定的单段标识，创建后不可变，同时是默认别名、不可删除。
- 另有 0..n 个 **alias**，仅作访问路径映射（可含 `/`，如 `team/my-repo`；多段别名无需百分号编码）。
- git URL 与管理 API 的 `ref` 参数都可以用 Name 或任意 alias，二者等价。
- **末位 alias = 首选展示 ref**：首页列表与 clone 提示用它，新增别名即成为新的首选。
- 命名规则（Name 与 alias 共用；唯一性比较**不区分大小写**，解析仍大小写敏感）：
  字符限 `A-Za-z0-9_-.` 与 `/` 分段；单段 ≤64B、总长 ≤100B、段数 ≤8；段首尾须为字母/数字/下划线；
  不得以 `.git` 结尾；保留字 `api`、WebUI 前缀、`healthz`、`metrics` 及其子树不可用。
- `Name ∪ alias` 是全局唯一命名空间，冲突返回 409（改别名/删别名/删仓库会释放名字）。

### 三种仓库形态

| 形态 | 权威 | 能 push 吗 | 说明 |
|---|---|---|---|
| 普通 | 本地 | 能 | 常规托管仓库 |
| 镜像（`mirror.mode` 空或 `pull`） | 上游 | **不能**（403） | 定时/手工从上游 HTTP(S) 全量镜像所有 refs |
| 中转 relay（`mirror.mode=relay`） | 上游为基线 | 能，但**必须先 fetch** | 每个 ref 先与上游实时基线比对通过，再异步转发上游 |

### 删除是软删除

`DELETE /api/v1/repos/info` 只在仓库目录里写 `pgit.deleted` 标记：git 数据全部保留、索引即刻注销、
git 与管理 API 立刻 404。删掉标记文件并重启即恢复；要彻底释放名字才需手工删除目录。
有标记的目录在重启后不会被扫描，也不能用同名重建。

## 4. git 访问

### URL 形态

```bash
# HTTP（TLS 交给外层反向代理，pgit 自身不实现 TLS）
git clone http://<host>:<port>/<alias>.git

# SSH（与 HTTP 共用同一端口；用户名随意，服务端不校验）
# 首次连接需接受 host key：加 -o StrictHostKeyChecking=accept-new
git clone ssh://git@<host>:<port>/<alias>.git
```

- 路径必须是 `/<alias>.git`，`.git` 后缀必需（服务端按此前缀解析），**不是** `/<name>.git` 这种绑定目录名的形式。
- 别名含斜杠时照常书写：`git clone http://<host>:<port>/team/my-repo.git`。
- fetch 支持增量（have 差量），clone/fetch 都是纯 Go 实现的对象流。

### 认证

- **HTTP**：仅当 `httpAuth=true` 时启用**一组全局** Basic 凭据（配置项 `credentials`），不区分仓库、无角色；
  未启用则完全不校验。`/healthz`、`/metrics` 永远不校验。
- **SSH**：host key 默认 ed25519（缺失自动生成）。服务端不校验用户名与公钥（全放行桩）。
- 没有 per-repo 权限、没有 TLS：不要把这个端口直接暴露到不可信网络。

### 写规则（push）

- **普通仓库**：允许任意 push，只做 old-oid CAS；不禁止 force，但非快进推送会在服务端日志里标记 `[force-push]`（仅审计）。
- **镜像仓库**：push 一律拒绝 —— HTTP 403 / SSH stderr `fatal: mirror repository: push disabled`（广告阶段就拦）。
  上游有新提交时用 `POST /api/v1/repos/sync` 或等定时同步。
- **中转仓库（relay）**：允许 push，但每个 ref 都必须过准入闸门 —— **先 `git fetch` 再 push**：
  - 上游无该 ref 且 `old=0`（新分支/新 tag）→ 放行
  - `old ≠ 上游当前 oid` → 拒绝：`relay: stale base (upstream <ref> is <oid>); fetch and retry`
  - `old = 上游 oid` 但新值不是其后代（非快进）→ 拒绝：`relay: non-fast-forward push rejected`
  - 删除 ref（`new=0`）：`old = 上游 oid` 且 `relayAllowDelete`（默认允许）为真 → 放行，否则拒绝
  - ref 不在白名单（默认 `refs/heads/` + `refs/tags/`）→ 拒绝；上游不可达/认证失败 → 整批拒绝
  - 放行且本地更新成功的 ref 进入 `pendingRefs` 并**异步转发上游**（永远是快进推送，不需要 force）；
    网络类失败按 `relayPendingRetryIntervalSec`（默认 60s）退避重试，上游明确拒绝则放弃重试并记 `lastPushError`。

## 5. 管理 API

统一前缀 `/api/v1/`。约定：

- **仓库一律通过 `ref` 参数引用**（Name 或 alias），不占路径段；只有 tree/blob 的文件路径是路径参数（尾随通配）。
- 参数走 form-urlencoded（POST/DELETE 的 body）或查询串，**不是 JSON body**。
- 参数值拿到的已经是解码后的原文；给 curl 传多段别名时直接写 `-d "alias=team/my-repo"` 即可。
- 逐字段的结构化说明（含请求/响应示例与注意事项）：`GET /api/v1/`。
- 开启 `httpAuth` 时全部管理 API 需要 Basic 凭据。仓库不存在返回 404，缺参数/参数非法返回 400。

### 分组速查

- 仓库管理：`GET /api/v1/repos`、`POST /api/v1/repos`、`GET /api/v1/repos/info`、`DELETE /api/v1/repos/info`、
  `POST /api/v1/repos/aliases`、`DELETE /api/v1/repos/aliases`、`POST /api/v1/repos/default-branch`、`POST /api/v1/repos/settings`
- 只读浏览：`GET /api/v1/repos/tree`、`GET /api/v1/repos/blob`、`GET /api/v1/repos/archive`、`GET /api/v1/repos/commits`
- 镜像：`POST /api/v1/repos/sync`、`GET /api/v1/repos/sync-log`、`GET /api/v1/repos/mirror-status`
- 中转：`POST /api/v1/repos/relay/push`、`GET /api/v1/repos/relay-status`、`GET /api/v1/repos/relay-log`、`POST /api/v1/repos/relay/align`
- GitHub：`GET /api/v1/github/repos`、`POST /api/v1/github/import`

### 常用示例

建普通仓库：

```bash
curl -s -X POST $BASE/api/v1/repos \
  -d "name=notes" -d "description=my notes" -d "defaultBranch=main"
```

建镜像仓库（定时拉取上游）：

```bash
curl -s -X POST $BASE/api/v1/repos \
  -d "name=gh-foo" -d "description=mirror of foo" \
  -d "mirrorUrl=https://github.com/user/foo.git" \
  -d "mirrorInterval=3600" \
  -d "mirrorProxy=http://127.0.0.1:7890"   # 可选；URL 带 userinfo 即代理认证
```

建中转仓库（允许下游快进推送并自动转发上游）：

```bash
curl -s -X POST $BASE/api/v1/repos \
  -d "name=relay-foo" -d "mirrorUrl=https://github.com/user/foo.git" \
  -d "mirrorMode=relay" -d "relayRefs=refs/heads/, refs/tags/" -d "relayAllowDelete=true"
```

改描述/镜像配置（`mirrorPassword` 留空表示保留原值；`mirrorMode` 不传表示保持原模式）：

```bash
curl -s -X POST $BASE/api/v1/repos/settings -d "ref=gh-foo" -d "description=new desc" -d "mirrorInterval=600"
```

同步与观测：

```bash
curl -s -X POST $BASE/api/v1/repos/sync -d "ref=gh-foo"            # 同步执行，返回本次日志条目（非镜像 400；已在跑 409；队列满 503）
curl -s "$BASE/api/v1/repos/mirror-status?ref=gh-foo"              # scheduled/queued/syncing/lastSync/lastError/nextScheduled
curl -s "$BASE/api/v1/repos/sync-log?ref=gh-foo&limit=20"          # 日志，最新在前
curl -s "$BASE/api/v1/repos/relay-status?ref=relay-foo"            # upstream/pendingRefs/lastPushError/baseRefs/differ
curl -s -X POST $BASE/api/v1/repos/relay/push -d "ref=relay-foo"   # 手工转发（先 reconcile 本地领先的 ref）
curl -s "$BASE/api/v1/repos/relay-log?ref=relay-foo&limit=20"      # 转发日志，最新在前
```

删除（需 `confirm` 等于仓库 canonical name，用别名寻址时也一样）：

```bash
curl -s -X DELETE "$BASE/api/v1/repos/info?ref=team/my-repo&confirm=my-repo"
```

GitHub 批量导入（Token 放请求头，避免进 URL 与日志；只读发现接口同款参数）：

```bash
curl -s "$BASE/api/v1/github/repos?owner=LaoQi&includeForks=false" -H "X-Github-Token: $GH_TOKEN"
curl -s -X POST $BASE/api/v1/github/import \
  -H "X-Github-Token: $GH_TOKEN" \
  -d "owner=LaoQi" -d "repos=alpha" -d "repos=beta" -d "syncInterval=3600"
```

导入会为每个选中仓库建镜像仓库：本地 `Name = {namePrefix}{owner}_{repo}`、额外别名 `{owner}/{repo}`
（于是 clone 地址就是 `http://<host>:<port>/{owner}/{repo}.git`），description 与默认分支取自 GitHub。
Token **只在私有仓库**落盘（作为 basic 认证密码），公有仓库匿名同步；已存在的同名仓库/别名一律跳过不覆盖；
每仓库独立成败，看响应里的 `results[]`；单批上限 200。

### 危险/易错操作

- `POST /api/v1/repos/relay/align`：把本地 refs 强制对齐上游基线并丢弃 `pendingRefs`。用于上游永久拒绝
  （保护分支、Token 权限不足）后的恢复；本地独有提交会失去 ref 指向（对象仍在磁盘上）。需 `confirm` == canonical name。
- 删除/覆盖类操作先确认对象，别拿生产当试验场。

## 6. 运维端点（不鉴权，供探针/抓取）

- `GET /healthz` → `{"status":"ok|degraded","checks":{gitRoot,syncManager,repositories,syncQueue?,relayQueue?}}`；
  仓库根不可读等异常时返回 503 且 `status=degraded`。
- `GET /metrics` → Prometheus 文本格式（`pgit_http_*`、`pgit_git_*`、`pgit_pack_*`、`pgit_mirror_*`、`pgit_relay_*`、`pgit_repos_total`）。

## 7. WebUI

- `GET /` → 302 重定向到 `/<webuiPrefix>/`（默认 `/__webui/`）。
- 前端是 History API 路由（与 API 同构）：`/`（仓库列表）、`/repo/info?ref=`、`/repo/tree/{path...}?ref=&treeish=`、
  `/import`（GitHub 导入）、`/api`（接口文档页）。
- `pgit -w ./webui` 可把内置前端导出到磁盘，改完用配置项 `webuiAssets` 指向该目录即可生效。

## 8. 限制与配置

| 配置项 | 默认 | 作用 |
|---|---|---|
| `maxPushBytes` | 2GiB | 单次 push 请求体上限 |
| `maxConcurrentPacks` | 4 | 并发 pack 传输数（超出等待） |
| `mirrorMaxConcurrentSyncs` | 5 | 镜像同步并发（任务队列容量固定 1024） |
| `relayMaxConcurrentPushes` | 5 | 中转转发并发（任务队列容量固定 1024） |
| `relayPendingRetryIntervalSec` | 60 | 待转发 ref 的后台重试间隔（负值=关闭重试） |
| `mirrorStallTimeoutSec` / `mirrorRetryAttempts` / `mirrorRetryBaseDelaySec` | 120 / 3 / 1 | 上游请求停滞看门狗、重试次数与退避基数（镜像与中转共用） |
| `httpAuth` + `credentials` | false / 空 | 全局 HTTP Basic 鉴权 |
| `enableSSH` / `sshHostKey` | false / — | 是否启用 SSH 与 host key 路径（缺失自动生成） |
| `webuiPrefix` / `webuiAssets` | `__webui` / 空 | WebUI 前缀与自定义资源目录（空=用内嵌资源） |

- 调度相关字段多可通过 `SIGHUP` 热加载；`listen`、`enableSSH`、`gitRoot`、`httpAuth`、`webuiPrefix`、`webuiAssets` 需重启。
- 存储布局：仓库 `<gitRoot>/<name>.git/`；元数据 `pgit.json`；镜像日志 `pgit-sync.jsonl`；中转日志 `pgit-relay.jsonl`；
  对象仅 loose 存储（不落 packfile、不 repack）。
- 队列满时：定时同步任务被丢弃并计数（不阻塞不重试），手工同步直接报错（503）。

## 9. 故障排查

| 现象 | 原因与处置 |
|---|---|
| git/API 404 `repository not found` | alias 拼错、仓库被软删除、或 URL 少了 `.git` 后缀 / 用了 `owner/repo` 却没建对应别名 |
| push 403 `mirror repository: push disabled` | 目标是镜像仓库（只读）。要投稿请让维护者建 relay 或普通仓库 |
| push 被拒 `relay: stale base ...` | relay 仓库基线上有更新：先 `git fetch` 再 push（准入要求 `old` = 上游当前 oid） |
| push 被拒 `relay: non-fast-forward push rejected` | relay 不接受重写历史/force（镜像语义只放行快进） |
| API 400 `not a mirror repository` / `not a relay repository` | 对非镜像仓库调了 sync/sync-log/mirror-status，或对非中转仓库调了 relay 接口 |
| API 409 | 别名与既有 Name/alias 冲突（不区分大小写），或同步/转发任务已在跑 |
| API 503 queue full | 同步队列打满（容量 1024），稍后重试或提高并发配置 |
| relay 一直没转发成功 | 看 `GET /api/v1/repos/relay-status` 的 `pendingRefs`/`lastPushError` 与 relay-log；上游永久拒绝（保护分支、Token 权限不足）可用 `POST /api/v1/repos/relay/align` 对齐回基线后重来 |
| SSH 连接被拒 | `enableSSH=false`，或客户端尚未接受 host key（用 `-o StrictHostKeyChecking=accept-new`） |
| 首页 clone 地址不是预期别名 | 别名顺序决定首选：末位 alias 即首选展示 ref，重新加别名可调整 |

## 10. 关于本文档

- 内建、只读：随二进制发布，`GET` 永远可读；磁盘上没有可写的副本，也没有对应的写接口。
- 改内容即改代码：源文件 `internal/pgs/server/guide.md`，改完重新构建部署（文档描述的就是当前运行的这份软件）。
- 更细的逐字段说明看 `GET /api/v1/`；真实地址、端口、systemd 单元、备份路径等**部署事实**看部署方文档
  （如节点说明文档里对应本服务的条目）。

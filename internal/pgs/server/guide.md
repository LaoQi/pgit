# pgit —— 自建 git 服务

HTTP 与 SSH 共用一个端口的 git 托管服务（纯 Go，无 git 二进制依赖），另有只读 WebUI 与一套管理 API。
仓库有三类：**普通**（可 push）、**镜像**（上游权威，只读，定时/手工同步）、**中转 relay**（上游为基线，下游可 push，服务负责转发上游）。

- 文中 `<host>:<port>` 是本实例监听地址（默认 `0.0.0.0:3000`）；真实地址、端口、部署方式见部署方文档。
- 本文只讲**怎么用**；逐字段的接口说明（含请求/响应示例）看 `GET /api/v1/`（JSON，26 个端点）。

## 最短上手

```bash
BASE=http://<host>:<port>

curl -s $BASE/api/v1/repos                            # 仓库列表（按最近提交时间倒序）
curl -s "$BASE/api/v1/repos/info?ref=<name|alias>"    # 元数据 + 分支/标签（唯一能列 refs 的接口）

git clone $BASE/<alias>.git                           # 克隆；别名可含斜杠（如 team/my-repo）
# 开启 httpAuth 时用： git clone http://<user>:<pass>@<host>:<port>/<alias>.git

curl -s -X POST $BASE/api/v1/repos \
  -d "name=my-repo" -d "description=demo" -d "defaultBranch=main"
curl -s -X POST $BASE/api/v1/repos/aliases -d "ref=my-repo" -d "alias=team/my-repo"

curl -s "$BASE/api/v1/repos/tree?ref=my-repo"                     # 列目录
curl -s "$BASE/api/v1/repos/blob/README.md?ref=my-repo"           # 读文件
curl -sOJ "$BASE/api/v1/repos/archive?ref=my-repo&treeish=main"   # 取 zip
```

## git 访问

- HTTP：`git clone http://<host>:<port>/<alias>.git` —— 路径必须以 `.git` 结尾；TLS 由外层反代提供。
- SSH：`git clone ssh://git@<host>:<port>/<alias>.git` —— 与 HTTP 同端口，用户名任意，
  首次连接加 `-o StrictHostKeyChecking=accept-new`。
- 认证：仅当服务端开启 `httpAuth` 时校验**一组全局** Basic 凭据（无 per-repo 权限）；未开启则不校验。
- push 规则：
  - 普通仓库：直接 push（服务端只做 old-oid CAS，不禁止 force；非快进会在服务端日志标记 `[force-push]`）。
  - 镜像仓库：**403 拒绝**（`mirror repository: push disabled`）—— 等定时同步或调 `POST /api/v1/repos/sync`。
  - 中转 relay：允许 push，但**必须先 `git fetch` 再 push**。准入以上游实时基线判定：
    `old` 必须等于上游当前 oid（否则 `relay: stale base (upstream <ref> is <oid>); fetch and retry`），
    且必须是快进（否则 `relay: non-fast-forward push rejected`）；删除 ref 默认允许。
    通过后服务异步转发上游，可用 `relay-status` / `relay-log` 观察。

## 管理 API

前缀 `/api/v1/`。仓库一律用 `ref` 参数寻址（仓库名或任意别名，不占路径段）；参数为 form-urlencoded（**不是 JSON body**）。
开启 `httpAuth` 时全部需要 Basic 凭据（`/healthz`、`/metrics` 除外）。缺参数 400，仓库不存在 404。

| 用途 | 端点 |
|---|---|
| 列表 / 创建 | `GET /api/v1/repos`、`POST /api/v1/repos` |
| 详情 / 删除 | `GET /api/v1/repos/info`、`DELETE /api/v1/repos/info`（删除需 `confirm` = 仓库名；软删除，数据保留） |
| 别名 | `POST /api/v1/repos/aliases`、`DELETE /api/v1/repos/aliases` |
| 默认分支 / 设置 | `POST /api/v1/repos/default-branch`、`POST /api/v1/repos/settings` |
| 只读浏览 | `GET /api/v1/repos/tree`、`GET /api/v1/repos/blob`、`GET /api/v1/repos/archive`、`GET /api/v1/repos/commits` |
| 镜像 | `POST /api/v1/repos/sync`、`GET /api/v1/repos/sync-log`、`GET /api/v1/repos/mirror-status` |
| 中转 | `POST /api/v1/repos/relay/push`、`GET /api/v1/repos/relay-status`、`GET /api/v1/repos/relay-log`、`POST /api/v1/repos/relay/align` |
| 归档 | `GET /api/v1/repos/archives`（列表）、`POST /api/v1/repos/archives`（手动快照）、`DELETE /api/v1/repos/archives`（删除）、`GET /api/v1/repos/archives/bundle`（下载 bundle）、`POST /api/v1/repos/archives/restore`（恢复）（见「镜像归档与恢复」） |
| GitHub | `GET /api/v1/github/repos`、`POST /api/v1/github/import` |

创建镜像 / 中转仓库：

```bash
curl -s -X POST $BASE/api/v1/repos \
  -d "name=gh-foo" -d "mirrorUrl=https://github.com/user/foo.git" -d "mirrorInterval=3600"
curl -s -X POST $BASE/api/v1/repos \
  -d "name=relay-foo" -d "mirrorUrl=https://github.com/user/foo.git" -d "mirrorMode=relay"
```

同步、观测与手工转发：

```bash
curl -s -X POST $BASE/api/v1/repos/sync -d "ref=gh-foo"          # 同步执行；非镜像 400、已在跑 409、队列满 503
curl -s "$BASE/api/v1/repos/mirror-status?ref=gh-foo"            # 调度状态：queued/syncing/lastSync/lastError
curl -s "$BASE/api/v1/repos/sync-log?ref=gh-foo&limit=20"        # 同步日志（最新在前）
curl -s "$BASE/api/v1/repos/relay-status?ref=relay-foo"          # 基线、pendingRefs、lastPushError
curl -s "$BASE/api/v1/repos/relay-log?ref=relay-foo&limit=20"    # 转发日志
curl -s -X POST $BASE/api/v1/repos/relay/push -d "ref=relay-foo" # 手工转发一次
```

GitHub 批量导入：本地名 `{owner}_{repo}`、别名 `{owner}/{repo}`（于是 clone 地址为 `/{owner}/{repo}.git`），
description 与默认分支取自 GitHub；**公开仓库匿名同步、不落盘 Token**，私有仓库把 Token 作为该仓库的 basic 密码存下；
已存在的同名仓库/别名跳过不覆盖，单批上限 200，逐仓库成败看 `results[]`。

```bash
curl -s "$BASE/api/v1/github/repos?owner=LaoQi" -H "X-Github-Token: $GH_TOKEN"
curl -s -X POST $BASE/api/v1/github/import -H "X-Github-Token: $GH_TOKEN" \
  -d "owner=LaoQi" -d "repos=alpha" -d "repos=beta" -d "syncInterval=3600"
```

⚠️ `POST /api/v1/repos/relay/align` 是危险出口：本地 refs 强制对齐上游并丢弃待转发集合（需 `confirm` = 仓库名），
只在上游永久拒绝转发时用。

## 镜像归档与恢复

镜像/中转仓库在同步时会遇到两种「会丢掉本地历史」的上游变更：

| 上游动作 | 镜像默认行为（未开归档） | 开启归档后 |
|---|---|---|
| 删除分支/标签 | 本地 ref 被删除（对象留在磁盘，无 GC） | 先把当前仓库打成 bundle 快照，再删除 |
| force push / rebase（历史被重写） | 本地 ref 静默回退到新历史 | 先把当前仓库打成 bundle 快照，再回退 |

开启方式（配置项，`SIGHUP` 热加载）：

```json
{
  "mirrorArchive": true,
  "mirrorArchiveDir": "pgit-archive",
  "mirrorArchiveKeep": 3,
  "mirrorArchiveMaxBytes": 2147483648
}
```

- `mirrorArchiveDir` 默认 `pgit-archive`（相对进程工作目录，**不能以 `.git` 结尾**，否则启动扫描会当成仓库）。
  目录布局：`<dir>/<repoName>/<unixMilli>-<reason>-<ref>.bundle` + 同名 `.json`（sidecar）。
- 归档包是**标准 git bundle**（完整历史，无前置包）：可以直接 `git bundle verify x.bundle`、
  `git ls-remote x.bundle`、`git clone x.bundle` 校验或取回，也可以用 pgit 自身恢复。
- **fail-safe**：归档失败（磁盘满、目录不可写）时这些 ref **不会被更新**，其余 ref 照常同步；
  `mirror-status` 的 `lastError` 与同步日志的 `archivedSkipped`/`archiveError` 会记录原因。
  下一轮同步会再次尝试归档。
- 保留策略：每仓库保留 `mirrorArchiveKeep` 份（默认 3，负数=不限），总量不超过
  `mirrorArchiveMaxBytes`（默认 2GiB，负数=不限）；超限从最旧的开始淘汰。
- 归档包里不含镜像的 token（sidecar 不含任何凭据字段）。

列出、手动快照、下载、删除与恢复：

```bash
curl -s "$BASE/api/v1/repos/archives?ref=<name|alias>"            # 列表（最新在前）+ 当前配置
curl -s -X POST $BASE/api/v1/repos/archives -d "ref=<name|alias>"  # 立即打一份手动快照（enabled 关着也能用）
curl -sOJ "$BASE/api/v1/repos/archives/bundle?ref=<name|alias>&archive=<id>"   # 下载标准 bundle
curl -s -X DELETE "$BASE/api/v1/repos/archives?ref=<name|alias>&archive=<id>&confirm=<canonical name>"  # 删除单份
curl -s -X POST $BASE/api/v1/repos/archives/restore \
  -d "ref=<name|alias>" -d "archive=<id>" -d "confirm=<canonical name>"
# 服务器本机离线恢复（与上面同一条代码路径）：
#   pgit -c config.json --restore /path/to/<id>.bundle --repo <name|alias>
```

WebUI 也有同一套：仓库详情页 → **Snapshots** → `/archives?ref=<name|alias>`
（查看配置与列表、Take snapshot now、下载、恢复、删除）。

恢复 = 把归档里的对象解包落成 loose（幂等）+ 归档记录的 refs 覆盖写回 + HEAD 指回归档记录的默认分支。
恢复是**危险操作**（会覆盖同名 ref），所以要求 `confirm` 等于仓库 canonical name。

⚠️ 对镜像仓库恢复后，如果上游仍然是「被删除/被重写」的状态，**下一次同步会再次归档并覆盖本地**。
需要保住回退后的历史时，先修上游（恢复上游 ref / 用上游的 force push 推回），或把仓库暂停：
`POST /api/v1/repos/settings -d "ref=<name>" -d "mirrorInterval=0"`（之后不会再定时同步）。

## 其它入口

- WebUI：`/<webuiPrefix>/`（默认 `/__webui/`，`GET /` 会 302 过去）；页面 `/repo/info?ref=`、`/repo/tree/...?ref=&treeish=`、`/import`、`/api`。
- 探针（不鉴权）：`GET /healthz`（异常返回 503 + `degraded`）、`GET /metrics`（Prometheus 文本）。

## 关于本文档

内建、只读：随二进制发布，`GET` 永远可读，没有写接口；内容变更即软件变更，需改源码
（`internal/pgs/server/guide.md`）并重新部署。

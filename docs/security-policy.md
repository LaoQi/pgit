# pgit 安全策略（仅记录，未实施）

> 状态：**仅记录，暂不实施**。本文描述 pgit 当前的认证/授权现状、目标策略与已定决策，
> 供后续实施时直接落地。实施进度见 `docs/architecture-review.md` 阶段 4。
> 最后更新：2026-09-23。

## 1. 当前现状（阶段 1–3 结束时的真实行为）

| 面 | 现状 | 风险 |
|----|------|------|
| SSH 认证 | `PasswordCallback`/`PublicKeyCallback` 一律 `return nil, nil`（`ssh.go:84-92`）——**任何密钥/口令都可连接**，无用户概念 | 任何人可读写全部仓库 |
| SSH 用户名 | **完全未使用**（回调不读 `connMetadata.User()`）；URL 里的 `user@` 无实际作用 | 无 |
| HTTP 鉴权 | 单一全局 `credentials`（`user -> 明文口令`，默认 `test:123456`），`httpAuth=false` 时**完全开放** | 口令明文、无 per-repo 权限 |
| 权限模型 | 无。认证通过即拥有全部仓库的读写权限 | 无法分租户/只读协作 |
| 传输加密 | 仅 `net.Listen`，无 TLS；HTTP 侧凭据可被中间人窃取 | 口令/内容泄露 |
| 其他 | 无请求速率限制、无审计日志、无 token/撤销机制 | — |

## 2. 目标策略

### 2.1 身份与凭据

- 引入统一用户表（HTTP 与 SSH 共用一份），字段：`name`、`passwordHash`（bcrypt）、
  `authorizedKeys`（SSH 公钥列表）、`admin`（是否管理员）、`repos`（可访问仓库白名单，支持 `*`）。
- 口令一律 bcrypt 存储；配置文件中的旧明文口令在首次成功登录后自动升级为 bcrypt。
- 兼容旧配置格式（`credentials: {"user": "pass"}`）：视为 `admin=false` +
  无 repo 限制（保持升级前行为），需显式迁移到新结构才能收紧权限。

### 2.2 HTTP 鉴权

- `httpAuth=true` 时：管理 API（`/api/v1/*` 的写操作）要求 `admin=true`；
  git 传输与浏览 API 按 `repos` 白名单授权读/写。
- 凭据来源保持 HTTP Basic（不改变 URL 结构，见 §4）。
- 401 响应保持 `WWW-Authenticate: Basic realm="pgit"`，便于 `git` 交互式提示。

### 2.3 SSH 鉴权

- `sshAuthType` 配置项语义明确化（当前该字段存在但**未生效**）：
  - `none`：全放行（等价当前行为，仅建议在纯内网/测试使用）
  - `password`：校验用户表口令
  - `publickey`：校验 `authorizedKeys`
- **决策 A（已定）**：SSH **忽略用户名，只认密钥**（`~git` 惯用做法）。
  服务端由密钥反查所属用户。好处：现有 `ssh://coco@host:port/alias.git` 形式的 URL **无需修改**，
  只需登记公钥。
- 决策 B（已定）：`sshAuthType` 默认值保持 `none`（即延续当前全放行行为），
  需要鉴权必须显式配置——避免升级后意外中断既有 clone。

### 2.4 传输加密

- 决策 C：支持**自带证书文件**（`tlsCert`/`tlsKey` 配置，`listen` 或新增 `tlsListen`）。
- ACME/Let's Encrypt 自动签发**暂不引入**（会新增依赖与状态管理复杂度），
  如需要可在外层放反向代理终止 TLS。

### 2.5 其他

- 速率限制与失败登录节流（可选，后续按需）。
- 审计：记录认证主体、仓库、操作（push/clone/fetch）、结果，便于追溯。

## 3. 实施顺序（建议，未开始）

1. 用户表模型 + 配置兼容层（不改任何现有行为，仅解析）。
2. HTTP 侧接入用户表与 per-repo 鉴权；管理 API 要求 admin。
3. SSH 侧按决策 A/B 接入 authorized_keys。
4. TLS（决策 C）。
5. 审计日志与速率限制（可选）。

每步独立可交付，且**默认行为不变**，只在显式配置后收紧。

## 4. 兼容性说明（重要）

开启鉴权**不改变 git 访问 URL 的结构**，只影响凭据的提供方式：

| 协议 | URL 形式 | 开启鉴权后 |
|------|----------|-----------|
| HTTP | `http://host/alias.git` | 路径不变；凭据经 Basic auth（`http://user@host/alias.git` 或交互式提示） |
| SSH | `ssh://user@host:port/alias.git` | 路径与端口不变；按决策 A 用户名被忽略，**只需登记公钥** |

即：需要改的是**服务器端配置（登记用户/密钥）与镜像脚本中的凭据**，而非仓库路径。

## 5. 明确不做（当前阶段）

- 不实现 protocol v2 的认证扩展、不做 OAuth/OIDC。
- 不做 per-branch 权限（只到 repo 粒度）。
- 不做证书自动轮转/ACME。

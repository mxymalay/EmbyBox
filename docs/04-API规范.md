# EmbyBox 设计文档 · 04 API 规范

> 前端、Bot、外部集成方按这份文档对接。所有接口路径、字段、错误码以本文为准。

---

## 1. 通用约定

### 1.1 基础路径与版本

```
https://<你的域名>/api/v1/...
```

- 所有业务接口在 `/api/v1/` 下
- 版本号在路径里，**不做请求头协商**——路径版本最直观，也最好在反代层做灰度
- 版本升级策略：新增版本时旧版本至少保留一个发布周期；破坏性变更必须升版本，**不允许原地改语义**

### 1.2 统一响应信封

**所有** `/api/` 下的响应（含错误）都是这个结构：

```json
{
  "code": 200,
  "message": "success",
  "data": { }
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `code` | int | 业务码。200 表示成功，其余见表 §5 |
| `message` | string | 给人看的提示，**可直接展示给用户** |
| `data` | any | 业务数据。错误时通常为 `{"kind": "..."}` |

**为什么同时用 HTTP 状态码和 code**：
HTTP 状态码让反代、监控、浏览器 DevTools 直观看出成败；`code` 携带更细的业务语义。两者同时存在，且**必须一致**（不会出现 HTTP 200 + code=500）。

### 1.3 错误响应的稳定性约定

```json
{
  "code": 4010,
  "message": "用户名或密码错误",
  "data": { "kind": "invalid_credentials" }
}
```

- **`kind` 是给机器看的稳定标识**，前端做逻辑分支、做国际化都基于它
- **`message` 是给人看的**，随时可能改文案
- ⚠️ 前端**绝对不要**用 `message` 做判断，一改文案就全坏

### 1.4 认证

| 方式 | 传递 | 用途 |
|---|---|---|
| 会话令牌 | `Authorization: Bearer <token>` | Web 用户端 |
| 管理 API Key | `Authorization: Bearer ember_sk_<...>` | 外部系统集成 |
| 会话 Cookie | `Cookie: session=<token>` | 同源 Web（与 Bearer 二选一） |

**认证级别**（在路由声明里指定）：

| 级别 | 含义 | 失败响应 |
|---|---|---|
| `none` | 公开 | — |
| `user` | 需登录 | 401 + `kind=unauthenticated` |
| `admin` | 需管理员 | 403 + `kind=forbidden` |
| `apikey` | 需管理 API Key | 403 + `kind=invalid_api_key` |

**过期用户的特殊处理**：过期用户的请求会正常通过 `user` 级鉴权，响应里带 `expired: true`。**只允许访问个人中心、续费、兑换相关接口**，访问播放/写入类接口返回 403 + `kind=expired`。

这是「过期用户仍能登录续费」的落地方式——如果过期就 401，用户连付钱的入口都找不到。

### 1.5 分页

统一用**游标分页**，不用 offset——offset 在数据变动时会漏项/重项。

**请求**：
```
GET /api/v1/admin/users?limit=20&cursor=<opaque>
```

| 参数 | 说明 |
|---|---|
| `limit` | 每页条数，默认 20，最大 100 |
| `cursor` | 上一页返回的 `next_cursor`，首次请求不传 |
| `order` | 排序方向，`desc`（默认）或 `asc` |

**响应**：
```json
{
  "code": 200,
  "message": "success",
  "data": {
    "items": [ ],
    "next_cursor": "eyJpZCI6MTAwfQ==",
    "has_more": true,
    "total": 1234
  }
}
```

`total` 是可选的（大表上算总数很慢），后台列表才提供。

**例外**：管理后台的表格页需要跳页，这类接口额外支持 `page` + `page_size`，但**返回的 `total` 必须是准确值**，不允许用估算值。

### 1.6 时间格式

- 输入输出统一 **RFC3339 UTC**：`2026-10-05T10:12:33Z`
- 需要按天聚合的字段（如 `date`）用业务时区的 `YYYY-MM-DD`：`2026-10-05`
- 前端负责转成用户本地时区展示

### 1.7 幂等

三类接口支持幂等键：

| 接口 | 幂等键位置 | 说明 |
|---|---|---|
| 下单 | `Idempotency-Key` 请求头 | 同 key 重复请求返回同一订单 |
| 兑换码核销 | 服务端用 `code_id + user_id` 唯一约束 | 天然幂等 |
| 积分变动 | 服务端 `idempotency_key` 字段 | 由调用方生成 |

**所有会产生副作用的写接口都必须支持幂等**。这是"用户在弱网下点两次"不导致重复扣费的前提。

### 1.8 限流

响应头：

```
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 57
X-RateLimit-Reset: 1770000000
```

超限返回 `429` + `kind=rate_limited`。

**关键约定**：涉及认证的接口（登录、注册、找回密码、TG 绑定），**失败的请求同样消耗配额**。否则攻击者可以通过"成功了才计数"的差异来枚举账号。

**默认桶与覆盖**：§2 中标注"默认"的接口使用默认桶——按认证主体（未认证按 IP）60 次/分钟；显式标注（如 `10/m`）覆盖默认值。涉及远端调用或批量生成的昂贵操作必须显式声明，不得落在默认桶。多桶同时命中时，**最先阻断的那个桶生效**，`X-RateLimit-*` 响应头描述该桶（第二轮 D18：限流声明是 §2 每张接口表的必备列，与 docs/08-安全设计.md 的"新接口有明确的限流声明"对应）。

---

## 2. 接口总览

### 2.1 公开接口（`auth: none`）

| 方法 | 路径 | 用途 | 限流 |
|---|---|---|---|
| GET | `/healthz` | 存活探针 | 不限 |
| GET | `/readyz` | 就绪探针 | 不限 |
| GET | `/api/v1/system/info` | 系统信息（版本、站点名） | 60/m |
| GET | `/api/v1/system/events` | 扩展点清单 | 60/m |
| GET | `/api/v1/auth/config` | 登录页所需配置（是否开放注册、验证码开关等） | 60/m |
| POST | `/api/v1/auth/login` | 登录 | 10/m |
| POST | `/api/v1/auth/register` | 注册 | 5/m |
| POST | `/api/v1/auth/password/reset-request` | 请求重置密码 | 3/m |
| POST | `/api/v1/auth/password/reset` | 用验证码重置 | 5/m |
| GET | `/api/v1/auth/invite/{code}` | 校验邀请码有效性 | 10/m |

### 2.2 用户接口（`auth: user`）

| 方法 | 路径 | 用途 | 限流 |
|---|---|---|---|
| GET | `/api/v1/user/profile` | 我的资料 | 默认 |
| PATCH | `/api/v1/user/profile` | 修改资料（邮箱等） | 默认 |
| POST | `/api/v1/user/password` | 修改密码 | 5/m |
| POST | `/api/v1/user/logout` | 登出 | 默认 |
| GET | `/api/v1/user/entitlements` | 我的会员权益 | 默认 |
| GET | `/api/v1/user/points` | 积分余额与流水 | 默认 |
| POST | `/api/v1/user/points/checkin` | 每日签到 | 10/m（另有业务日配额） |
| GET | `/api/v1/user/orders` | 我的订单 | 默认 |
| POST | `/api/v1/user/redeem` | 兑换卡密 | 10/m |
| GET | `/api/v1/user/playback` | 播放记录 | 默认 |
| GET | `/api/v1/user/reports/{period}` | 观影报告 | 30/m |
| GET | `/api/v1/user/requests` | 我的求片 | 默认 |
| POST | `/api/v1/user/requests` | 提交求片 | 10/m（另有业务配额） |
| GET | `/api/v1/user/requests/{id}` | 求片详情（含步骤流水） | 默认 |
| POST | `/api/v1/user/requests/{id}/cancel` | 撤销求片 | 30/m |
| POST | `/api/v1/user/requests/{id}/bump` | 免费顶帖 | 30/m（另有 12 小时冷却） |
| GET | `/api/v1/user/routes` | 可用线路 | 默认 |
| GET | `/api/v1/user/notifications` | 通知 | 默认 |
| POST | `/api/v1/user/notifications/read` | 标记已读 | 默认 |
| GET | `/api/v1/user/tickets` | 我的工单 | 默认 |
| POST | `/api/v1/user/tickets` | 提交工单 | 10/m |
| POST | `/api/v1/user/telegram/bind-code` | 获取 TG 绑定码 | 5/m |

### 2.3 管理接口（`auth: admin`）

| 方法 | 路径 | 用途 | 限流 |
|---|---|---|---|
| GET | `/api/v1/admin/users` | 用户列表 | 默认 |
| POST | `/api/v1/admin/users` | 创建用户 | 默认 |
| GET | `/api/v1/admin/users/{id}` | 用户详情 | 默认 |
| PATCH | `/api/v1/admin/users/{id}` | 修改用户 | 默认 |
| POST | `/api/v1/admin/users/{id}/status` | 改状态（封禁/解封/审批） | 默认 |
| POST | `/api/v1/admin/users/{id}/entitlements` | 发放/延长权益 | 30/m |
| POST | `/api/v1/admin/users/{id}/reset-password` | 重置密码 | 10/m |
| POST | `/api/v1/admin/users/batch` | 批量操作 | 10/m |
| GET | `/api/v1/admin/plan-groups` | 权益档位列表 | 默认 |
| POST | `/api/v1/admin/plan-groups` | 新建档位 | 默认 |
| PATCH | `/api/v1/admin/plan-groups/{id}` | 修改档位 | 默认 |
| PUT | `/api/v1/admin/plan-groups/{id}/policy` | 设置片库权限（★ 卖差价的入口） | 默认 |
| GET | `/api/v1/admin/plans` | 套餐列表 | 默认 |
| POST | `/api/v1/admin/plans` | 新建套餐 | 默认 |
| GET | `/api/v1/admin/redeem/batches` | 卡密批次 | 默认 |
| POST | `/api/v1/admin/redeem/batches` | 生成批次 | 5/m（昂贵，批量生成） |
| POST | `/api/v1/admin/redeem/batches/{id}/revoke` | 作废批次 | 30/m |
| GET | `/api/v1/admin/requests` | 求片列表 | 默认 |
| POST | `/api/v1/admin/requests/{id}/retry` | 重试求片 | 30/m |
| POST | `/api/v1/admin/requests/{id}/resolve` | 人工完成 | 30/m |
| GET | `/api/v1/admin/servers` | 媒体服务器列表 | 默认 |
| POST | `/api/v1/admin/servers` | 添加服务器 | 默认 |
| POST | `/api/v1/admin/servers/{id}/test` | 连接测试 | 10/m（昂贵，触发远端调用） |
| GET | `/api/v1/admin/plugins` | 插件列表 | 默认 |
| POST | `/api/v1/admin/plugins/{id}/permissions` | 调整插件权限 | 30/m |
| GET | `/api/v1/admin/jobs` | 任务列表 | 默认 |
| PATCH | `/api/v1/admin/jobs/{id}` | 改调度/开关 | 默认 |
| POST | `/api/v1/admin/jobs/{id}/run` | 立即执行（支持 dry_run） | 10/m |
| GET | `/api/v1/admin/audit` | 审计日志 | 默认 |
| GET | `/api/v1/admin/settings` | 系统设置 | 默认 |
| PUT | `/api/v1/admin/settings` | 保存设置 | 30/m |
| GET | `/api/v1/admin/system/routes` | 已注册的 HTTP 路由清单 | 默认 |

---

## 3. 核心接口详解

### 3.1 登录

```
POST /api/v1/auth/login
Content-Type: application/json
```

**请求**：
```json
{
  "username": "alice",
  "password": "secret123",
  "turnstile_token": "可选，人机验证"
}
```

**成功**：
```json
{
  "code": 200,
  "message": "success",
  "data": {
    "token": "eyJhbGciOi...",
    "expires_at": "2026-10-08T10:12:33Z",
    "user": {
      "id": "01JCVZ8XK2M4N6P8Q0R2S4T6V8",
      "username": "alice",
      "status": "active",
      "can_play": true,
      "is_admin": false,
      "avatar_url": "/api/v1/avatars/01JCVZ...",
      "expires_at": "2026-11-01T00:00:00Z"
    }
  }
}
```

**失败**（注意：用户不存在与密码错误返回**完全相同**的响应）：
```json
{
  "code": 4010,
  "message": "用户名或密码错误",
  "data": { "kind": "invalid_credentials" }
}
```

> **反枚举要求**：用户名不存在 / 密码错误 / 账号被封 这三种情况对**未通过密码校验**的请求必须返回同一响应。账号被封只在密码正确后才告知，否则攻击者可以验证某个用户名是否存在。

### 3.2 提交求片

```
POST /api/v1/user/requests
```

**请求**：
```json
{
  "media_type": "tv",
  "tmdb_id": "1399",
  "title": "权力的游戏",
  "year": "2011",
  "seasons": [1, 2]
}
```

**成功**：
```json
{
  "code": 200,
  "message": "success",
  "data": {
    "requests": [
      { "id": "01JCVZ...", "season": 1, "status": "queued", "points_cost": 50 },
      { "id": "01JCVZ...", "season": 2, "status": "queued", "points_cost": 50 }
    ],
    "points_balance": 350
  }
}
```

**失败**：

| 场景 | HTTP | code | kind |
|---|---|---|---|
| 积分不足 | 400 | 4000 | `insufficient_points` |
| 已入库 | 409 | 4090 | `already_in_library` |
| 重复求片 | 409 | 4090 | `duplicate_request` |
| 超出配额 | 429 | 4290 | `quota_exceeded` |
| 求片功能被暂停 | 403 | 4030 | `request_disabled` |

### 3.3 求片详情（含步骤流水）★

这是体验差异化的关键接口——用户能看到真实进度，而不是笼统的"处理中"。

```
GET /api/v1/user/requests/{id}
```

**响应**：
```json
{
  "code": 200,
  "message": "success",
  "data": {
    "id": "01JCVZ...",
    "title": "权力的游戏",
    "season": 1,
    "status": "transferring",
    "status_message": "正在转存到网盘",
    "points_cost": 50,
    "points_refunded": 0,
    "created_at": "2026-10-05T09:00:00Z",
    "can_cancel": false,
    "can_bump": false,
    "next_bump_at": "2026-10-05T21:00:00Z",
    "steps": [
      { "stage": "queued",         "status": "success", "detail": "工单已创建",           "started_at": "2026-10-05T09:00:00Z", "duration_ms": 12 },
      { "stage": "searching",      "status": "success", "detail": "搜索到 12 个候选资源",  "started_at": "2026-10-05T09:00:12Z", "duration_ms": 3400 },
      { "stage": "selected",       "status": "success", "detail": "已选中最优资源",        "started_at": "2026-10-05T09:00:16Z", "duration_ms": 5 },
      { "stage": "acquiring",      "status": "success", "detail": "已获取资源链接",        "started_at": "2026-10-05T09:00:16Z", "duration_ms": 8200 },
      { "stage": "transferring",   "status": "started", "detail": "转存进度 45%",         "started_at": "2026-10-05T09:00:25Z" }
    ]
  }
}
```

**`can_cancel` 的规则**：进入 `acquiring` 之后不可撤销。这是业务规则，由服务端判定并返回，前端不自行推断。

**`can_bump` 的规则**：距上次顶帖超过冷却期（默认 12 小时）才为 true。

### 3.4 发放权益（管理员）

```
POST /api/v1/admin/users/{id}/entitlements
Idempotency-Key: <必填>
```

**请求**：
```json
{
  "plan_group_id": "01JCVZ...",
  "operation": "extend",
  "duration_days": 30,
  "reason": "客服补偿"
}
```

| 参数 | 说明 |
|---|---|
| `operation` | `extend`（延长）/ `set_date`（指定到期日）/ `permanent`（设为永久）/ `revoke`（撤销） |
| `duration_days` | `operation=extend` 时必填 |
| `expires_at` | `operation=set_date` 时必填 |

**成功**：
```json
{
  "code": 200,
  "message": "success",
  "data": {
    "entitlement": { "plan_group_id": "01JCVZ...", "expires_at": "2026-12-05T00:00:00Z" },
    "policy_sync_status": "pending"
  }
}
```

> **`policy_sync_status` 的意义**：本地已改，远端媒体服务器的权限还没同步成功。前端据此提示"权限同步中"，而不是假装一切正常。这是竞品普遍缺失的可见性。

### 3.5 设置片库权限（卖差价的入口）★

```
PUT /api/v1/admin/plan-groups/{id}/policy
```

**请求**：
```json
{
  "server_id": "01JCVZ...",
  "allowed_libraries": ["lib_movies", "lib_tv"],
  "allow_download": false,
  "allow_transcode": true,
  "max_concurrent": 2,
  "max_parental_rating": 14,
  "blocked_tags": ["成人"]
}
```

**`allowed_libraries` 的语义**：空数组表示不限制（能看到全部片库）。这是刻意的——新建档位时默认全开，管理员再逐步收紧，比默认全关更符合直觉。

### 3.6 卡密批次生成

```
POST /api/v1/admin/redeem/batches
```

**请求**：
```json
{
  "name": "2026年10月 月卡批次",
  "code_type": "renew",
  "plan_group_id": "01JCVZ...",
  "duration_days": 30,
  "count": 100,
  "max_uses_per_code": 1,
  "valid_until": "2026-12-31T23:59:59Z",
  "note": "双十一活动"
}
```

**成功**：
```json
{
  "code": 200,
  "message": "success",
  "data": {
    "batch_id": "01JCVZ...",
    "count": 100,
    "codes": ["A1B2-C3D4-E5F6", "..."],
    "valid_until": "2026-12-31T23:59:59Z"
  }
}
```

> 卡密密文只在生成时返回一次。之后只能查询状态，不能再次读取明文。

### 3.7 兑换卡密

```
POST /api/v1/user/redeem
```

**请求**：`{ "code": "A1B2-C3D4-E5F6" }`

**成功**：
```json
{
  "code": 200,
  "message": "success",
  "data": {
    "type": "renew",
    "granted": { "plan_group_id": "01JCVZ...", "duration_days": 30 },
    "entitlement": { "expires_at": "2026-12-05T00:00:00Z" }
  }
}
```

**失败**：`invalid_code` / `code_expired` / `code_exhausted` / `code_revoked` / `already_redeemed`

### 3.8 任务管理（支持 dry_run）

```
POST /api/v1/admin/jobs/{id}/run
```

**请求**：`{ "dry_run": true }`

**响应**：
```json
{
  "code": 200,
  "message": "success",
  "data": {
    "run_id": 1234,
    "dry_run": true,
    "status": "success",
    "duration_ms": 340,
    "affected": 12,
    "message": "识别出 12 个到期用户，dry-run 模式下未执行禁用操作"
  }
}
```

> **dry_run 是运维刚需**：任何会删数据或发钱的任务，都必须允许先空跑一次看会发生什么。

---

## 4. 实时推送

需要实时性的场景（求片进度、通知）用 **SSE**，不用 WebSocket——SSE 更简单、可穿透反代、自动重连。

```
GET /api/v1/user/events/stream
Accept: text/event-stream
```

**事件格式**：
```
event: request.updated
data: {"id":"01JCVZ...","status":"transferring","status_message":"转存进度 45%"}

event: notification
data: {"id":"01JCVZ...","title":"你的会员即将到期","level":"warn"}
```

需要在请求结束后退订的场景，客户端直接关闭连接即可。

---

## 5. 错误码字典

### 5.1 业务码（`code` 字段）

| code | 含义 | 对应 HTTP |
|---|---|---|
| 200 | 成功 | 200 / 201 |
| 4000 | 参数错误 | 400 |
| 4010 | 未登录或凭证失效 | 401 |
| 4030 | 权限不足 | 403 |
| 4040 | 资源不存在 | 404 |
| 4090 | 状态冲突 | 409 |
| 4290 | 触发限流 | 429 |
| 5000 | 服务端错误 | 500 |
| 5010 | 功能未实现 | 501 |
| 5030 | 依赖不可用 | 503 |

### 5.2 错误类型（`kind` 字段，前端判断依据）

**认证类**
| kind | 触发场景 |
|---|---|
| `unauthenticated` | 未携带凭证或凭证失效 |
| `invalid_credentials` | 用户名或密码错误（含用户不存在） |
| `account_banned` | 账号被封禁 |
| `account_pending` | 账号待审批 |
| `expired` | 账号已过期（该接口需要播放权限） |
| `forbidden` | 权限不足 |
| `invalid_api_key` | 管理 API Key 无效或已撤销（仅 `apikey` 级路由；HTTP 403 + code 4030） |
| `turnstile_failed` | 人机验证失败 |

> **`invalid_api_key` 与 `forbidden` 的分界**：Key 本身无效/已撤销用 `invalid_api_key`；持有有效 Key 但访问其授权范围之外的操作用 `forbidden`（第二轮 D06——此前 `invalid_api_key` 只出现在 §1.4 鉴权规则里，错误字典缺失）。

**用户类**
| kind | 触发场景 |
|---|---|
| `username_taken` | 用户名已存在 |
| `email_taken` | 邮箱已注册 |
| `weak_password` | 密码强度不足 |
| `invalid_email` | 邮箱格式错误 |
| `email_domain_not_allowed` | 邮箱域名不在白名单 |
| `invalid_verify_code` | 验证码错误或已使用 |
| `invite_required` | 需要邀请码 |

**业务类**
| kind | 触发场景 |
|---|---|
| `insufficient_points` | 积分不足 |
| `duplicate_request` | 重复求片 |
| `already_in_library` | 该内容已入库 |
| `quota_exceeded` | 超出配额 |
| `request_disabled` | 求片功能已暂停 |
| `cannot_cancel` | 当前阶段不可撤销 |
| `bump_cooldown` | 顶帖冷却中 |
| `invalid_code` | 卡密无效 |
| `code_expired` | 卡密已过期 |
| `code_exhausted` | 卡密已用尽 |
| `code_revoked` | 卡密已作废 |
| `already_redeemed` | 该码已被此用户兑换 |

**系统类**
| kind | 触发场景 |
|---|---|
| `rate_limited` | 触发限流 |
| `internal_error` | 服务端错误（**不返回详情**） |
| `not_found` | 接口或资源不存在 |
| `media_server_unavailable` | 媒体服务器不可达 |
| `degraded` | 依赖降级（数据可能不完整） |

> **插件错误码**：第三方插件自定义的错误 kind 必须加插件 ID 前缀，如 `my-plugin.custom_error`，避免与内核冲突。

---

## 6. 版本与兼容策略

| 变更类型 | 是否允许 | 做法 |
|---|---|---|
| 新增接口 | ✅ | 直接加 |
| 新增可选请求字段 | ✅ | 直接加 |
| 新增响应字段 | ✅ | 直接加（客户端应忽略未知字段） |
| 删除响应字段 | ❌ | 先标记弃用，至少一个版本后再删 |
| 修改字段语义 | ❌ | 必须升版本 |
| 修改错误 kind | ❌ | 必须升版本 |
| 修改错误 message | ✅ | 随时可改（前端不得依赖） |

**弃用流程**：响应头加 `Deprecation: true` + `Sunset: <日期>`，同时文档标注。

---

## 7. 客户端接入检查清单

给前端与集成方的自查清单：

- [ ] 解析响应统一走 `code`，不用 HTTP 状态码做业务判断
- [ ] 逻辑分支用 `data.kind`，不用 `message`
- [ ] 忽略响应里的未知字段（服务端会加字段）
- [ ] 写操作携带幂等键
- [ ] 401 时清理本地令牌并跳登录页
- [ ] 403 + `kind=expired` 时跳续费页，**不是跳登录页**
- [ ] 429 时读取 `X-RateLimit-Reset` 并退避重试
- [ ] 列表用游标分页，不用页码（除后台表格）
- [ ] 时间按 RFC3339 解析，展示时转本地时区

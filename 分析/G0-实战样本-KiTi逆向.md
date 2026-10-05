# G0 实战样本：某商业 Emby 私服运营系统（KiTi）逆向需求清单

> 样本：`https://mb.8888654.xyz/`（KiTi 私有媒体服务 · 用户中心）
> 方法：前端产物逆向（压缩 bundle）+ 未鉴权 API 探测 + DNS/证书指纹
> 价值：**这不是开源项目的推测，是真实在跑、有人付费的商业系统**。它的功能面 = 这个领域真实付费需求的完整快照。

---

## 1. 技术栈指纹

| 层 | 事实 |
|---|---|
| 前端 | Vue 3 + TypeScript + Vite + **rolldown-vite**（2025 年新构建器） |
| 状态/存储 | 无 Pinia；自研 `sessionUser` 模块 + `sessionStorage`/`localStorage` 双层 |
| UI 体系 | 无 UI 组件库；自研 CSS 变量设计系统（42 个 `--premium-*` + 19 个 `--user-*`）+ Tailwind 原子类 |
| 图标 | Font Awesome |
| 字体 | HarmonyOS Sans SC（本地 woff2，923 KB × 3 字重） |
| 加密 | **node-forge**（281 KB lib chunk）→ 前端 RSA-OAEP-256 加密登录 |
| 后端 | **Go**（404 返回标准库 `404 page not found`） |
| 部署 | Cloudflare 代理（通配符证书 GTS WE1，主域仅 NS/SOA，仅 `mb` 子域存活） |
| 打包 | 26 个 chunk，模块化懒加载，`__vite__mapDeps` 预加载表 |

**结论**：前端是自研闭源（Sourcegraph 全库 0 命中所有独特标识串），后端 Go 自研。

---

## 2. 后端 API 全清单（47 个端点，从产物提取 + 实测）

```
── 认证与账号 ──
POST  /api/v1/auth/login                       登录（RSA-OAEP-256 加密密码）
POST  /api/v1/auth/register                    注册
POST  /api/v1/auth/logout                      登出
GET   /api/v1/auth/me                          当前用户 ★实测 401 {"code":401,"message":"未登录"}
GET   /api/v1/auth/password-key                ★实测公开 RSA 公钥 + key_id 轮换
POST  /api/v1/auth/password                    改密
POST  /api/v1/auth/account/switch              切换配对账号 ★
GET   /api/v1/auth/routes                      线路列表
POST  /api/v1/auth/avatar/upload               头像上传
GET   /api/v1/auth/avatar                      头像
GET   /api/v1/avatars/builtins                 内置头像库
GET   /api/v1/auth/invite/{code}               邀请码校验
POST  /api/v1/auth/use-code                    卡密/续期码核销
GET   /api/v1/auth/card-purchase-link          购卡链接
POST  /api/v1/auth/license/activate            License 激活

── Telegram 绑定 ──
GET   /api/v1/auth/telegram/options            ★实测公开，返回双 Bot
POST  /api/v1/auth/telegram/bind/request       发起绑定
POST  /api/v1/auth/telegram/bind/confirm       确认绑定
GET   /api/v1/auth/telegram/session            会话

── 用户中心 ──
GET   /api/v1/user/home                        首页聚合
GET   /api/v1/notifications                    通知中心
POST  /api/v1/notifications/read               标记已读
GET   /api/v1/points/mine                      积分余额与流水
POST  /api/v1/points/checkin                   每日签到

── 求片 ──
GET   /api/v1/requests                         求片列表
GET   /api/v1/requests/mine                    我的求片
GET   /api/v1/requests/mine/{id}               求片详情
GET   /api/v1/requests/search                  资源搜索
GET   /api/v1/requests/tmdb/{type}/{id}        TMDB 详情 ★后端代理，前端不暴露 key

── 播放与报告 ──
GET   /api/v1/playback/mine                    我的播放记录
GET   /api/v1/playback/report/mine             观影报告
GET   /api/v1/playback/report/mine/items       报告条目

── 资源反馈 ──
GET   /api/v1/resource-reports                 反馈列表
GET   /api/v1/resource-reports/mine            我的反馈
```

**实测**：`/api/v1/system/status`、`/api/v1/subscribe`、`/api/v1/downloader` 等 MoviePilot 特征路径**全部 404** → MP 部署在别处，不暴露公网。

---

## 3. 用户中心功能面（9 个页面）

| 页面 | 路由 | 功能 |
|---|---|---|
| 首页 | `/home` | 积分卡片、排行榜、在看/追剧、最近入库、签到入口 |
| 求片中心 | `/requests` | TMDB 搜索、季选择、批量求片、工单队列、撤销、免费顶帖 |
| 资源反馈 | `/feedback` | 片源问题反馈 + 消息时间线 |
| 我的播放 | `/playback` | 播放历史 |
| 观影报告 | `/watch-report` | 月度趋势图、时长统计 |
| 线路入口 | `/routes` | 多线路选择（普通/白名单/Pro 线路），独占线路标识 |
| 积分中心 | `/points` | 签到、积分流水、消耗规则 |
| 通知中心 | `/notifications` | 公告、系统通知 |
| 个人中心 | `/profile` | 资料、头像、密码、TG 绑定、卡密核销、账号切换 |

移动端有独立底部导航（首页/求片/播放/报告/我的）。

---

## 4. 账号体系（真实商业分级）

```javascript
// accountAccess.js 原文
[{ value:'normal',    label:'普通用户'   },
 { value:'whitelist', label:'白名单用户' },
 { value:'pro',       label:'Pro 用户'   }]
```

| 维度 | 取值 |
|---|---|
| **账号类型** | `normal` / `whitelist` / `pro` / `admin` / `invalid`（异常态，`is_vip && is_pro` 同时为真时判为冲突） |
| **线路类型** | `normal` / `whitelist` / `pro`（`whitelist_only` / `pro_only` 互斥校验） |
| **账号状态** | `active` / `disabled` / `pending`（等待审核） |
| **配对账号** | `paired_account` —— **一个用户绑定两个身份，可一键切换** |
| **访问版本** | `access_version` —— 权限策略版本号（用于失效旧会话） |
| **下载权限** | `allow_downloads` |
| **转码权限** | `allow_video_transcode` / `allow_audio_transcode` |
| **并发限制** | `max_concurrent` |
| **家长控制** | `max_parental_rating` / `block_unrated` / `blocked_tags` |
| **片库范围** | `allowed_libraries`（多片库隔离） |
| **线路范围** | `allowed_routes` + `route_exclusive`（独占线路） |
| **求片配额** | `request_mode` / `request_quota` |
| **到期** | `expire_at` |

**这张表是整个领域最完整的需求清单**——每个字段都是一个真实存在的运营需求。

---

## 5. 求片状态机（11 个状态，这是体验的核心）

从 `UserRequests.js` 提取的完整状态串：

```
排队中 → 搜索中 → 已选资源 → 解锁中 → 转存中 → 等待 Emby 入库 → 自动补位中
                                                    ↓
              没有资源 / 等待资源补齐 / 等待授权 / 等待人工处理 / 自动处理失败
```

| 状态 | 含义 | 背后的技术动作 |
|---|---|---|
| 排队中 | 工单入队 | 扣积分，落库 |
| 搜索中 | 找资源 | 多 Provider 并发搜索 |
| 已选资源 | 选定候选 | 按质量/大小/做种数排序择优 |
| **解锁中** | 获取资源链接 | 网盘资源站解锁（付费/积分） |
| **转存中** | 转存到自己的网盘 | 115 转存 |
| 等待 Emby 入库 | 落库待扫描 | strm 生成 / 文件移动 |
| 自动补位中 | 缺集补齐 | 订阅巡检 |
| 等待授权 | 凭证过期 | Cookie/Token 需重授权 |
| 等待人工处理 | 自动失败 | 转人工 |

**配套交互**（都是真金白银打磨出来的）：
- 求片扣积分，撤销返还（"成功撤销后返还 N 积分"）
- 进入转存/订阅阶段后**不可撤销**（"进入外部转存或订阅阶段后将不能自动撤销"）
- **免费顶帖**：12 小时内可再次提醒管理员，不扣积分、不创建新工单
- 多季合并工单，可整组撤销
- 入库复核："复核发现 N 条已入库"

---

## 6. 资源获取链路（前端漏出的内部词汇）

```javascript
// UserRequests.js 中用于过滤 admin_note 的内部关键词表
['自动处理','影巢','hdhive','moviepilot','mp ','115','转存','兜底','重试','资源候选']
```

**完整链路还原**：

```
用户求片
   ↓
Go 后端工单系统（自研）
   ├─ 路径 A：MoviePilot 订阅 → 自动搜索 → qB/Transmission → 入库
   ├─ 路径 B：影巢 HDHive → 解锁 → 115 网盘转存 → strm → 入库
   └─ 路径 C：兜底 / 重试 / 人工介入
   ↓
Emby 入库 → 复核 → 工单完成
```

> 注：`hdhive`（影巢）+ `moviepilot` 是圈内成熟组合，存在现成插件链 `hdhive-search-unlock-to-115`。

---

## 7. 其他运营要素

| 要素 | 事实 |
|---|---|
| **双 Telegram Bot** | `@kitiprobot`（Pro 用户 Bot）+ `@kitiyh_bot`（普通用户 Bot）——**按账号等级分流** |
| **前端加密登录** | RSA-OAEP-256，公钥带 `key_id` 支持轮换，防明文密码与重放 |
| **CDN/线路** | 多线路 + 独占线路 + 账号等级匹配 |
| **邀请体系** | `/api/v1/auth/invite/{code}` 邀请码校验 + 卡密核销 |
| **购卡闭环** | `card-purchase-link` 直接给购卡链接（商业化关键） |
| **License 激活** | `license/activate` 独立端点 |
| **安全加固** | 严格 CSP（`default-src 'self'`）、`frame-ancestors 'none'`、HSTS、`noindex`、COOP、Permissions-Policy 全面收紧 |
| **隐私** | 未鉴权接口全部 401，仅 `/auth/password-key` 与 `/auth/telegram/options` 公开（且不含敏感信息） |

---

## 8. 从这份样本提炼的「刚需清单」

按「实现成本 × 用户感知」排序，这是 EmbyOne 的功能优先级依据：

| 优先级 | 需求 | 现有开源项目覆盖情况 |
|---|---|---|
| P0 | 用户注册/登录/Emby 建号/到期封禁 | ✅ 覆盖好（ember/Twilight） |
| P0 | 用户自助门户（播放记录、报告） | ⚠️ 部分（ember 有，pulse 只读） |
| P0 | 求片工单 + 资源搜索 + 进度可见 | ⚠️ 都有但都不完整 |
| P0 | 多线路/入口选择 | ❌ 只有 Meridian 做反代，但无用户端 |
| P1 | 积分体系 + 签到 | ⚠️ TG Bot 里有，Web 端少见 |
| P1 | 卡密/兑换码/邀请码 | ✅ ember 做得好 |
| P1 | Telegram 绑定与运营 | ✅ 覆盖好 |
| P1 | 账号等级 + 差异化权限 | ⚠️ 有但是硬编码 |
| P1 | 片库/线路/转码/并发粒度授权 | ⚠️ 部分（ember 有 plan group） |
| P2 | 资源反馈 + 消息时间线 | ❌ 几乎没有 |
| P2 | 观影报告（可视化） | ⚠️ pulse 有部分 |
| P2 | 配对账号 / 多身份切换 | ❌ 独有需求 |
| P2 | 签到/顶帖等运营小交互 | ❌ 几乎没有 |
| P2 | 购卡链接/支付闭环 | ⚠️ 只有 ember 接了 Stripe |

**空位非常明显**：P1/P2 里有大量需求只有商业系统做了，开源世界基本是空白。这就是 EmbyOne 的机会。

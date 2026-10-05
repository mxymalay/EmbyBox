# G2 Telegram 机器人与保活类项目分析

分析对象为本地只读样本（`参考项目/` 下三个 clone）。全部结论来自逐文件阅读，行号与路径均可复查。

| 项目 | 代码量 | 语言/栈 | 数据库 | 形态 | 与 Emby 的关系 |
| --- | --- | --- | --- | --- | --- |
| sakura-embyboss | 15,943 行 py（86 文件） | Pyrogram 2 + pyromod + SQLAlchemy 2 + Alembic | MySQL 5.7 | Docker（bot+mysql）、systemd | 站长端：发号/续期/封禁/风控中枢 |
| lemon-emby-bot | 2,457 行 py（15 文件） | python-telegram-bot 20 + aiosqlite + FastAPI | SQLite（WAL） | Docker 单文件、内嵌 Web 面板 | 站长端轻量运营中控 + 积分游戏化 |
| emby-keeper | 308 py + 40 vue | Pyrogram(kurigram) + Telethon + ddddocr + Flask/SocketIO + FastAPI + Vue3 + typer | 无 DB（TOML + 缓存 + KV） | CLI / 网页终端 / Web API 三形态、Docker、Windows 安装器 | 用户端：被服务者的自动化打工人 |

三者构成一条完整生态链：**sakura-embyboss 发号 → lemon-emby-bot 运营留存 → emby-keeper 替用户签到保活**。emby-keeper 甚至内置了直接注册 sakura-embyboss 的适配器（见 3.2）。

---

## 1. sakura-embyboss

### 1.1 定位与技术栈

代码 `参考项目/sakura-embyboss/`。一个单体 Pyrogram Bot，承担「Emby 合租站运营后台」的全部职责，权限体系分四层：**未注册用户 / 普通用户 lv=b / 白名单 lv=a / 封禁 lv=c**，外加 `admins` 与 `owner` 两个 Telegram 身份层。

关键事实（均为代码依据）：

- 客户端：`bot/__init__.py:152` `bot = Client(...)`，`workers=300, max_concurrent_transmissions=1000`，`parse_mode=MARKDOWN`；用 **pyromod 的 `Client`** 拿到 `callListen / ask_return` 这种「会话式等待用户回复」能力，这是它交互设计的基石。
- 命令前缀：`bot/__init__.py:33` `prefixes = ['/', '!', '.', '，', '。']`（中文标点也接受，防手机端输入法误触）。
- 配置：`config.json` + pydantic 模型 `bot/schemas/schemas.py`，启动即 `Config.load_config()` 并 `save_config()` 回写；`Open` 模型承载**所有运营开关**（`stat` 自由注册、`checkin`、`exchange`、`whitelist`、`invite`、`leave_ban`、`client_filter_*`、积分价格 `exchange_cost=300 / whitelist_cost=9999 / invite_cost=1000 / srank_cost=5`）。
- 数据库：`bot/sql_helper/__init__.py` 建 `mysql+pymysql://...?charset=utf8mb4`，`pool_size=16, pool_recycle=1800`；启动时调 `run_migrations()` 自动跑 Alembic，并有 `_legacy_create_all_tables()` 兜底 `create_all`。
- Web 层：可选 FastAPI（`bot/web/__init__.py`，`api.status=False` 默认关闭），暴露 `/emby/*` webhook 接收（Emby 插件回传），鉴权方式是**把 bot_token 当 token 用**（`bot/web/api/__init__.py` 的 `verify_token`）。
- 部署：`docker-compose.yml` 里 mysql:5.7 + `jingwei520/sakura_embyboss:latest`，`network_mode: host`，config.json 与 log 目录挂载；另有 `embyboss.service`（裸机 systemd）。

### 1.2 解决的用户故事

| 角色 | 故事 |
| --- | --- |
| 站长 | 「我开了一个 Emby 合租站，300 个用户，每天有人到期有人跑路。我要发注册码控量、到期自动封禁/删除、定期备份数据库、看谁在偷号多开、群发通知、一键给所有人加天数。」 |
| 用户 | 「我要私聊机器人开号、看自己到期时间、每天签到拿积分、用积分换续期/白名单/邀请码、在群里抢红包看排行榜。」 |
| 群管理 | 「我要在群里直接发红包、看观影榜、踢掉不在群里的号、拒绝皮套人（频道身份广告）刷屏。」 |
| owner | 「我要一键全站封禁/解封、一键清空所有人积分、跑路时删掉所有用户（真的有 `/paolu`）。」 |

### 1.3 完整指令表

命令定义源：`bot/__init__.py:76-141`（三套 `BotCommand`），注册源分散在 `bot/modules/**`，用 `bot/scheduler/bot_commands.py` 按 Telegram scope 下发（管理员在私聊/群成员维度各一套，提权降权时即时刷新）。

**用户级（`user_p`，所有私聊与群聊可见）**

| 指令 | 作用 | 实现文件 |
| --- | --- | --- |
| `/start` | 私聊开启用户面板；`/start <code>` 走注册码/续期码核销深链 | `bot/modules/commands/start.py:48` |
| `/myinfo` | 群内查自己状态 | `start.py:31` |
| `/count` | 媒体库数量（`/emby/Items/Counts`） | `start.py:40` |
| `/red <金额> <份数> [1\|2] [祝福语]` | 群内发红包（拼手气/均分/回复他人=专属） | `bot/modules/extra/red_envelope.py:88` |
| `/srank` | 积分风云录榜单（扣 `srank_cost` 手续费，不足则**禁言 1 分钟**） | `red_envelope.py:406` |

**管理员级（`admin_p` = 用户级 + 39 条独有，累计 44 条）**

| 分组 | 指令 |
| --- | --- |
| 用户管理 | `/kk`（用户卡片：封禁/解封/送天数/额外媒体库）`bot/modules/panel/kk.py:18`、`/score`（加减小数积分 `us`）、`/coins`（加减 sakura_b `iv`）`bot/modules/commands/score_coins.py`、`/deleted`（清理死号）、`/kick_not_emby`（踢出群内无号崽）、`/syncgroupm`（消灭不在群的人）、`/syncunbound`（消灭未绑定 bot 的 Emby 账户）、`/scan_embyname`（同名账户扫描）、`/embyadmin`（给 emby 控制台权限）`bot/modules/commands/syncs.py` |
| 到期与续期 | `/renew`（单人调整到期）、`/renewall`（**一键给所有未封禁用户派天数**）、`/check_ex`（手动到期检测）、`/low_activity`（手动活跃检测）`bot/modules/commands/renew.py`、`renewall.py`、`bot/panel/sched_panel.py` |
| 积分发放 | `/coinsall`（按等级一键发币）、`/coinsclear`（**一键清零所有积分**）`renewall.py:60,122` |
| 账号增删 | `/rmemby`（删 tg+emby）、`/only_rm_emby`、`/only_rm_record`、`/ucr`（私聊创建非 TG 的 Emby 用户）、`/urm`、`/uinfo` | `bot/modules/commands/rmemby.py`、`bot/modules/extra/create.py` |
| 权限 | `/prouser`（加白名单）、`/revuser`（去白名单）、`/white_channel`、`/rev_white_channel`、`/unban_channel`（皮套人白名单） | `bot/modules/commands/pro_rev.py`、`bot/modules/extra/antichanel.py` |
| 审计 | `/userip`（某用户播过的设备+IP）、`/udeviceid`、`/auditip`、`/auditdevice`、`/auditclient`、`/watching`（当前观看状态） | `bot/modules/extra/create.py:250,308`、`bot/modules/commands/audit.py`、`watching.py` |
| 榜单 | `/days_ranks`、`/week_ranks`、`/uranks`（观影时长榜，失效时手动召唤）、`/sync_favorites` | `bot/modules/panel/sched_panel.py` |
| 媒体库 | `/embylibs_blockall` / `unblockall` / `extraembylibs_*` | `bot/modules/commands/emby_libs.py` |
| 运维 | `/restart`、`/update_bot`（git 拉取自动更新）、`/callall`（群发）、`/config`（高级控制面板）、`/check_ex` | `bot/modules/panel/config_panel.py` |

**Owner 级（`owner_p` = 管理员级 + 11 条，累计 55 条；配置了 `extra_emby_libs` 时再追加 2 条）**

`/proadmin`、`/revadmin`（增删管理）、`/bindall_id`（一键补全所有人的 embyid）、`/backup_db`、`/unbanall`、`/banall`、`/paolu`（**删除所有用户**）、`/restore_from_db`（从备份恢复 Emby 账户）、`/embylibs_*`。

**回调（CallbackQuery）表** — 面板实质是「命令 + inline 键盘」双轨制，callback_data 分布在 8 个面板文件中：

| 前缀 | 用途 | 文件 |
| --- | --- | --- |
| `members` / `create` / `changetg` / `bindtg` / `delme` / `delemby` / `reset` | 用户面板：开号、换绑 TG、删号、重置密码 | `bot/modules/panel/member_panel.py` |
| `embyblock` / `emby_block` / `emby_unblock` | 用户自控媒体库 | 同上 |
| `storeall` / `store-reborn` / `store-whitelist` / `store-invite` / `store-query` | **积分商城**（续期解封 / 白名单 / 邀请码 / 码查询） | `member_panel.py:611-744` |
| `my_favorites` / `my_devices` | 我的收藏（剧集更新订阅）、我的设备 | `member_panel.py:744,783` |
| `manage` / `open-menu` / `open_stat` / `open_timing` / `all_user_limit` / `open_us` | 管理面板：注册开关、定时注册、人数上限 | `bot/modules/panel/admin_panel.py` |
| `cr_link` / `ch_link` / `delete_codes` / `ch_admin_link` / `register_*` / `pagination_keyboard` | **码务中心**：生成/统计/分页/删除 | `admin_panel.py:320-501` |
| `set_renew` / `set_freeze_days` / `set_invite_lv` / `set_checkin_lv` | 运营参数开关 | `admin_panel.py:504-589` |
| `config` / `partition_code_panel` / `partition_code_action_*` / `set_line` / `set_whitelist_line` / `set_block` / `set_mp*` / `set_tz*` / `set_client_filter*` / `toggle_client_filter` / `set_activity_check_days` / `set_fuxx_pitao` / `set_red_envelope_*` | 高级配置面板（线路、屏蔽库、MoviePilot 对接、探针、客户端过滤、活跃天数、红包开关） | `bot/modules/panel/config_panel.py`（878 行，全项目最大单体面板） |
| `sched-*` | 定时任务开关面板 | `bot/modules/panel/sched_panel.py:77` |
| `server` | 线路/密码展示 | `bot/modules/panel/server_panel.py` |
| `download_center` / `get_resource` / `mp_search_*` / `request_record_*` | MoviePilot 求片/下载中心 | `bot/modules/panel/request_movie_panel.py` |
| `checkin` | 每日签到 | `bot/modules/callback/checkin.py:13` |
| `red_envelope-<id>` / `users_iv:<page>_<tg>` | 抢红包、榜单翻页 | `bot/modules/extra/red_envelope.py` |
| `user_ban` / `gift` / `closeemby` / `fuckoff` | kk 用户卡片操作 | `bot/modules/panel/kk.py` |
| `uinfo_disable-` / `uinfo_enable-` / `uinfo_delete-`(+confirm/cancel) | 非 TG 用户管理 | `bot/modules/extra/create.py` |

**非命令入口**：`/start <code>` 深链核销（`start.py:56-67`，先拦 `userip-<name>` 特例，再校验 `u in ranks.logo or u == str(uid)`）、`/callall` 私聊群发、webhook HTTP 端点。

### 1.4 数据模型

Alembic 初版即全量 DDL（`bot/sql_helper/alembic/versions/20260315_01_init_schema.py`），7 张表：

```sql
-- 用户主表：TG 用户与 Emby 账户的唯一映射
CREATE TABLE `emby` (
  `tg` BIGINT PRIMARY KEY,      -- Telegram user id（主键，也就意味着一个 TG 只能一个号）
  `embyid` VARCHAR(255),        -- Emby user id
  `name` VARCHAR(255),          -- Emby 用户名（开号时用户自己填，无唯一约束）
  `pwd` VARCHAR(255),           -- 系统生成的登录密码（明文）
  `pwd2` VARCHAR(255),          -- 用户填的 4~6 位「安全码」= 密码重置/换绑二次验证凭据（明文）
  `lv` VARCHAR(1),              -- a 白名单 / b 正常 / c 封禁(封存期) / d 未注册
  `cr` DATETIME,                -- 创建时间
  `ex` DATETIME,                -- 到期时间
  `us` INT,                     -- 「积分」：天数余额，自动续期时按 30 天抵扣
  `iv` INT,                     -- 「sakura_b」币：签到/榜单/红包产出的虚拟币
  `ch` DATETIME                 -- 最后签到时间（签到的唯一性判断依据）
);
```

```sql
CREATE TABLE `emby2` (  -- 非 TG 通道账户（管理员 /ucr 创建或换绑来源），主键是 embyid
  `embyid` VARCHAR(255) PK, `name`, `pwd`, `pwd2`, `lv`, `cr`, `ex`, `expired` INT
);
CREATE TABLE `Rcode` (  -- 唯一的码表，三种码共表
  `code` VARCHAR(50) PK, `tg` BIGINT,   -- 发放者
  `us` INT,                             -- 天数
  `used` BIGINT, `usedtime` DATETIME    -- 使用者与时间（NULL=未使用）
);
CREATE TABLE `request_records` (  -- MoviePilot 求片记录（download_id, tg, request_name, cost, detail, state, progress...）
CREATE TABLE `emby_favorites` (   -- 收藏订阅：UNIQUE(embyid,item_id)，用于剧集更新提醒
CREATE TABLE `partition_codes` (  -- 分区码：code, partition, duration_days, created_by
CREATE TABLE `partition_grants` ( -- 分区授权实例：tg, embyid, embyname, partition, expires_at, status, code
```

设计要点与缺陷：

- **两套币**：`us`（天数）与 `iv`（虚拟币）分离，`us` 是硬通货（1:30 天），`iv` 是软币，运营价格全部以 `iv` 计价。这个分离很好，但 `us` 的充贬值只有 Code 表一条路径。
- **码系统只有一张表 + 字符串前缀编码类型**，没有 `type` 字段、没有批次、没有有效期、没有作废状态、没有生成数量上限。类型识别全靠 `"Register" in code` / `"Renew" in code` / `"Whitelist" in code`（`bot/modules/commands/exchange.py:18-26`，`sql_code.py:119-153` 用 `Code.code.contains(keyword)` 做统计）。
- `partition_codes` / `partition_grants` 是后加的**分区付费**能力（按媒体库分组售卖），有独立的 grant 生命期与到期回收定时任务，是全项目设计最现代的一块。

### 1.5 核心流程

**A. 开号（两条通道 + 队列 + 回滚）**

```
用户点【创建账户】(member_panel.py:110 create)
  → 资格判定：_open.stat（自由注册）为真 或 自身 us>0（有注册码余额）
  → pyromod 会话等待 120s 输入「[用户名][空格][安全码]」(create_user:33)
  → RegisterJob 入队 (bot/func_helper/register_queue.py)
  → 5 个 worker（register_worker_count）异步取单
  → 槽位原子校验：_open.tem + reserved >= _open.all_user → slot_full
  → emby.emby_create(name, days) → POST /emby/Users/New + Password + Policy
  → 二次读库校验（防并发覆盖）→ 写库 → 失败则 rollback_created_account() 删远端号
```

值得抄的三点：`asyncio.Queue` + 信号量式槽位预留（`_reserved_slots`）、每个用户 `get_user_lock()` 互斥、**先远端建号再回写 DB，任何一步失败都调 `emby_del` 回滚**（`register_queue.py:185`）。这条链路是全项目工程质量最高的地方。

**B. 续期（三条通道）**

1. **续期码**：`/start SAKURA-90-Renew_xxxxxxxxxx` → `_redeem_renew_code_atomic()`（`exchange.py:82`）。`with_for_update()` 行锁 + `code.used is None` 判重，原子写 `code.used/usedtime`，到期时间 `ex = max(now, ex) + days`；若原本是 lv=c（封存中）则顺手改回 `lv=b` 并调 `emby_change_policy(disable=False)` 解封。
2. **到期自动用积分续**：定时任务 `check_expired()`（`bot/scheduler/check_ex.py:16`）中，lv=b 且 `us>=30` → 扣 30 天 + 延期 30 天；否则若开 `exchange` 且 `iv>=exchange_cost(300)` → 扣币延期 30 天。
3. **管理员**：`/renew`、`/renewall`（批量给所有未封禁用户加天数）。

**C. 封禁（四态状态机 + 两段式清退）**

```
lv=b 到期 且 (us<30 且 iv<300)          → emby 禁用(IsDisabled=True) → lv=c，进入「封存期」
lv=c 到期后 freeze_days(默认5天) 内续期   → 解封 → lv=b
lv=c 超过 freeze_days                   → emby_del 删除远端号 → 清库(embyid/name/ex 置空) → lv=d
```

`check_ex.py` 对 `emby2` 表（非 TG 用户）另有一套 `expired` 打标逻辑。活跃度维度另有 `check_low_activity()`（`bot/scheduler/userplays_rank.py:108`）：扫 Emby `/Users` 的 `LastActivityDate`，lv=b 且 `activity_check_days`（默认 21 天）未活跃 → 禁用降 c；lv=c 且禁用后 15 天未解禁 → 直接删号。**「到期」和「不活跃」用同一个 lv=c 表示**，这是状态机的一个混叠点，运营时无法区分封禁原因。

**D. 与 Emby 的交互面**

`bot/func_helper/emby.py`（1479 行，aiohttp 重写版，单例 + 3 次重试 + 统一 `EmbyApiResult`）。实际调用：

| Emby API | 用途 |
| --- | --- |
| `POST /emby/Users/New`、`POST /Users/{id}/Password`、`POST /Users/{id}/Policy` | 建号（Policy 由 `create_policy()` 生成：`SimultaneousStreamLimit=2`、`BlockedMediaFolders=['播放列表']+extra_emby_libs`、关转码/下载/删除） |
| `DELETE /emby/Users/{id}` | 删号 |
| `GET /emby/Users`、`GET /Users/{id}`、`GET /Users/Query?NameStartsWithOrGreater=` | 用户查询 |
| `GET /emby/Library/VirtualFolders` | 媒体库列表 → 与 `partition_libs` 配置映射 |
| `GET/POST /emby/Users/{id}/Policy` | 单库开关（`hide_folders_by_names` / `show_folders_by_names` / `enable_all_folders_for_user`） |
| `GET /emby/Sessions` | 在线会话（`get_current_playing_count`） |
| `POST /emby/Sessions/{sid}/Playing/Stop` + `/Sessions/{sid}/Message` | **终止会话并弹提示**（`terminate_session`，客户端风控用） |
| `POST /emby/Users/AuthenticateByName` | 改绑时校验用户密码 |
| `POST /emby/user_usage_stats/submit_custom_query` | 观影统计、IP/设备/客户端审计（`get_emby_report` / `get_emby_userip` / `get_users_by_ip` / `get_users_by_device_name` / `get_users_by_client_name`） |
| `GET /emby/Items/Counts`、`/emby/Items?IncludeItemTypes=...`、`/Items/{id}/Images/Primary` | 媒体数、求片搜索、海报 |
| `POST /emby/Users/{id}/FavoriteItems/{item_id}`、`GET /Users/{id}/Items?Filters=IsFavorite` | 收藏同步 |
| `GET /emby/Devices/Info?Id=` | 设备 ID 反查 |

**WEBHOOK 侧**（需 Emby 插件回传，`bot/web/api/webhook/`）：`client_filter.py` 拦截可疑 UA（默认黑名单含 curl/wget/python/aria2/ffmpeg/vlc 等 12 条正则，也支持 whitelist 模式），命中即 `terminate_session` 并可选择 `emby_change_policy(disable=True)` + 降 lv=c，同时给管理群发拦截卡片并**转发给用户本人**；`line_report.py` 校验线路权限；`favorites.py` / `media.py` 做剧集更新推送与收藏同步。

### 1.6 亮点

1. **注册队列的三重防超卖**：worker 数、队列上限、`tem + reserved >= all_user` 槽位双计数（`register_queue.py:39-73`），并发抢注不会超出总席位；并带远端回滚。
2. **码务中心的运营粒度**：不仅能按天/数量/类型生成，还能按管理员维度统计「注册/续期/白名单 未用/已用」（`sql_count_code_types`），支持按类型+天数批量删除未使用码（`sql_delete_unused_by_days`），owner 专属。这是同类项目里唯一做了码库存管理的。
3. **积分商城四件套**（`member_panel.py:611-744`）：积分自动续期开关展示、`store-reborn` 用积分解封（限「因不活跃被封」场景，避免浪费）、白名单购买（9999）、邀请码自助签发（1000/30天，支持深链或纯码，`cr_link_one` 复用管理端逻辑）。商城把「消费」和「拉新」做成了一个闭环。
4. **红包是原生群内玩法**：拼手气/均分/回复专属三种模式，内存态 `red_envelopes` dict，抢完自动生成结算图；拼手气算法 `int(random.uniform(1, 2*rest_money/rest_members))`（`red_envelope.py:255`）是标准二倍均值法，末位包干，总额守恒。
5. **排行榜是渲染出来的海报**：`bot/ranks_helper/ranks_draw.py` 用 PIL 绘制带底图、遮罩、自定义字体的观影榜/周榜/红包封面，并带翻页键盘。运营传播力上了一个档次。
6. **客户端风控**：不仅能踢会话，还能区分 UA 黑/白名单模式、是否封号、是否只记录，并把拦截事件**同时推给管理群与被拦截用户**（`client_filter.py:90-136`）。
7. **皮套人反广告**：`bot/modules/extra/antichanel.py` 监听群消息，识别 `sender_chat`（频道身份发言）并封禁，白名单可豁免。
8. **审计三件套**：按 IP / 设备名 / 客户端名反查所有关联用户（`audit.py`，409 行），是真实运营中揪共享号的主力工具。

### 1.7 缺陷

1. **卡密系统没有批次概念**。无 `batch_id`、无 `expires_at`、无 `revoked`、无「一个码只能用一次以外」的任何状态；类型靠字符串子串判断（`"Renew" in code`），一旦有人把 logo 起名含 "Renew" 就会全线错乱。生成数量无上限（`_parse_create_code_input` 只校验 `count > 0`）。
2. **明文密码**。`emby.pwd` / `pwd2` 直接明文入库并在 TG 里明文发送（`register_queue.py:171`「安全密码（仅发送一次）」其实库里一直存着）。
3. **状态机混叠**：不活跃封禁与到期封禁共用 `lv=c`，`store-reborn` 只能靠用户自觉「别在到期状态下用」。
4. **资金操作无统一事务**：`/score` `/coins` `/coinsall` `/coinsclear` 都是「读-改-写」（`score_coins.py:65` `us = e.us + b` 后 `sql_update_emby`），并发下会丢更新；只有码核销用了 `with_for_update()`。
5. **无任何自动化测试**（仓库无 tests 目录）。资金/开号这类高危路径零回归保护。
6. **全局可变状态满天飞**：`bot/__init__.py` 把 config 拆成 40 多个模块级变量，`_open.tem` 这种计数器存在内存里（重启丢计数），`register_queue` 单例、`cacheout` 缓存都在进程内 → **无法水平扩容**，多副本会重复发码/重复续期。
7. **代码卫生**：大量裸 `except:` 与 `print(e)` 调试残留（`sql_code.py`、`red_envelope.py`），`sql_emby.py:38` 直接吞异常。日志虽用 loguru，但无结构化事件。
8. **依赖偏重**：numpy + Pillow + alembic + uvicorn + fastapi 全塞进一个 bot 进程（`requirements.txt`），镜像体积与内存占用对 1C1G 小鸡不友好。
9. **`/paolu`（删所有用户）无二次确认强度**，一句话命令即全站清空。
10. **多开防护依赖 Emby 自身**（Policy 里 `SimultaneousStreamLimit=2`），没有设备指纹/同 IP 多账号关联分析，只有事后审计。

---

## 2. lemon-emby-bot

### 2.1 定位与技术栈

代码 `参考项目/lemon-emby-bot/`，仅 2,457 行，README 自称「运行时内存仅 ~20MB，专为 512MB 小鸡优化」。它的取舍很明确：**不要面板海洋，要群内快操作 + 经济系统 + 风控**。

- Bot：`python-telegram-bot` v20 async（`bot/telegram_bot.py:21 LemonEmbyBot`），单文件 1,195 行承载全部 25 条命令 + 1 个 `CallbackQueryHandler`。
- DB：`aiosqlite` + `PRAGMA journal_mode=WAL`（`core/database.py:19`），三张表手写 DDL，无迁移工具。
- Emby：`core/emby.py` 125 行 aiohttp 客户端，**模板用户策略克隆**（`template_user_id` → 复制其 Policy，并强制 `IsAdministrator=False`，`emby.py:80`）。
- 调度：`core/scheduler.py` 自研 126 行 while+sleep 巡检循环，周期由 `rules.check_interval_minutes`（默认 5 分钟）配置。
- Web：FastAPI 单页控制台（`web/api.py` + `web/static/index.html`），与 bot 同进程 `asyncio.gather`（`main.py:71-77`）。
- 测试：`tests/test_pytest_suite.py` 7 个用例（登录生命周期、**防双花**、**卡密原子核销**、小游戏、转账+榜单、Web API 安全）。

### 2.2 解决的用户故事

- **站长（技术但不折腾）**：「我没有 MySQL，就一台小鸡。我要在群里回复一下就把号开了，别让我登服务器。」
- **用户**：「我在群里点按钮就能签到、抽奖、掷骰子、打劫群友；有积分就换时长和并发设备数；有卡密就兑换。」
- **运营者**：「我要把用户留存在群里，用签到+游戏把日活拉起来，用积分商城把积分回收，用卡密售卖续期。」

### 2.3 完整指令表

注册源：`bot/telegram_bot.py:59-96`。

**用户指令（13 条，含别名）**

| 指令 | 别名 | 说明 |
| --- | --- | --- |
| `/start` | — | 主控面板（8 按钮 + 管理员多 1 个） |
| `/help` | — | 指令大全（按是否管理员动态拼接） |
| `/my` | — | 账号档案（到期/积分/设备上限/服务器地址） |
| `/bind <用户名> <密码>` | — | **自助开号或绑定**（仅私聊，群聊拒绝处理凭据） |
| `/redeem <卡密>` | — | 卡密核销 |
| `/resetpw <新密码>` | — | 自助改 Emby 密码 |
| `/checkin` | — | 每日签到（时长 + 积分双奖励） |
| `/shop` | `/store` | 积分商城（4 商品） |
| `/lottery` | `/draw` `/chou` | 幸运大转盘（20 分/次） |
| `/transfer <用户> <数量>` | `/pay` `/sendpts` | 积分转账 |
| `/rank` | `/top` `/leaderboard` | 积分 TOP10 |
| `/dice [押注]` | `/touzi` | 回复他人=发起 PK；直接发=人机对战 |
| `/rob` | `/steal` `/qiang` | 回复他人打劫积分 |
| `/duel` | `/pk` | 等价 `/dice` |
| `/info [用户名/TG_ID]` | `/check` `/whois` `/user` | 查号（支持回复他人） |

**管理员指令（11 条）**

| 指令 | 说明 |
| --- | --- |
| `/create [天数] [密码]`（回复群友） | **群内一键开号**，自动取被回复者 TG ID，私聊推送地址/账号/密码/客户端指引 |
| `/create <用户名> [密码] [天数] [TG_ID]` | 手动指定参数开号 |
| `/deluser` | 一键销号（Emby + DB） |
| `/gen <天数> [张数]` | 批量生成卡密（1 张 ~ 50 张，天数 1~3650） |
| `/status` | 服务器状态 + 活跃播放会话 |
| `/users` | 用户列表 |
| `/ban` / `/unban <用户>` | 封禁/解封 |
| `/addtime <用户> <天数>` | 手动加时长 |
| `/addpts` / `/delpts` | 发放/扣减积分（支持回复触发） |

**回调（`cb_*`，共 16 个）**：`cb_my`、`cb_checkin`、`cb_shop`、`cb_lottery`、`cb_buy_days_7/30/90`、`cb_buy_dev_plus1`、`cb_game_dice_menu`、`cb_rank`、`cb_redeem_info`、`cb_lines`、`cb_clients`、`cb_refresh`、`cb_admin_panel`、`cb_main_menu`、`cb_accept_duel_<id>`、`cb_reject_duel_<id>`（`telegram_bot.py:105-145, 994+`）。

### 2.4 数据模型

```sql
users( tg_id PK, emby_user_id, emby_username UNIQUE, expiry_date TEXT,
       points INT DEFAULT 0, max_devices INT DEFAULT 2, is_disabled INT,
       last_checkin TEXT, created_at TEXT )
codes( code PK, card_type TEXT, value INT, created_by INT,
       used_by INT, used_at TEXT, created_at TEXT )
logs ( id PK AUTOINCREMENT, tg_id, action, details, created_at )
```

对比 sakura 的评价：

- **`codes` 有 `card_type`**（`days` / `points`），终于有了类型字段（`api.py:81` 用 `pattern="^(days|points)$"` 在 Web 层做校验）；但仍**无批次、无有效期、无作废**。
- `max_devices` 作为用户级商品被商城/抽奖修改，与 Emby 侧策略并不自动同步（Emby 侧只有模板克隆时的一次性 Policy 写入），这是**语义空洞**：DB 里的 max_devices 只在巡检踢流时被读取（`scheduler.py:101`）。
- `logs` 表是 sakura 完全没有的**行为审计**，虽只有 20 个记录点，但方向正确。
- 无 `phone`/`region`/`invite_from` 字段 → **没有裂变溯源能力**。

### 2.5 核心流程

**A. 开号**（两种）

- 用户自助：`/bind 用户名 密码` → 用户名白名单过滤 `isalnum or _-`（`telegram_bot.py:767`）→ 已存在同名 Emby 账户时**拒绝认领**（`logger.warning("Refusing bind for existing unlinked Emby user")`，`telegram_bot.py:780`）→ 新建 + `create_user_record(days=30)`。
- 管理代开：`/create` 支持回复触发，口令 `"/create 30 密码"`；用户名冲突、TG 已绑定都会拦截并给出替代命令（`/addtime`）。

**B. 兑换（原子核销）**

```python
# core/database.py:462
cursor = await db.execute("UPDATE codes SET used_by=?, used_at=? WHERE code=? AND used_by IS NULL", (...))
if cursor.rowcount == 0: return {"success": False, "msg": "该兑换码刚已被其他请求使用！"}
```
两步式（先 SELECT 检查再条件 UPDATE）保证并发下不双兑，是本项目最扎实的一段。

**C. 积分经济**

| 途径 | 数值（代码硬编码） | 出处 |
| --- | --- | --- |
| 签到 | +`checkin_points`(默认10) 分，+`checkin_reward_days`(默认1) 天 | `telegram_bot.py:734`、`database.py:147` |
| 卡密（points 型） | +`value` 分 | `database.py:473` |
| 抽奖 | 45%~75% 区间 +35 分 | `database.py:249` |
| 掷骰人机 | 赢 +bet 净（先扣后返 2×） | `database.py:260` |
| PVP 掷骰 | 双方各扣 bet，胜者拿 2× | `database.py:352` |
| 打劫 | 成功抢 10%~25%（5~80 分） | `database.py:298` |
| 转账 | 用户间零和 | `database.py:398` |

| 消耗 | 数值 | 出处 |
| --- | --- | --- |
| 商城 7 天 | 50 分 | `database.py:177` |
| 商城 30 天 | 180 分 |
| 商城 90 天 | 500 分 |
| 商城 并发设备 +1 | 300 分 |
| 抽奖 | 20 分/次 |
| 打劫保证金 | 15 分（失败赔付给目标） |

**抽奖概率表**（`database.py:234-253`，写死在代码里，运营不可配）：

| 概率 | 奖励 | 类型 |
| --- | --- | --- |
| 5% | 并发设备上限 +1（永久） | grand |
| 15% | +7 天时长 | days |
| 25% | +3 天时长 | bonus |
| 30% | +35 积分（净赚 15） | points |
| 25% | 「赛博安慰奖：功德 +1」 | none |

经济期望核算（每次 20 分）：**积分净消耗 = 20 − 30%×35 = 9.5 分/次**；**时长期望产出 = 15%×7 + 25%×3 = 1.8 天/次**。以商城 7 天 = 50 分折算，1.8 天 ≈ 12.9 分，加上 9.5 分净消耗 = 玩家实际获得约 22.4 分价值 / 20 分成本，**微正 EV 但方差巨大**，主要靠 5% 的永久设备位拉动。掷骰与打劫是零和（人机骰子赢 15/36、输 15/36、平 6/36 退注；打劫失败 15 分赔付给受害者），因此**唯一真实通缩源是抽奖的 9.5 分/次**，加上签到的 10 分/天注入 —— 系统是净通胀的，只靠商城 300/500 分的大额消耗来回收，缺少自动调节机制。

**D. 到期与并发风控**（`core/scheduler.py`）

- `check_expirations()`：遍历全部用户，`expiry < now` 且未禁用 → `set_user_disabled(True)` + `is_disabled=1` + `log_action("AUTO_EXPIRE")` + 私聊提醒（含「签到或用兑换码自助续费」引导）。**失败时保留可重试状态**（`continue` 而不是标记已处理），这点比多数项目严谨。`warn_days_before_expiry` 的提前预警分支是 `pass` 空实现 —— 功能写了但没做。
- `check_concurrency()`：`GET /Sessions` → 过滤 `NowPlayingItem is not None` → 按 `UserId` 分组 → 超过 `max_devices` 则对**多出的最新会话**逐个 `stop_session`（先发 Message 提示再 `Playing/Stop`），并私聊发送拦截通知（账号/限制数/当前数/被阻断设备与客户端/媒体名/「请勿转借」）。这是三项目里**唯一真正做了实时并发踢流**的（sakura 靠 Emby 自身 limit，emby-keeper 不做风控）。

### 2.6 亮点

1. **群内回复式操作**：`/create`、`/info`、`/deluser`、`/rob`、`/dice`、`/transfer`、`/addpts` 全部支持 `reply_to_message` 取目标。运营者在群里 @ 一下就能干活，这是 TG 运营的真实肌肉记忆。lemon 把这个体验做到了命令设计的核心。
2. **资金操作全部原子化**：`deduct_user_points_atomic` 用 `UPDATE ... WHERE points >= amount` + `rowcount` 判定（`database.py:128`），测试用例 `test_atomic_double_spend_prevention` 专门验证。
3. **游戏化留存**：骰子（人机 + PVP 邀请按钮 120 秒过期）、打劫（含反杀罚金）、抽奖、转账、排行榜，五种玩法覆盖「即时刺激 / 社交 / 概率赌博 / 社交赠予 / 排名攀比」，是一套完整的群活跃引擎。
4. **风控前置**：命令级冷却 1.5~2s（`_check_cooldown`）、打劫日限 5 次（`rob_counts` 按 `日期:uid` 计数）、打劫目标 <30 分新手保护、押注上限 clamp 到 [1,1000]、天数 clamp 到 [1,3650]。
5. **Web 层安全**：启动时**拒绝默认 secret_key**（`api.py:40` RuntimeError）、`secrets.compare_digest` 恒定时间比对、IP 级失败计数 5 次锁 5 分钟（`api.py:18-34`）、Pydantic `Field(pattern=..., ge=..., le=...)` 参数区间、隐藏 `/docs` 与 `/redoc`。
6. **HTML 注入防护**：所有外部字符串过 `html.escape()`（昵称、Emby 影片名、设备名、自定义密码），避免 TG 消息实体解析被劫持。
7. **可测试性**：7 个 pytest 用例覆盖了资金与鉴权这些真正要命的地方。

### 2.7 缺陷

1. **SQLite 单机天花板**：`aiosqlite.connect()` 每方法新建连接（无连接池复用），高并发下 WAL 写锁竞争；无迁移工具，改 schema 只能手写 `IF NOT EXISTS`。
2. **卡密仍缺批次/有效期/作废**：`codes` 表无 `expires_at`、无 `batch`、无 `void`，`/gen` 一次 50 张封顶但无总量控制。相比之下 sakura 至少有类型统计与批量清理。
3. **无邀请裂变**：没有 invite 相关表字段，「拉新」完全靠管理员手动发码。
4. **积分通胀无治理**：抽奖是唯一通缩源（9.5 分/次），签到每天 +10 分；无总量统计、无回收池、无管理端配平工具，也没有「积分流水」表（只有 logs 里的字符串）。
5. **玩法数值硬编码**：抽奖概率、商城价格、打劫概率与罚金全在 `database.py` 里写死，运营改价要改代码重启。
6. **内存态易失**：`pending_duels`、`user_cooldowns`、`rob_counts`、`FAILED_ATTEMPTS` 全在进程内存，重启即清空，无法多副本。
7. **无强制的群成员校验**：不像 sakura 有 `user_in_group_filter` 强制入群，lemon 的 `group_id` 配置项基本没用上。
8. **Emby 用户名不做风险词/黑名单**，不做同名占用检测（只有 `get_user_by_name` 精确匹配），也不生成随机强密码以外的策略约束。
9. **到期预警是死代码**（`scheduler.py:82-84` 的 `pass`），运营无法提前提醒导致沉默流失。
10. **无多账号/多服务器**：`EmbyClient` 单实例、`server_url` 单值。

---

## 3. emby-keeper

### 3.1 定位与技术栈

代码 `参考项目/emby-keeper/`，是三个项目里工程化程度最高、也最「危险」的一个：**它不是一个给站长用的机器人，而是一个替真实用户去 TG 里打工的自动化客户端**。

- 版本信息：`pyproject.toml` name=embykeeper 7.6.1，`requires-python >=3.8`，GPLv3，三个 console script 入口：`embykeeper`（CLI）、`embykeeperweb`（Flask 网页终端）、`embykeeperapi`（FastAPI 后端）。
- TG 客户端：`kurigram==2.2.12`（pyrogram 的 fork，用于签到/监控）+ **telethon**（用于登录并导出 `StringSession` 给 pyrogram 用，`telegram/session.py:198 get_session_str_from_telethon`）。
- OCR：`ddddocrfix>=1.5.5` + onnxruntime，支持自定义 onnx 模型热下载。
- 前端：Vue3（`frontend/src/`，36 个 `.vue`、8 个视图） + Flask 内嵌 **PTY 网页终端**（`embykeeperweb/app.py` 里 `pty.openpty()` / `fcntl` / `termios` 直接开伪终端跑 CLI） + SocketIO 实时日志（`embykeeperapi/sse.py` 另有一套 SSE）。
- 无数据库：配置是 `config.toml`（TOML + pydantic schema `embykeeper/schema.py`），运行态存 `embykeeperapi/kv.py`（JSON 文件 KV）与 `embykeeper/cache.py`（aiocache）。
- 部署：`Dockerfile`（python:3.8-slim + venv）、`deploy/Dockerfile`、`app.json`/`render.yaml`/`heroku.yml`/`hf/`（HuggingFace Space 部署）、Windows 安装器（`windows/installer-gui`）、`.codesandbox`。7 种部署形态。

**规模**（`ls` 实测）：签到器站点模块 **141 个**（`embykeeper/telegram/checkiner/*.py` 共 159 个文件，扣除 `__init__.py`、`_base.py`、`_templ_a/b/c.py` 与 `test_*.py`），群监控器 **18 个**（目录共 39 个 py），水群素材站 **4 个**（`telegram/messager/`），自动注册器（`telegram/registrar/`）。

### 3.2 解决的用户故事

用户不是站长，而是**被服务的观众**：「我同时在 5 个 TG 公益服机器人里签到，它们要求每日签到、看广告、答题、抽奖才给续期，服务器还要求 7~12 天内有一次播放记录否则删号。我需要一个东西每天在随机时间自动替我做完这些。」

配套的站长侧故事：「我有 40 个 Emby 账号要养（自己多号或帮朋友），需要一台机器全托管、能推送结果、能网页改配置。」

**与 sakura-embyboss 的闭环节点**（本报告最有价值的发现）：`embykeeper/telegram/embyboss.py` 的 `EmbybossRegister` 是对 sakura-embyboss 的逆向适配器 —— 它用正则解析该 bot 的面板文本：

```python
current_status   = re.search(r"当前状态 \| ([^\n]+)", text).group(1).strip()
register_status  = re.search(r"注册状态 \| (True|False)", text).group(1) == "True"
available_slots  = int(re.search(r"可注册席位 \| (\d+)", text).group(1))
```
然后点「创建账户」按钮、发送 `"{username} {password}"`、等待编辑消息出现「创建用户成功」。而这些字符串正是 `sakura-embyboss/bot/modules/commands/start.py:78-84` 输出的面板。`telegram/monitor/_templ_a.py:44` 的注释更是直白：`# 尝试注册的机器人名称 (需为: https://github.com/berry8838/Sakura_embyboss)`。**买号者用 keeper 抢注，卖号者用 sakura 发号**，两边已经形成了事实上的攻防对偶。

### 3.3 「指令」表

emby-keeper 没有 TG 命令菜单，它的接口是 **CLI 参数 + Web API + 对目标 bot 的交互语义**三层。三层都列：

**A. CLI（`embykeeper/cli.py`，typer 选项）**

| 选项 | 含义 |
| --- | --- |
| `config_file`（位置参数） | 配置文件路径 |
| `--checkiner / --emby / --subsonic / --monitor / --messager / --registrar` | 模块级开关（可组合） |
| `--registrar-bot` | 指定抢注目标 bot |
| `--api --api-host --api-port` | 顺带拉起 FastAPI |
| `--instant` | 忽略调度立即执行一次 |
| `--once` | 只跑一轮即退出（cron 模式） |
| `--play <url>` | 手动播放指定播放页 URL 做保活 |
| `--dump / --top / --analyze / --follow / --public` | 配置导出、日志分析、跟随日志、公开模式 |
| `--verbosity --simple-log --disable-color --debug-cron --debug-notify --telegram-test-server --windows --version --example-config` | 运维与调试 |

**B. Web API（`embykeeperapi/`）**

| 路由 | 功能 |
| --- | --- |
| `POST /auth/login` `POST /auth/logout` `GET /auth/me` | 认证（Web 密码 → token） |
| `GET/POST/PUT/DELETE /kv/*` | 前端读写配置与状态 |
| `POST /pm/start` `POST /pm/stop` `POST /pm/restart` `GET /pm/status` | **进程管理**（网页上启停托管的后端进程） |
| `GET /sse/logs` `GET /sse/status` `GET /sse/{channel}` | SSE 实时日志与状态流 |
| `ANY /api/{path}`（`proxy.py`） | 反向代理层 |

**C. 对目标 bot 的交互语义**（`telegram/checkiner/_base.py` + 模板）

| 交互 | 实现 |
| --- | --- |
| 发 `/start` 或站点自定义命令 | `bot_checkin_cmd`（默认 `/start`，模板 A） |
| 点签到按钮 | 遍历 `message.reply_markup.inline_keyboard`，匹配 `bot_checkin_button = ["签到","簽到"]`，`message.click(k)`，随机延迟 0.5~1.5s（`_templ_a.py:88-99`） |
| 识别按钮回调返回 | `on_button_answer`：若返回 URL 型按钮则判失败；否则把 `answer.message` 当文本处理 |
| 回复验证码 | `on_captcha` → `message.reply(captcha)`（默认实现） |
| 通过按钮选答案（模板 C） | 用 `fuzz.ratio` 在按钮文本里模糊匹配 OCR 结果后 `click`（`_base.py:861-879`） |
| 抽奖/看广告类交互 | 各站点子类覆写 `on_text` / `on_photo` / `on_answer` |
| 成功判定关键词 | `bot_success_keywords` / `bot_fail_keywords` / `bot_checked_keywords`，默认词表见 `default_keywords`；成功后可 `bot_success_pat` 正则抽取「+X 分 -> Y 分」 |
| 兜底 | `on_unexpected_text → gpt_handle_message`：把消息文本 + 按钮列表交给 LLM，要求返回 `[CLICK] <按钮>` 之类的决策（`_base.py:621-700`） |

### 3.4 数据模型

**没有服务端数据库**，这是它的根本性架构选择，也是它无法做「运营后台」的原因。

| 载体 | 内容 | 文件 |
| --- | --- | --- |
| `config.toml` | 全部配置，pydantic 校验 | `embykeeper/config.py`、`schema.py`、`config.example.toml`（11,170 字节，注释详尽到每项一说明） |
| `TelegramAccount` | `phone / api_id / api_hash / session / checkiner / monitor / messager / registrar / site / checkiner_config` | `schema.py` |
| `EmbyAccount` | `url / username / password / time / play_id / allow_multiple / allow_stream / time_range / interval_days / enabled / proxy` | `schema.py` |
| KV 存储 | 前端写入的运行时键值（JSON 文件） | `embykeeperapi/kv.py` |
| 缓存 | 调度下次执行时间、运行状态 | `embykeeper/cache.py`（aiocache） |
| 会话 | Telethon `StringSession` 字符串（可导入导出/粘贴） | `telegram/session.py` |

调度设计的核心在 `embykeeper/schedule.py`：**`interval_days` + `time_range` 双约束的随机调度**，例如 `interval_days = "<7,12>"`（7~12 天）、`time_range = "<11:00AM,11:00PM>"`（当天 11:00~23:00 之间），并且**下次执行时间会被缓存到磁盘**（`cache.set(f"scheduler.{sid}", {"config_hash":..., "next_time":...})`），重启不重算、配置改了自动失效重算。这是为了让行为看起来「像人」，也是同类工具里少见的细节。

### 3.5 核心流程

**A. 机器人签到生命周期**（`telegram/checkiner/_base.py`，881 行）

```
start() → 建立 listener（filter 到目标 bot 的私聊）
  → walk_history(limit)       # 可选：回溯历史消息，先试着捞上一次的验证码图片（bot_use_history）
  → send_checkin()            # 依次发 bot_checkin_cmd，间隔 bot_send_interval
  → message_handler(message)  # 分类：TEXT / CAPTION / CAPTCHA
      ├─ CAPTCHA → on_photo() → OCR → on_captcha() → message.reply(captcha)
      ├─ TEXT    → on_text()  → 关键词判定：
      │             命中「账户错误」→ fail()
      │             命中「尝试次数过多」→ fail()
      │             命中「已签到」→ finish(NONEED)
      │             命中 fail 关键词 → retry()（最多 valid_retries，默认无限）
      │             命中 success 关键词 → before_success() → 抽取分数 → after_success() → finish(SUCCESS)
      │             都不命中 → on_unexpected_text() → GPT 兜底
      └─ 异常（OSError/超时）→ retry()
```

重试与超时都由基类统一：`retries`（`checkiner.retries=4`）、`timeout`（120s）、`self._waiting` 里按 regex 注册等待点、`wait_until(pattern)`。

**B. 验证码识别**（`embykeeper/ocr.py`，293 行）

这是本项目最核心的技术点：

1. **多进程隔离**：`OCRService` 用 `multiprocessing.Process` 跑 ddddocr，`Queue` 双向通信，`request_id` 关联 future。原因很实际 —— ddddocr/onnxruntime 会吃内存并且 CPU 密集，隔离进程 + 空闲回收才撑得住多账号。
2. **实例池 + 空闲回收**：`_pool` 按 `(ocr_name, char_range)` 复用实例；`_monitor()` 循环里 `subscribers == 0 且空闲 > 300 秒` 就 `force_stop()`（`ocr.py:151`）。用 `subscribe/unsubscribe` 计数（上下文管理器 `with ocr:`）。
3. **字符集约束**：`CharRange` 枚举（纯数字 / 小写 / 大写 / 数字+小写 / … 共 8 种），走 `model.set_ranges(char_range)` 并把 `classification(probability=True)` 的每字符最大值拼起来（`ocr.py:220-228`）。**用字符集先验把 OCR 准确率拉满**，这是关键技巧。
4. **自定义模型热下载**：`ocr_name` 指定时从 `get_datas()` 拉 `{name}.onnx` + `{name}.json` 到本地再加载（`ocr.py:206-218`）。
5. **GIF 验证码**：抽帧（最多 5 帧）+ 按帧数均分 alpha 做**透明叠加合成**再 OCR（`ocr.py:230-259`），对付那种逐帧抖动的验证码。
6. **后处理**：去掉所有标点与空格（`translate(str.maketrans("", "", string.punctuation))`），并用 `bot_captcha_len` 校验长度，长度不对直接重试（`_base.py:541-547`），发送前再 `sleep(2~4)` 拟人化。
7. **历史回溯**：`bot_use_history` 设定时先扫最近 N 条历史消息找上一次的验证码图片先试一次（`_base.py:790-804`）。
8. **模板 C 的「验证码作为选项」**：某些 bot 把验证码放在按钮上，就用 `fuzz.ratio` 模糊匹配按钮文本（`_base.py:871-879`）。

**C. 会话维持**（`telegram/session.py`，500+ 行）

- 登录：优先 **Telethon**（api_id/api_hash/phone 登录，支持手机号+验证码+2FA），成功后 `StringSession` 导出交给 Pyrogram 用（`session.py:198`）；也支持直接粘 session string。
- 维护：`ClientsSession.watchdog(timeout=120)` 周期检查连接，`clean/clean_all/shutdown` 统一回收；`_connect_handler` / `_disconnect_handler` 挂钩断线。
- 健康自检：`test_network()`、`test_time()`（检测服务器时间偏移，TG 对时间敏感）。
- 多账号：`config.telegram.account` 数组，每个账号独立 `_AccountState`（`checkin_main.py:37`），各自维护 task/site_tasks/batch_tasks 与 reconciler。

**D. Emby 保活（最重要的「伪造」逻辑）**（`embykeeper/emby/api.py`、`emby/main.py`）

保活目标：让服务器在「7~12 天内」看到一次真实播放记录。实现分四步：

1. **伪装请求头**（`api.py:212 build_headers`）：
```python
auth_headers = {"Client": env.client, "Device": env.device, "DeviceId": env.device_id,
                "Version": env.client_version}
headers["X-Emby-Authorization"] = f'MediaBrowser Token={token},Emby UserId={run_id},{auth_header}'
```
`DeviceId` 是持久化的伪设备标识 → 服务端把它当成一台固定设备的多次播放，而不是每次新设备（多开检测看的就是这个）。
2. **模拟人类浏览**（`api.py:load_main_page`）：依次 `GET /Users/{id}/Views` → `GET /Users/{id}`（读 LastLoginDate）→ `GET /DisplayPreferences/usersettings` → `get_resume_items(Video)` → `get_resume_items(Audio)` → 对每 25 个媒体库 `get_latest_items`，取不到就从文件夹里读（`get_folder_items`）。每步之间 `sleep(0.1~0.3)`。**这是把「打开 Emby App」这件事完整重放了一遍。**
3. **伪造播放**（`api.py:372 play`）：
   - 先构造巨大的 `DeviceProfile`（含 h264/hevc 的 CodecProfile、SubtitleProfiles 等）作为 `POST /Sessions/Playing` 的 payload；
   - 一边**真的去拉视频字节流**（`aiter_content(chunk_size=1024)`，读到随机 1% 的 chunk 就 skip 以模拟网络抖动），一边按 `random.uniform(5,10)` 秒模拟加载；
   - 主循环每 ≤10 秒 `POST /Sessions/Playing/Progress` 上报一次 tick（`int((time - t) * 10000000)`，Emby 的 100ns tick 单位），日志上报间隔随播放时长指数退避（5s → 10s → … → 300s 上限）；
   - 播放数量与时长由配置决定：`time` 可为定值或 `[min,max]` 随机，`allow_multiple` 决定是否允许跨多部片子凑时长（`watch()` 会 `random.shuffle` 候选集、校验 `MediaType=="Video"`、`RunTimeTicks` 不足时丢弃或按 `allow_stream` 伪造长度、失败原因分 invalid/no_length/wrong_type/short_length 四类统计）；
   - 结束时按 `random.uniform(0.95, 1.0)` 的观看进度补最后一帧 Progress 上报，制造「看到 95%~100% 才关掉」的自然收尾（`api.py:~729`）。
   - `Emby.playing_count` 全局计数用于并发控制。
4. **反封锁**：`curl_cffi` 的 `AsyncSession(impersonate="chrome", verify=False)` 伪造 TLS 指纹；`proxy` 支持 http/socks5；遇 502/503/504 退避重试 3 次；遇 401 自动重新 `login()`；遇 Cloudflare 挑战（`cf-wrapper` / `Just a moment`）调用 `use_cfsolver()` 走 `embykeeper/cloudflare.py` 解验证码并把 `cf_clearance` 作为 cookie 复用。

调度层 `emby/main.py` 支持**统一调度**（所有账号同一随机时刻批量保活）与**独立调度**（每个账号自己的 `time_range`/`interval_days`），并且配置热更新时会**保留正在执行的任务、只重排未来计划**（`_refresh_future_schedules`）。保活前还会 `random.uniform(180, 360)` 秒的随机等待（`main.py:350`），进一步打散时间分布。

**E. 群监控与抢注**（`telegram/monitor/`、`registrar/`）

- Monitor 基类（`monitor/_base.py`）配置项极丰富：`chat_keyword`（regex 列表）、`chat_except_keyword`、`chat_user`（限定触发者）、`chat_probability`（概率触发）、`chat_delay`（延迟）、`chat_follow_user`（等 N 个人先回才回）、`chat_reply`（固定串/函数/异步函数）、`trigger_interval`、`trigger_sim`、`trigger_max_time`。带 `Session` 类（`reply/follow/wait/cancel`）管理多轮对话。用途：抢邀请码、抢红包、自动答题（如 `terminus_exam.py`、`pornfans_answer.py`）、抢注（`chat_reply` 里嵌 `EmbybossRegister`）。
- Messager（`telegram/messager/`）：**自动水群**，YAML 素材 + 定时表（`_MessageSchedule`），`to_message_schedule()` 解析 `_MessageMaterialSchema`（文本/图片/概率/倍数），甚至有 `_smart.py` 的智能变体。配置文件里明确警告 `risk 较高, 默认禁用`。

### 3.6 亮点

1. **站点适配的工程范式**：模板 A/B/C 三个模板类 + 一行工厂函数（`_templ_a.py:120` `def use(**kw): return type("TemplatedClass", (TemplateACheckin,), kw)`），新增站点常常只要声明 `bot_checkin_cmd`、`bot_checkin_button`、`bot_success_keywords`。141 个站点因此可维护。
2. **OCR 子系统**：多进程 + 实例池 + 空闲回收 + 字符集先验 + 自定义 onnx 热加载 + GIF 合成 + 长度校验 + 历史回溯，一套完整的验证码对抗工程。这是 lemon/sakura 完全没有的能力（它们只发验证码给人看）。
3. **Emby 播放伪装的完整度**：设备指纹持久化、浏览路径重放、真实拉流、进度抖动、指数退避上报、95% 收尾、随机等待窗口 —— 从服务端视角几乎无法与真人区分。
4. **调度的人性化**：`<min,max>` 区间 + 时间窗 + 下次时间落盘缓存 + 配置哈希失效，兼顾了随机性与幂等。
5. **多形态交付**：CLI（适合 cron/systemd）、Flask + PTY 网页终端（适合小白，能在浏览器里跑 CLI）、FastAPI + Vue3 面板 + SSE 日志 + 进程管理 `/pm/*`、Docker、Heroku/Render/HF Space、Windows 安装器。同一份核心，7 种外壳。
6. **LLM 兜底**：`gpt_handle_message` 把「看不懂的消息 + 可用按钮」交给模型决策，是应对站点改版的通用降级路径 —— 站点改了文案，签到器不至于全线报废。
7. **通知生态**：`notifier` 支持 Telegram（可指定用第几个账号推送、立即/定时批量推送）与 **Apprise**（一条 URI 打通数十种通知渠道）。

### 3.7 缺陷

1. **无数据库是硬伤**：签到/保活结果只进日志与推送，`kv.py` 是 JSON 文件。因此无法回答「这个账号上个月成功了几次」「哪个站点失败率最高」「我养了 400 个号谁是活人」，也无法做配额、计费、多租户。**这是我们要超越的第一条。**
2. **全局可变状态泛滥**：`telegram/lock.py` 一整文件全是模块级 dict（`ocrs`、`misty_monitors`、`pornfans_*`、`authed_services`、`super_ad_shown`），配合 `config` 全局单例，基本告别可测试性与多实例。
3. **站点适配是维护债**：141 个签到器依赖硬编码关键词与正则（`bot_success_keywords`、`bot_checkin_button=["签到","簽到"]`），站点一改版就得跟进；又因为站点是私域小黑盒，无法用文档化接口替代。
4. **版权与合规形态脆弱**：GPLv3 + 涉及自动水群/自动抢注/绕过验证码，上游做过 `--public` 之类的能力开关；`messager` 自带「风险较高」警告但功能完整可用。
5. **依赖极重**：`requirements.txt` 40+ 项，含 numpy、Pillow、onnxruntime（经 ddddocrfix）、curl_cffi、pyrogram、telethon、flask、fastapi、socketio、apprise、pymongo、psutil、emoji、thefuzz、xpinyin。镜像远超 lemon 的 20MB 级目标。
6. **无多租户**：一个实例一套 `config.toml` 一个进程，无法给「10 个站长 × 各 50 个账号」提供服务；`--api` 也只是本机控制面。
7. **验证码依赖外部资源**：`get_datas()` 需要联网拉取 onnx 模型，离线/内网环境直接失效。
8. **测试覆盖单薄**：`tests/` 之外只有各模块旁挂的 `test_*.py`（如 `monitor/test_pornfans.py`），看起来是调试脚本而非回归测试。
9. **时间敏感**：`test_time()` 说明服务器时间偏移会导致签到大面积失败，但配置里没有强提醒。
10. **没有风控，只有对抗**：它站在被检测方，本身不提供任何面向站长的限制能力（并发、设备、异常行为）。

---

## 4. 横向归纳

### 4.1 Telegram 作为运营入口的刚需功能清单

按「用户 → 站长」两端的真实路径列出，并标注三项目实现程度与我们对新系统的建议优先级。

| # | 能力 | sakura-embyboss | lemon-emby-bot | emby-keeper | 建议优先级 |
| --- | --- | --- | --- | --- | --- |
| 1 | 私聊面板 `/start` + Inline 键盘 | ✅ 完整（多套面板） | ✅ 完整（8 按钮） | ❌（无 bot 端） | **P0** |
| 2 | 强制入群/频道校验 | ✅ `user_in_group_filter` | ❌ | ❌ | **P0** |
| 3 | 自助开号（用户名+密码/安全码） | ✅ 队列+回滚 | ✅ `/bind` 私聊限定 | ✅ 反向抢注 | **P0** |
| 4 | 管理代开号（群内回复触发） | ❌（面板点选） | ✅ `/create` 回复 | ❌ | **P0** |
| 5 | 卡密/兑换码（生成+核销） | ✅ 三类型（注册/续期/白名单）+ 库存统计 + 批量清理 | ✅ 两类型（天数/积分）+ 原子核销 + Web 生成 | ❌ | **P0**，但需补**批次/有效期/作废**（三项目都缺） |
| 6 | 到期续期（码/积分/管理三种） | ✅ | ✅ 码+签到+管理 | ❌ | **P0** |
| 7 | 到期封禁与自动清退 | ✅ 两段式（c→d） | ✅ 禁用保留数据 | ❌ | **P0** |
| 8 | 到期前预警提醒 | ❌（只有到期那天） | ⚠️ 死代码 | ❌ | **P1** |
| 9 | 每日签到（连签/阶梯奖励） | ⚠️ 单次随机 | ⚠️ 单次固定 | ✅ 替用户签 | **P1**（连签阶梯是可做的差异化） |
| 10 | 积分/虚拟币体系 | ✅ 双币（us+iv） | ✅ 单币 points | ❌ | **P1** |
| 11 | 积分商城（时长/设备/白名单/邀请码） | ✅ 4 类 | ✅ 4 类 | ❌ | **P1** |
| 12 | 邀请裂变与溯源 | ⚠️ 只发邀请码，无溯源 | ❌ | ❌ | **P1**（缺口，见 4.2-2） |
| 13 | 抽奖/概率玩法（含概率可配） | ⚠️ 仅红包 | ✅ 抽奖但硬编码 | ❌ | **P1**（概率必须可配 + 有公示） |
| 14 | 群内游戏（骰子/PVP/打劫/红包） | ✅ 红包+榜单 | ✅ 骰子/PVP/打劫/转账/榜 | ❌ | **P2**（留存加分项） |
| 15 | 排行榜与传播海报 | ✅ PIL 海报 + 翻页 | ⚠️ 纯文本 TOP10 | ❌ | **P2** |
| 16 | 并发播放限制与踢流 | ⚠️ 靠 Emby Policy=2 | ✅ 巡检踢多余会话+私聊告警 | ❌（反过来是绕过方） | **P0** |
| 17 | 客户端/UA 风控与封禁 | ✅ webhook + 黑白名单 + 踢会话 | ❌ | ❌ | **P1** |
| 18 | 设备/IP 审计与关联分析 | ✅ 三件套（IP/设备/客户端） | ❌ | ❌ | **P1** |
| 19 | 多开/共享号检测（同 IP 多账号等） | ⚠️ 事后审计 | ❌ | ❌ | **P1**（缺口，见 4.2-3） |
| 20 | 媒体库分级授权（分区售卖） | ✅ `partition_codes/grants` | ❌ | ❌ | **P2**（商业化亮点） |
| 21 | 自动化定时任务 + 面板开关 | ✅ APScheduler 8 个任务 | ✅ while 巡检 2 个任务 | ✅ 随机窗调度 | **P0** |
| 22 | 数据库备份/恢复 | ✅ 自动备份+`/restore_from_db` | ❌ | ❌ | **P1** |
| 23 | 管理端 Web 面板 | ✅ FastAPI（默认关） | ✅ 单页 Dark 控制台 | ✅ Vue3 + SSE + 进程管理 | **P1** |
| 24 | 审计日志（行为流水） | ⚠️ loguru 文本 | ✅ `logs` 表（20 个埋点） | ❌ | **P0**（要结构化 + 可查询） |
| 25 | 多服务器/多线路支持 | ⚠️ emby_line 文本 | ❌ | ⚠️ `[[emby.account]]` 多账号 | **P1** |
| 26 | 多租户（一站多站主） | ❌ | ❌ | ❌ | **P1**（三项目零覆盖，最大空白） |
| 27 | 客户端连接引导（地址/密码/推荐 App） | ✅ `/server` | ✅ 开号即私聊推送 | ❌ | **P0**（转化率关键） |
| 28 | 通知渠道（站内/邮件/Apprise） | ⚠️ 仅 TG | ⚠️ 仅 TG | ✅ TG + Apprise | **P2** |

### 4.2 这类项目的通病（我们要超越的地方）

**1. 卡密系统集体停留在「一张表 + 一个 used 字段」。**
- sakura：`Rcode(code, tg, us, used, usedtime)`，**类型靠字符串子串判断**（`"Renew" in code`），无批次、无有效期、无作废、无生成上限，删码只能按「类型+天数」批量删未使用的（`sql_delete_unused_by_days`）。
- lemon：`codes(code, card_type, value, created_by, used_by, used_at, created_at)`，有 `card_type` 但**无 `expires_at`、无批次、无作废**。
- 我们要做的：`code_batches`（批次：名称/生成人/类型/面值/数量/有效期/状态/备注）→ `codes`（归属批次、状态机 unused/redeemed/void/expired、redeem_at/redeemed_by_ip）、核销走条件更新 + 幂等键、支持部分作废与对账导出。这是**零竞争差异点**，也是所有站长的真实痛点（发出去的码被盗刷无法止损）。

**2. 邀请/裂变没有数据模型。**
sakura 有 `_open.invite / invite_cost / invite_lv` 配置和 `cr_link_two()` 生成 `{for_tg}-{random}` 形式的码，但 **`Rcode` 表里没有 `inviter` 字段**，谁拉了谁无法统计；lemon 完全没有；emby-keeper 有抢注但没有归属概念。
超越点：`invites(inviter_tg, invitee_tg, code, bound_at, level)` + 邀请奖励（被邀请人首次开号/续费后发放）+ 防刷（同 IP / 同设备 / 同手机号段限制）+ 邀请榜。

**3. 风控是「看会话数」而不是「看人」。**
- sakura 强项在事后审计（`auditip/auditdevice/auditclient` 反查）与 UA 过滤，但**并发限制外包给 Emby 的 `SimultaneousStreamLimit`**，无法做「同账号 3 个城市 IP」「一台设备 5 个账号」这类关联判定。
- lemon 会巡检踢流，但只比 `len(sessions) > max_devices`，`max_devices` 还是用户能自己用积分买到的数值，且 DB 与 Emby Policy 不同步。
- emby-keeper 恰好是这条防线的对手：**它持久化 `DeviceId`、伪造 `X-Emby-Authorization`、模拟真实拉流**。防守方必须升级到行为层。
- 超越点：设备指纹（DeviceId + UA + Client）、IP 地理与 ASN 聚类、同 IP 多账号图分析、播放行为特征（瞬时起播、tick 严格线性增长、无转码/无 seek、时长集中于 5~10 分钟）、风险分与自动处置分级（警告 → 踢流 → 限流 → 冻结）。

**4. 积分经济没有闭环治理。**
- sakura：`iv` 的来源有签到（随机 1~10）、观影榜（前 10 名 + 时长分钟数）、红包（用户间转移）；消耗有 300/月自动续期、9999 白名单、1000/30天邀请码、5 分榜单手续费。**观影榜全站发币、无上限**，属于典型的通胀引擎，且没有任何总量统计与调节工具（`/coinsclear` 是核弹，不是调节）。
- lemon：签到日注入 10 分，抽奖日回收 9.5 分/次，其余玩法零和。**净通胀**，且价格与概率硬编码。
- 超越点：积分账本（`point_ledger`：来源类型、数量、余额快照、关联业务 ID）+ 每日产出/回收报表 + 可配的经济参数（价格、概率、日上限、衰减）+ 通胀告警 + 管理端一键调参（改配置不重启）。

**5. 关键资金路径缺测试。**
sakura **0 个测试**（有开号回滚、码核销行锁这些高危逻辑）；lemon 有 7 个 pytest（含防双花与原子核销，是三者中唯一达标的）；emby-keeper 的 `test_*.py` 更接近调试脚本。
超越点：开号/续期/核销/退款的并发与幂等测试作为 CI 门槛。

**6. 单实例、无水平扩展。**
sakura 的 `register_queue` 单例、`_open.tem` 内存计数、`cacheout` 缓存；lemon 的 `pending_duels`/`user_cooldowns`/`rob_counts`；emby-keeper 的 `lock.py` 全局 dict + 全局 config。三者在多副本部署下都会出现重复扣费、重复发码、重复续期。
超越点：状态全部落库 + 乐观锁/条件更新 + 分布式任务领取（`SELECT ... FOR UPDATE SKIP LOCKED` 或任务表租约），缓存只做加速不做真相。

**7. 多租户完全空白。**
三个项目都是「一个实例服务一个站」。而真实生态里，一个小站长手里可能同时管 Emby、Jellyfin、Nastool、几个线路。没有任何一个项目提供「多服务器 + 多 TG 群 + 多管理员组」的隔离模型。
超越点：`tenants`（站点）+ `servers`（Emby 实例）+ `channel_bindings`（TG 群/频道归属哪个站点）+ 数据行级隔离 + 每站点独立的价格表与经济参数。

**8. 可观测性差。**
`print(e)`（sakura `sql_code.py:151`、`red_envelope.py:73`）、裸 `except:` 吞异常（`sql_emby.py:38`）、无 metrics、无结构化事件流。唯一像样的是 lemon 的 `logs` 表与 emby-keeper 的 loguru+SSE。
超越点：结构化事件（`event_type` + `subject` + `payload` JSON）+ 关键路径 trace id（一次开号一条链路）+ 管理端可查可导出。

**9. 「保活」与「风控」是同一枚硬币，没人同时做两端。**
emby-keeper 精于伪造播放，sakura/lemon 精于运营管理，两者从不相交，唯一的接触点是 emby-keeper 里那个写死的 sakura 面板正则（`telegram/embyboss.py`）与 `_templ_a.py:44` 的仓库注释。
超越点：做一个能同时持有两端的系统 —— 运营端知道每个用户的正常行为基线，因此**能识破 keeper 类工具的异常特征**；同时运营端也能用同样的 API 能力做「官方保活」（例如到期前帮用户自动产生一次真实播放记录来提高留存）。这是三份样本都没有的位置，也是差异化最大的位置。

**10. 账号凭据安全普遍裸奔。**
sakura 明文存 `pwd`/`pwd2` 并在 TG 明文发送、webhook 直接拿 bot_token 当 API token；lemon 明文存密码、日志打印「Refusing bind for existing unlinked Emby user」这类细节但凭据仍明文；emby-keeper 把账号密码放在 `config.toml` 明文、session string 也是明文。
超越点：凭据加密落库（KMS/主密钥派生）、只在必要时刻解密、一次性展示后即焚、Telegram 侧敏感消息 `timer` 自毁（sakura 已有 `sendMessage(..., timer=60)` 的雏形）、API token 与 bot token 分离且可轮换。

---

**结论一句话**：三个样本把「Telegram 作为 Emby 运营入口」的体验层做到了可用甚至精美（sakura 的面板与码务、lemon 的群内快操作与游戏化、emby-keeper 的自动化与验证码对抗），但在**资金安全（卡密批次/账本/测试）、风控深度（行为画像 vs 会话计数）、多租户与经济治理**这三块集体缺位 —— 这三块正好是新系统的立足点。

# G5 MoviePilot 核心引擎分析

> 分析对象：`参考项目/moviepilot`（jxxghp/MoviePilot）
> 规模：2182 个 Python 文件，Python >=3.14，100+ 直接依赖
> 定位：**整个 Emby 私服生态的自动化中枢**，也是所有"用户中心"类项目的下游执行器

---

## 1. 项目定位与技术栈

MoviePilot 基于 NAStool 重新设计，聚焦"自动化核心"：识别 → 搜索 → 筛选 → 下载 → 整理 → 入库 → 通知。

| 层 | 技术 |
|---|---|
| 语言 | Python 3.14（要求 `requires-python = ">=3.14"`） |
| Web 框架 | FastAPI + Uvicorn |
| ORM | SQLAlchemy 2.x + Alembic 迁移 |
| 数据库 | SQLite（默认）/ PostgreSQL |
| 调度 | APScheduler |
| 前端 | 独立 `frontend-dist`（Vue + Vuetify） |
| 部署 | Docker，单进程 |

依赖清单里能看到它的野心：`anthropic`、`google-genai`、`discord.py`、`boto3`、`cloakbrowser`（反爬浏览器）——已经不只是下载工具，而是带 AI Agent 的媒体自动化平台。

## 2. 目录结构 = 教科书级六边形架构

```
app/
├─ domain/          # 领域模型：MediaInfo / MetaInfo / Torrent / Site / Plugin / Episode
├─ application/     # 应用服务：169 py，编排用例（download / transfer / mediaserver / messaging / plugin）
├─ modules/         # 能力模块：166 py，对接外部系统
├─ chain/           # 业务处理链：109 py，跨模块编排
├─ adapters/        # 适配器：web / system / external / cache / network（六边形端口）
├─ runtime/         # 运行时：插件系统、事件、缓存、日志
├─ sdk/             # 开发者 SDK：插件契约、chain、database、events、config
├─ api/             # HTTP 端点：45+ 个端点文件
├─ scheduler/       # 调度器：catalog / registry / execution / reconcile
├─ db/models/       # 32 个数据模型文件
└─ workflow/        # 工作流引擎
```

**分层依赖方向清晰**：`api → application → domain`，`chain` 通过 `run_module()` 调用 `modules`，`adapters` 把外部世界翻译成端口。

## 3. 核心设计一：双层扩展模型（Module + Plugin）

这是 MoviePilot 最值得抄、也最值得超越的地方。它把"扩展"拆成**两个正交的层**：

### 3.1 Module = 能力提供者（宿主内置）

`app/modules/` 下 41 个模块，每个对接一类外部系统：

| 类别 | 模块 |
|---|---|
| 媒体服务器 | `emby`、`jellyfin`、`plex`、`trimemedia`、`ugreen` |
| 下载器 | `qbittorrent`、`transmission`、`rtorrent` |
| 元数据源 | `themoviedb`、`thetvdb`、`douban`、`bangumi`、`anilist`、`imdb`、`fanart`、`musicbrainz`、`acoustid` |
| 通知渠道 | `telegram`、`wechat`、`slack`、`discord`、`feishu`、`dingtalk`、`qqbot`、`webpush`、`vocechat`、`synologychat` |
| 基础设施 | `postgresql`、`redis`、`indexer`（多站点统合）、`filter`、`subtitle`、`filemanager` |

**模块基类契约**（`app/modules/__init__.py:17`）：

```python
class _ModuleBase(metaclass=ABCMeta):
    def init_module(self)                                  # 实例化
    def init_setting() -> Optional[Tuple[str, Union[str, bool]]]  # 配置键 + 默认开关
    def on_config_changed(self)                            # 热重载
    def get_name() -> str                                  # 类方法：模块名
    def get_type() -> ModuleType                           # 大类
    def get_subtype() -> Union[...]                        # 子类
    def get_priority() -> int                              # 同类型多模块的优先级
    def test() -> Optional[Tuple[bool, str]]               # 连通性自检
    def stop() -> Optional[bool]
```

**关键机制：同类型多模块 + 优先级**。比如有 3 个下载器模块，自动下载时可以按 `get_priority()` 决定用哪个；Emby 和 Jellyfin 同时存在也能共存。

**`ServiceBase` 支持多实例**（`get_instances() -> Dict[str, TService]`）——一个模块可以配置多台服务器（3 台 Emby、2 个 qB），这是很多轻量项目做不到的。

业务样板基类在 `app/modules/_base/`：`downloader.py`、`media.py`、`mediaserver.py`、`notification.py`，把重复样板沉下去，差异通过「类属性 + 钩子方法」保留。设计得很干净。

### 3.2 Plugin = 功能扩展包（第三方生态）

`app/sdk/plugin/base.py` 的 `_PluginBase` 就是插件契约（**这是精华，全文抄下来**）：

```python
class _PluginBase(metaclass=ABCMeta):
    # ── 生命周期 ──
    def init_plugin(config)                        # 初始化
    def stop_service()                             # 停止
    # ── 元信息 ──
    def get_name() -> Optional[str]
    def get_state() -> bool                        # 启用开关
    # ── 扩展点（声明式注册）──
    def get_command()                              # 注册 TG 命令
    def get_api() -> List[Dict]                    # 注册 HTTP 路由 ★
    def get_service() -> List[Dict]                # 注册定时任务（cron/interval/date）★
    def get_module() -> Optional[Dict]             # 动态注册能力模块 ★
    def get_media_source()                         # 注册媒体源
    def get_auth_providers()                       # 注册认证提供者
    def get_actions()                              # 注册可执行动作
    def get_agent_tools() -> List[Type[Any]]       # 注册 AI Agent 工具 ★★
    # ── UI（动态 schema）──
    def get_render_mode() -> Tuple[str, Optional[str]]   # vuetify / vue
    def get_form() -> Tuple[List[Dict], Dict]      # 配置页（Vuetify 组件树）★★
    def get_page() -> Optional[List[Dict]]         # 详情页（Vuetify 组件树）★★
    def get_dashboard(key) -> Tuple[Dict, Dict, List]     # 仪表盘卡片 ★★
    # ── 数据（自带 schema）──
    def get_database_models() -> Optional[List[Type]]     # 插件自带表 ★★★
    def get_database_migrations() -> Optional[Path]       # 插件自带 Alembic 迁移 ★★★
    # ── 配置 ──
    def update_config(config) / get_config() / get_data_path()
```

**三个最惊艳的设计**：

1. **动态 UI schema**：插件不写 HTML，返回一棵 Vuetify 组件树（`get_form`/`get_page`/`get_dashboard`），前端渲染。插件开发者不懂前端也能做出好看界面；也支持 `render_mode="vue"` 走编译产物。
2. **插件自带数据库 + 迁移**：`get_database_models()` 声明 SQLAlchemy 模型，宿主启动时建表；更高级的用 `get_database_migrations()` 交给 Alembic，并在 PG 下用独立 schema 隔离。**第三方插件可以有自己的完整数据模型，不污染宿主表**。
3. **`get_agent_tools()`**：插件可以把自己的能力注册成 LLM 工具，直接接入 AI Agent 链路——这是 2026 年最新一代设计。

### 3.3 插件系统的代价（我们的机会）

`app/runtime/extensions/plugin/` 的规模：

| 文件 | 行数 |
|---|---|
| `manager.py` | 1477 |
| `storage.py` | 692 |
| `runtime.py` | 490 |
| `loader.py` | 486 |
| `lifecycle.py` | 463 |
| `projection.py` | 379 |
| `clone.py` | 374 |
| `catalog.py` | 334 |
| `classification.py` | 325 |
| `monitor.py` | 288 |
| `sync.py` | 258 |
| **合计** | **7159 行** |

**7159 行只为支撑插件机制**。再加上"插件依赖包安装"（`dependency.py`）——插件要装 pip 包、要处理代际兼容（`compatible_flags`）、要处理 frozen 运行模式。理解成本极高。

## 4. 核心设计二：Chain 业务链编排

`app/chain/` 把每条业务线拆成多个细粒度文件，而不是一个巨型类：

| 链 | 拆解文件 |
|---|---|
| `search/` | cache / execution / facade / media / music / pagination / plan / provider / recommend / result / site / subtitle / title |
| `subscribe/` | completion / create / identity / match / metadata / notify / policy / query / reconcile / refresh / search / searchtask |
| `download/` | batch / existence / failure / history / processing / selection / submission / subtitle / tasks |
| `transfer/` | checkpoint / execution / filter / format / history / plan / queue / records / request / retry / scrape / settlement / workflow |

每个目录都遵循 **`contract.py`（接口）+ `facade.py`（门面）+ 具体实现** 的模式。`ChainBase` 通过 mixin 组合能力（`RecognitionMixin`、`MessageProcessingMixin`、`NotificationMixin`），并用 `Protocol` 声明宿主契约（`_contracts.py`）。

统一调用入口：

```python
def run_module(self, method: str, **kwargs) -> Any          # 同步调用模块能力
def run_module_strict(self, method, **kwargs) -> Any        # 传播 provider 失败
async def async_run_module(self, method, **kwargs) -> Any   # 异步
```

**这是"按名字调用模块方法"的弱类型分发**——灵活但失去了静态检查。`_contracts.py` 里那些 `Protocol` 就是给这种动态调用补类型约束的补丁。

## 5. 数据模型

32 个模型文件，覆盖完整业务：

| 分组 | 模型 |
|---|---|
| 身份 | `user`、`passkey`、`userconfig` |
| 媒体 | `mediaserver`、`subscribe`、`subscribehistory`、`subscriptionsearch` |
| 下载 | `downloadhistory`、`downloadfailure`、`site`、`siteicon`、`sitestatistic`、`siteuserdata` |
| 整理 | `transferhistory`、`transferpending`、`transferexecutionstep`、`transfersettlementreceipt` |
| 插件 | `pluginidentity`、`plugininstallation`、`plugininstance`、`plugindata` |
| 消息 | `message`、`outbox` |
| Agent | `agentchat`、`agentinvocation`、`agenttask`、`agenttaskrun` |
| 其他 | `systemconfig`、`workflow` |

几个模型的设计亮点：

**`site.py`** —— 多站点适配的关键，字段里同时有 `cookie`、`ua`、`apikey`、`token`、`proxy`、`render`，还有**熔断限流三兄弟** `limit_interval / limit_count / limit_seconds`（单位时间最多请求 N 次），以及 `downloader`（站点专用下载器）。这是被 PT 站反爬毒打出来的设计。

**`subscribe.py`** —— 订阅不只是"关键词"，还带 `filter/include/exclude`（过滤规则）、`quality/resolution/effect/audio_quality/audio_format`（画质偏好）、`min_bitrate/min_bit_depth`、`downloaded_tracks`（音乐按曲目级 JSON 记录）。**订阅 = 关键词 + 过滤规则 + 质量偏好 + 状态机**。

## 6. 与 Emby 的集成

`app/modules/emby/` 拆成 `__init__.py`（244 行，模块壳）+ `emby.py`（1405 行，HTTP 客户端）。

模块暴露的能力接口（`EmbyModule`）：

```python
webhook_parser()              # 解析 Emby webhook 事件
media_statistic()             # 媒体库统计
mediaserver_librarys()        # 媒体库列表
mediaserver_items()           # 库内条目分页
mediaserver_iteminfo()        # 条目详情
mediaserver_tv_episodes()     # 剧集信息（缺集检测的数据源）
mediaserver_playing()         # 正在播放 ★
mediaserver_play_url()        # 播放直链
mediaserver_season_episode_ids()
mediaserver_latest()          # 最近入库
mediaserver_latest_images()
```

再加上 `_MediaServerModuleBase` 提供的 `user_authenticate()`（用户辅助认证）和媒体存在性检查——**Emby 既是媒体服务器，也是认证后端和播放数据源**。

## 7. 亮点总结（值得直接借鉴）

1. **Module/Plugin 双层扩展**：能力扩展（对接新系统）和功能扩展（加新玩法）解耦，互不干扰
2. **同类型多模块 + 优先级 + 多实例**：一个能力可以有多个 Provider，运行时择优
3. **动态 UI schema**：插件零前端成本出管理界面
4. **插件自带数据模型与迁移**：生态能长出自己的表，不绑架宿主 schema
5. **Chain 细粒度拆分**：业务链按"一次只做一件事"拆文件，单文件职责极窄
6. **事件驱动的模块协作**：`eventmanager` + `ChainEventType`，模块通过事件而不是直接调用来联动
7. **六边形端口清晰**：`adapters/` 把外部世界隔离在外，`domain/` 纯业务
8. **`get_agent_tools()`**：插件能力自动暴露给 AI Agent

## 8. 缺陷与超越点（EmbyBox 的机会）

| 痛点 | 具体表现 | 我们的对策 |
|---|---|---|
| **插件系统过重** | 7159 行只为插件机制；`manager.py` 单文件 1477 行 | 契约优先、把 90% 场景收敛到声明式清单，内核控制在千行内 |
| **依赖地狱** | Python 3.14 + 100+ 直接依赖；插件要装 pip 包、要处理代际兼容 | 单二进制 + 进程外插件（可选），插件系统与依赖系统解耦 |
| **模块分发靠字符串** | `run_module("method_name", **kwargs)` 弱类型，靠 `Protocol` 补丁 | 强类型接口 + 显式能力注册表，编译期/启动期可校验 |
| **启动慢** | 全量模块 + 插件 + 迁移 + 调度一起上 | 懒加载 + 能力按需装配 |
| **只面向"服主自动化"** | 没有面向**最终用户**的门户/积分/工单/风控 | EmbyBox 的核心差异化：用户侧运营能力是一等公民 |
| **无内建多租户** | 单实例单媒体服务器为主 | 原生多服务器/多租户（Emby、Jellyfin、Plex 同权） |
| **插件生态无沙箱** | 插件即宿主进程内代码，权限等同于宿主 | 能力授权清单 + 可审计的调用边界 |

---

## 9. 一句话结论

MoviePilot 解决的是**「资源怎么自动进库」**——它把自动化做到了极致，但对**「人」**几乎不管：没有用户注册、没有积分、没有工单、没有风控、没有面向用户的求片交互。

而 `ember`、`emby-pulse`、`usersvr` 这些项目反过来：管人很像样，但下载链要么直接调 MoviePilot，要么干脆没有。

**EmbyBox 的空位就在这里：以「人」为核心，把「资源」当作一种可编排的能力接进来，用一套轻量插件系统同时覆盖两侧。**

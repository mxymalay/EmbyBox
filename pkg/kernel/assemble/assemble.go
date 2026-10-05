// Package assemble 是内核唯一的"主流程"。
//
// 整个内核的业务逻辑就是这五步：
//
//	发现 → 排序 → 注册 → 鉴权 → 启动
//
// 业务代码一行都不在这里。如果你在这个包里看到了 "Emby"、
// "用户"、"积分" 这类词，说明抽象漏了。
package assemble

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/embyone/embyone/pkg/kernel/config"
	"github.com/embyone/embyone/pkg/kernel/event"
	"github.com/embyone/embyone/pkg/kernel/logx"
	"github.com/embyone/embyone/pkg/kernel/plugin"
	"github.com/embyone/embyone/pkg/kernel/registry"
	"github.com/embyone/embyone/pkg/kernel/store"
)

// App 是装配完成的运行时。
type App struct {
	Cfg *config.Config
	Log logx.Logger

	DB       *store.DB
	Registry *registry.Registry
	Bus      *event.Bus

	// Plugins 已装配的插件，顺序为拓扑序。
	Plugins []plugin.Plugin
	// DeclarativePlugins 只是被加载进来（尚未有执行器）的声明式插件。
	DeclarativePlugins []*plugin.Manifest
	// Envs 按插件 ID 索引的装配环境，供后台展示权限与迁移。
	Envs map[string]*plugin.Env
	// Warnings 装配期的非致命问题，必须暴露给管理员。
	Warnings []string

	startedAt time.Time
	mu        sync.RWMutex
	started   bool
}

// Boot 执行发现 → 排序 → 注册 → 鉴权的全过程。
//
// 返回后系统已"逻辑就绪"，但还没对外提供服务（那要等 Start）。
// 这样拆开是为了让测试可以在 Start 之前检查注册表内容。
func Boot(ctx context.Context, cfg *config.Config, log logx.Logger) (*App, error) {
	if cfg == nil {
		return nil, errors.New("assemble: 配置为空")
	}
	if log == nil {
		log = logx.Default()
	}

	app := &App{
		Cfg:       cfg,
		Log:       log,
		Envs:      make(map[string]*plugin.Env),
		startedAt: time.Now(),
	}

	// ── 1. 存储 ──────────────────────────────────
	app.Log.Info("打开数据库", "driver", cfg.DBDriver, "dsn", cfg.DBDSN)
	db, err := store.Open(ctx, store.Options{
		Driver: cfg.DBDriver,
		DSN:    cfg.DBDSN,
		Log:    log.With("component", "store"),
	})
	if err != nil {
		return nil, err
	}
	app.DB = db

	// ── 2. 内核自身的迁移（版本 1-999）─────────────
	if err := db.Migrate(ctx, KernelMigrations()); err != nil {
		db.Close()
		return nil, err
	}

	// ── 3. 内核服务 ──────────────────────────────
	app.Registry = registry.New()
	app.Bus = event.NewBus(log.With("component", "event"))

	// ── 4. 发现插件 ──────────────────────────────
	dis, err := plugin.Discover(cfg.PluginDirs, cfg.DisabledPlugs, log.With("component", "plugin"))
	if err != nil {
		db.Close()
		return nil, err
	}
	app.Warnings = append(app.Warnings, dis.Warnings...)
	app.DeclarativePlugins = dis.Declarative

	// ── 5. 拓扑排序 ──────────────────────────────
	manifests := make([]*plugin.Manifest, 0, len(dis.Builtins)+len(dis.Declarative))
	for _, p := range dis.Builtins {
		manifests = append(manifests, p.Manifest())
	}
	manifests = append(manifests, dis.Declarative...)

	order, err := plugin.ResolveOrder(manifests)
	if err != nil {
		db.Close()
		return nil, err
	}

	byID := make(map[string]plugin.Plugin, len(dis.Builtins))
	for _, p := range dis.Builtins {
		byID[p.Manifest().Metadata.ID] = p
	}

	// ── 6. 校验依赖版本 ──────────────────────────
	for _, m := range manifests {
		for _, req := range m.Spec.Requires {
			dep, ok := findManifest(manifests, req.ID)
			if !ok {
				db.Close()
				return nil, fmt.Errorf("plugin: %s 依赖的 %s 不存在", m.Metadata.ID, req.ID)
			}
			if !plugin.CheckVersionRequirement(dep.Metadata.Version, req.Version) {
				db.Close()
				return nil, fmt.Errorf("plugin: %s 要求 %s 版本 %s，实际为 %s",
					m.Metadata.ID, req.ID, req.Version, dep.Metadata.Version)
			}
		}
	}

	// ── 7. 按序注册 ──────────────────────────────
	var pluginMigrations []store.Migration

	for _, id := range order {
		p, isBuiltin := byID[id]
		if !isBuiltin {
			// 声明式插件（L0/L1）：M0/M1 阶段只登记，等 M6 接入脚本执行器。
			// 这里不做假动作——没有执行器就是没有，如实记录。
			app.Log.Info("声明式插件已登记（等待脚本执行器）", "plugin", id)
			continue
		}

		env, err := buildEnv(cfg, log, app, p.Manifest())
		if err != nil {
			db.Close()
			return nil, err
		}

		app.Log.Info("装配插件", "plugin", id, "summary", p.Manifest().Summary())
		if err := p.Register(ctx, env); err != nil {
			db.Close()
			return nil, fmt.Errorf("plugin %s: Register 失败: %w", id, err)
		}

		app.Plugins = append(app.Plugins, p)
		app.Envs[id] = env
		pluginMigrations = append(pluginMigrations, env.Migrations()...)
	}

	// ── 8. 执行插件迁移 ──────────────────────────
	// 统一在所有 Register 之后执行，好处是插件之间的表外键不会因为
	// 执行顺序而互相缺失。
	if len(pluginMigrations) > 0 {
		if err := db.Migrate(ctx, pluginMigrations); err != nil {
			db.Close()
			return nil, err
		}
	}

	// ── 9. 单实例约束自检 ────────────────────────
	// 需要独占执行的插件在这里就失败，而不是让它在运行期
	// 变成偶发的重复扣费/重复发码。
	for _, p := range app.Plugins {
		m := p.Manifest()
		if m.Spec.Execution.Mode != "single-instance" {
			continue
		}
		held, err := acquireInstanceLock(ctx, db, m.Metadata.ID)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("plugin %s: 获取单实例锁失败: %w", m.Metadata.ID, err)
		}
		if !held {
			db.Close()
			return nil, fmt.Errorf("plugin %s 声明了 single-instance，但锁已被占用："+
				"说明另一个实例正在运行。多实例部署时请把该插件的 execution.mode 改为 multi-instance",
				m.Metadata.ID)
		}
	}

	log.Info("装配完成",
		"插件数", len(app.Plugins),
		"声明式插件数", len(app.DeclarativePlugins),
		"能力数", len(app.Registry.All()),
		"事件订阅数", app.Bus.SubscriptionCount(),
		"警告数", len(app.Warnings))

	return app, nil
}

// Start 启动全部插件并派发 system.startup。
//
// 顺序：插件按拓扑序 Start，停机时逆序 Stop。
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return errors.New("assemble: 已经启动过")
	}
	a.started = true
	a.mu.Unlock()

	for _, p := range a.Plugins {
		id := p.Manifest().Metadata.ID
		a.Log.Info("启动插件", "plugin", id)
		if err := p.Start(ctx); err != nil {
			// 启动失败要回滚已启动的部分，避免留下半启动状态
			a.Log.Error("插件启动失败，开始回滚", "plugin", id, "err", err)
			a.stopAll(context.Background())
			return fmt.Errorf("plugin %s: Start 失败: %w", id, err)
		}
	}

	a.Bus.Notify(ctx, &event.Event{
		Type:   event.SystemStartup,
		Source: "kernel",
		Data: map[string]any{
			"started_at": a.startedAt,
			"plugins":    len(a.Plugins),
		},
	})
	return nil
}

// Stop 停机：逆序停止插件，派发 shutdown 事件，关闭数据库。
func (a *App) Stop(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.started {
		return
	}
	a.started = false

	a.Bus.Notify(ctx, &event.Event{Type: event.SystemShutdown, Source: "kernel"})
	a.stopAll(ctx)

	if a.DB != nil {
		if err := a.DB.Close(); err != nil {
			a.Log.Warn("关闭数据库失败", "err", err)
		}
	}
	a.Log.Info("已停止", "运行时长", time.Since(a.startedAt).Round(time.Second).String())
}

func (a *App) stopAll(ctx context.Context) {
	// 逆序停止：让依赖方先于被依赖方退出
	for i := len(a.Plugins) - 1; i >= 0; i-- {
		p := a.Plugins[i]
		id := p.Manifest().Metadata.ID
		if err := p.Stop(ctx); err != nil {
			a.Log.Warn("插件停止失败", "plugin", id, "err", err)
		}
	}
}

// Uptime 返回运行时长。
func (a *App) Uptime() time.Duration { return time.Since(a.startedAt) }

// PluginSummaries 返回全部插件的摘要，供后台 /api/admin/plugins 使用。
func (a *App) PluginSummaries() []PluginSummary {
	out := make([]PluginSummary, 0, len(a.Plugins)+len(a.DeclarativePlugins))

	for _, p := range a.Plugins {
		m := p.Manifest()
		env := a.Envs[m.Metadata.ID]
		s := PluginSummary{
			ID:          m.Metadata.ID,
			Name:        m.Metadata.Name,
			Version:     m.Metadata.Version,
			Description: m.Metadata.Description,
			Kind:        "builtin",
			Loaded:      true,
		}
		if env != nil {
			s.Permissions = env.Perms.List()
			s.Denied = env.Perms.Denied()
		}
		s.Capabilities = a.capabilitiesOf(m.Metadata.ID)
		out = append(out, s)
	}

	for _, m := range a.DeclarativePlugins {
		out = append(out, PluginSummary{
			ID:          m.Metadata.ID,
			Name:        m.Metadata.Name,
			Version:     m.Metadata.Version,
			Description: m.Metadata.Description,
			Kind:        "declarative",
			Loaded:      false, // 尚未有执行器
			Permissions: m.Spec.Permissions,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (a *App) capabilitiesOf(ownerID string) []string {
	var out []string
	for _, e := range a.Registry.All() {
		if e.Owner == ownerID {
			out = append(out, e.Capability.Name())
		}
	}
	sort.Strings(out)
	return out
}

// PluginSummary 是插件的对外摘要。
type PluginSummary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Kind         string   `json:"kind"` // builtin | declarative
	Loaded       bool     `json:"loaded"`
	Permissions  []string `json:"permissions"`
	Denied       []string `json:"denied,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// ────────────────────── 内部辅助 ──────────────────────

func buildEnv(cfg *config.Config, log logx.Logger, app *App, m *plugin.Manifest) (*plugin.Env, error) {
	perms := plugin.NewPermissionSet(grantedPermissions(m, cfg))

	return &plugin.Env{
		Cfg:      cfg,
		Log:      log.With("plugin", m.Metadata.ID),
		Registry: app.Registry,
		Bus:      app.Bus,
		DB:       app.DB,
		Perms:    perms,
		ID:       m.Metadata.ID,
	}, nil
}

// grantedPermissions 计算插件实际拿到的权限。
//
// 当前策略：清单声明什么就给什么。
// 未来接入插件市场时，这里改成读数据库里的管理员授权记录，
// 并集上"清单要求但被拒绝"的清单供 UI 展示。
func grantedPermissions(m *plugin.Manifest, cfg *config.Config) []string {
	return m.Spec.Permissions
}

func findManifest(ms []*plugin.Manifest, id string) (*plugin.Manifest, bool) {
	for _, m := range ms {
		if m.Metadata.ID == id {
			return m, true
		}
	}
	return nil, false
}

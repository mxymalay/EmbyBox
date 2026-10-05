package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"

	"github.com/embyone/embyone/pkg/kernel/config"
	"github.com/embyone/embyone/pkg/kernel/event"
	"github.com/embyone/embyone/pkg/kernel/logx"
	"github.com/embyone/embyone/pkg/kernel/registry"
	"github.com/embyone/embyone/pkg/kernel/store"
)

// Plugin 是内核认识的插件形态。
//
// 生命周期：
//
//	Register  —— 装配阶段。注册能力、订阅事件、声明迁移。
//	             **此时不能发起网络请求**：媒体服务器可能还没就绪，
//	             而且插件之间的注册顺序是有依赖的。
//	Start     —— 全部插件 Register 完成后调用。此时可以安全地
//	             使用其他插件注册的能力、启动后台 goroutine。
//	Stop      —— 停机。释放外部资源，让后台任务退出。
type Plugin interface {
	// Manifest 返回插件元信息与声明。
	Manifest() *Manifest

	// Register 装配。用 env 提供的能力完成自我注册。
	Register(ctx context.Context, env *Env) error

	// Start 启动。可选的耗时初始化放这里。
	Start(ctx context.Context) error

	// Stop 停止。
	Stop(ctx context.Context) error
}

// Env 是插件与内核交互的唯一通道。
//
// 插件不应该 import 除 contract / event / logx 之外的任何内核包，
// 一切能力都从 Env 取。这样内核重构时插件不受影响。
type Env struct {
	// Cfg 启动期配置。
	Cfg *config.Config

	// Log 已绑定插件身份的 logger。
	Log logx.Logger

	// Registry 能力注册表。注册自己的实现，或取用别人的。
	Registry *registry.Registry

	// Bus 事件总线。
	Bus *event.Bus

	// DB 数据库句柄。插件应通过 Migrations 声明表结构，
	// 而不是在这里直接建表——否则迁移记录会缺失。
	DB *store.DB

	// Perms 本插件被授予的权限集。
	Perms *PermissionSet

	// ID 本插件 ID，写审计日志时用。
	ID string

	// migrations 由插件通过 AddMigration 声明，内核统一执行。
	migrations []store.Migration
	mu         sync.Mutex
}

// AddMigration 声明一个迁移。
//
// 插件应在 Register 阶段调用，Version 从 1000 起自增
// （1-999 保留给内核，避免将来内核加表时与插件冲突）。
func (e *Env) AddMigration(version int, name, sqlText string) error {
	if version < 1000 {
		return fmt.Errorf("plugin %s: 迁移版本号必须 >= 1000（当前 %d），1-999 由内核保留", e.ID, version)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, m := range e.migrations {
		if m.Version == version {
			return fmt.Errorf("plugin %s: 迁移版本号 %d 重复", e.ID, version)
		}
	}
	e.migrations = append(e.migrations, store.Migration{
		Version: version, Name: name, SQL: sqlText, Owner: e.ID,
	})
	return nil
}

// Migrations 返回本插件声明的全部迁移，供装配器统一执行。
func (e *Env) Migrations() []store.Migration {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]store.Migration, len(e.migrations))
	copy(out, e.migrations)
	return out
}

// InTx 在本插件的数据库事务里执行 fn。
func (e *Env) InTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if e.DB == nil {
		return fmt.Errorf("plugin %s: 数据库不可用", e.ID)
	}
	return e.DB.InTx(ctx, fn)
}

// RequirePermission 便捷方法：检查本插件是否持有权限。
func (e *Env) RequirePermission(perm string) error {
	return RequirePermission(e.ID, e.Perms, perm)
}

// ────────────────────── 内置插件注册 ──────────────────────

// builtinRegistry 保存编译进主程序的插件构造函数。
//
// 为什么用「构造函数」而不是「实例」：
// 内置插件可能需要在构造时拿到编译期常量或做参数校验，
// 而且每次装配都应该是干净的新实例（测试里会重复装配）。
var (
	builtinMu        sync.RWMutex
	builtinFactories []BuiltinFactory
)

// BuiltinFactory 是内置插件的构造函数。
type BuiltinFactory func() Plugin

// Builtin 注册一个内置插件（L2）。
//
// 必须在 main 或 internal/builtin 包里显式调用——Go 没有 import 副作用
// 之外的自动发现机制，而这个显式清单本身就是一份"系统有哪些内置能力"的文档。
func Builtin(f BuiltinFactory) {
	builtinMu.Lock()
	defer builtinMu.Unlock()
	builtinFactories = append(builtinFactories, f)
}

// Builtins 返回全部已注册的内置插件实例，按 ID 排序保证顺序稳定。
func Builtins() []Plugin {
	builtinMu.RLock()
	factories := make([]BuiltinFactory, len(builtinFactories))
	copy(factories, builtinFactories)
	builtinMu.RUnlock()

	out := make([]Plugin, 0, len(factories))
	for _, f := range factories {
		if p := f(); p != nil {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Manifest().Metadata.ID < out[j].Manifest().Metadata.ID
	})
	return out
}

// BasePlugin 提供 Plugin 接口的默认实现，内置插件可以内嵌它，
// 只覆写自己关心的方法。
//
// 用内嵌而不是强制实现四个方法，是为了让最简单的插件
// （只需要 Register 订阅一个事件）只有十几行代码。
type BasePlugin struct {
	manifest *Manifest
}

// NewBasePlugin 用清单构造基础插件。
func NewBasePlugin(m *Manifest) BasePlugin {
	m.Builtin = true
	return BasePlugin{manifest: m}
}

// Manifest 返回清单。
func (b *BasePlugin) Manifest() *Manifest { return b.manifest }

// Register 默认什么都不做。
func (b *BasePlugin) Register(ctx context.Context, env *Env) error { return nil }

// Start 默认什么都不做。
func (b *BasePlugin) Start(ctx context.Context) error { return nil }

// Stop 默认什么都不做。
func (b *BasePlugin) Stop(ctx context.Context) error { return nil }

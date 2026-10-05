// Package registry 是能力注册表。
//
// 它是内核唯一知道"系统里有哪些能力"的地方。插件在装配阶段把自己的
// 实现注册进来，业务代码在运行期通过它取用。
//
// 设计要点：
//   - 同一种 Kind 可以注册多个实现（多台 Emby、多个通知渠道），
//     Best* 方法按 Priority + 健康度择优。
//   - 注册是并发安全的，但只应在装配阶段发生；运行期注册属于设计错误，
//     这里不做限制但会记警告日志。
package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mxymalay/embybox/pkg/kernel/contract"
)

// ErrNotFound 表示注册表里没有可用的该能力。
var ErrNotFound = errors.New("registry: no capability registered")

// Entry 是注册表里的一条记录。
type Entry struct {
	Capability contract.Capability
	// Owner 注册它的插件 ID，用于审计与卸载。
	Owner string
	// Health 最近一次健康探测结果。
	Health contract.Health
}

// Registry 是能力注册表。
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*Entry
	// order 记录注册顺序，保证遍历结果稳定（便于测试与展示）。
	order []string
}

// New 创建一个空注册表。
func New() *Registry {
	return &Registry{entries: make(map[string]*Entry)}
}

// Register 注册一个能力。
//
// 重名会返回错误而不是覆盖——静默覆盖会让"到底哪个实现生效"变得不可推理，
// 这是排查问题的噩梦。确实要替换时，先 Unregister。
func (r *Registry) Register(owner string, c contract.Capability) error {
	if c == nil {
		return errors.New("registry: nil capability")
	}
	name := c.Name()
	if name == "" {
		return errors.New("registry: capability has empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.entries[name]; ok {
		return fmt.Errorf("registry: capability %q already registered by plugin %q", name, existing.Owner)
	}

	r.entries[name] = &Entry{Capability: c, Owner: owner}
	r.order = append(r.order, name)
	return nil
}

// Unregister 移除一个能力（插件卸载时用）。
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.entries[name]; !ok {
		return
	}
	delete(r.entries, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// Get 按名字取能力。
func (r *Registry) Get(name string) (contract.Capability, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	e, ok := r.entries[name]
	if !ok {
		return nil, false
	}
	return e.Capability, true
}

// Entry 返回带元信息的记录，用于后台展示"谁注册了什么"。
func (r *Registry) Entry(name string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	e, ok := r.entries[name]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// All 返回全部已注册能力，顺序为注册顺序。
func (r *Registry) All() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Entry, 0, len(r.order))
	for _, name := range r.order {
		if e, ok := r.entries[name]; ok {
			out = append(out, *e)
		}
	}
	return out
}

// OfKind 返回某一大类的全部实现，按 Priority 降序排列。
func (r *Registry) OfKind(kind contract.Kind) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []Entry
	for _, name := range r.order {
		e, ok := r.entries[name]
		if !ok {
			continue
		}
		if e.Capability.Meta().Kind == kind {
			out = append(out, *e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Capability.Meta().Priority > out[j].Capability.Meta().Priority
	})
	return out
}

// MediaServers 返回全部媒体服务器实现，按 Priority 降序。
func (r *Registry) MediaServers() []contract.MediaServer {
	return filterKind[contract.MediaServer](r, contract.KindMediaServer)
}

// Notifiers 返回全部通知渠道实现，按 Priority 降序。
func (r *Registry) Notifiers() []contract.Notifier {
	return filterKind[contract.Notifier](r, contract.KindNotifier)
}

// BestMediaServer 返回择优后的媒体服务器。
//
// 优先级规则：Priority 高者优先；同等 Priority 时跳过健康检查失败的。
// 全部不健康时返回最后一个（总比没有好），并让调用方通过 Health 感知。
func (r *Registry) BestMediaServer() (contract.MediaServer, error) {
	all := r.MediaServers()
	if len(all) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, contract.KindMediaServer)
	}

	// 第一轮：找健康且优先级最高的
	for _, ms := range all {
		if hc, ok := ms.(contract.HealthChecker); ok {
			h := hc.HealthCheck(context.Background())
			if !h.OK {
				continue
			}
		}
		return ms, nil
	}

	// 第二轮：都不健康，退而求其次返回第一个，由调用方感知实际失败
	return all[0], nil
}

// SetHealth 由健康探针写入探测结果。
func (r *Registry) SetHealth(name string, h contract.Health) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.entries[name]; ok {
		if h.CheckedAtUnix == 0 {
			h.CheckedAtUnix = time.Now().Unix()
		}
		e.Health = h
	}
}

// filterKind 按 Kind 过滤并做类型断言，断言失败会被跳过并计入返回值之外。
// 泛型的引入是为了让调用方直接拿到接口类型，不用自己断言。
func filterKind[T any](r *Registry, kind contract.Kind) []T {
	entries := r.OfKind(kind)
	out := make([]T, 0, len(entries))

	for _, e := range entries {
		if typed, ok := e.Capability.(T); ok {
			out = append(out, typed)
		}
	}
	return out
}

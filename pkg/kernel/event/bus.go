// Package event 是事件总线，也是 EmbyOne 唯一的扩展点机制。
//
// 为什么用事件而不是直接函数调用：
//
//	插件之间不应该互相认识。求片插件不应该 import Telegram 插件。
//	它只需要在"求片创建成功"时抛一个事件，谁关心谁订阅。
//	这样拆掉任何一个插件，其他插件照常工作。
//
// 命名规范：<领域>.<实体>.<时机>
//
//	时机只有四个标准词，见 event.go 中的常量说明。
package event

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"sync"

	"github.com/embyone/embyone/pkg/kernel/logx"
)

// Event 是一次事件派发。
type Event struct {
	// Type 事件类型，形如 "user.register.after"。
	Type string

	// Data 输入数据。订阅者可以读，也可以在 before 型事件里改写。
	Data map[string]any

	// Result 输出数据。provide 型事件的订阅者把结果写在这里。
	Result map[string]any

	// Source 派发者标识（插件 ID 或 "kernel"），用于审计与防自杀式回环。
	Source string
}

// Handler 是事件处理函数。
//
// 返回 error 的含义取决于事件时机：
//   - before / check 型：非 nil 会中断流程（这是"否决权"）
//   - after / provide 型：非 nil 只记日志，不影响其他订阅者
type Handler func(ctx context.Context, e *Event) error

// Subscription 是一次订阅。
type Subscription struct {
	// Pattern 事件类型匹配模式，支持 '*' 通配（如 "user.*"）。
	Pattern string

	// Priority 执行顺序，小的先执行。默认 100。
	Priority int

	// Handler 处理函数。
	Handler Handler

	// Owner 订阅者插件 ID。
	Owner string
}

// Bus 是事件总线。
type Bus struct {
	mu   sync.RWMutex
	subs map[string][]Subscription // pattern -> subs
	log  logx.Logger
}

// NewBus 创建事件总线。
func NewBus(log logx.Logger) *Bus {
	if log == nil {
		log = logx.Discard()
	}
	return &Bus{subs: make(map[string][]Subscription), log: log}
}

// On 订阅事件。
//
// pattern 支持两级通配：
//
//	"user.register.after"  精确匹配
//	"user.register.*"      匹配该前缀下的任意时机
//	"user.*"               匹配 user 领域全部事件
//	"*"                    匹配一切（慎用，会收到大量噪音）
func (b *Bus) On(owner, pattern string, priority int, h Handler) error {
	if pattern == "" {
		return errors.New("event: empty pattern")
	}
	if h == nil {
		return errors.New("event: nil handler")
	}
	if priority == 0 {
		priority = 100
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// 同一 owner 同一 pattern 重复订阅视为配置错误，直接覆盖并警告，
	// 避免同一个钩子被执行两次（这在积分扣减场景会造成真实损失）。
	for i, s := range b.subs[pattern] {
		if s.Owner == owner {
			b.log.Warn("重复订阅同一事件，已覆盖", "plugin", owner, "event", pattern)
			b.subs[pattern][i] = Subscription{Pattern: pattern, Priority: priority, Handler: h, Owner: owner}
			return nil
		}
	}

	b.subs[pattern] = append(b.subs[pattern], Subscription{
		Pattern: pattern, Priority: priority, Handler: h, Owner: owner,
	})
	return nil
}

// Publish 同步派发事件并返回第一个错误。
//
// 用于 before / check 型事件：任何订阅者返回 error 都会中断整条流程，
// 让调用方能立即知道"被谁否决了、为什么"。
func (b *Bus) Publish(ctx context.Context, e *Event) error {
	subs := b.match(e.Type)
	for _, s := range subs {
		if err := s.Handler(ctx, e); err != nil {
			b.log.Warn("事件订阅者返回错误，流程中断",
				"event", e.Type, "plugin", s.Owner, "err", err)
			return fmt.Errorf("event %s rejected by %s: %w", e.Type, s.Owner, err)
		}
	}
	return nil
}

// Notify 异步语义的同步实现：派发事件但不中断流程，错误只记日志。
//
// 用于 after 型事件：注册成功了就是成功了，
// 发欢迎消息失败不应该让用户看到 500。
func (b *Bus) Notify(ctx context.Context, e *Event) {
	for _, s := range b.match(e.Type) {
		if err := s.Handler(ctx, e); err != nil {
			b.log.Warn("事件订阅者执行失败（已忽略）",
				"event", e.Type, "plugin", s.Owner, "err", err)
		}
	}
}

// Collect 派发 provide 型事件，收集所有订阅者写入的 Result。
//
// 返回的每一项都带来源标识，调用方可以按需取舍或加权。
// 单个订阅者失败不影响其他订阅者——"一源不可用不拖垮整条链"。
func (b *Bus) Collect(ctx context.Context, e *Event) []Collected {
	subs := b.match(e.Type)
	out := make([]Collected, 0, len(subs))

	for _, s := range subs {
		// 每个订阅者独立的事件副本，避免互相污染 Result
		local := &Event{Type: e.Type, Data: e.Data, Result: map[string]any{}, Source: s.Owner}

		if err := s.Handler(ctx, local); err != nil {
			b.log.Warn("provide 型订阅者失败（已跳过）",
				"event", e.Type, "plugin", s.Owner, "err", err)
			continue
		}
		if len(local.Result) == 0 {
			continue
		}
		out = append(out, Collected{Owner: s.Owner, Result: local.Result})
	}
	return out
}

// Collected 是 provide 型事件的一个订阅者贡献的结果。
type Collected struct {
	Owner  string
	Result map[string]any
}

// match 返回按 Priority 升序排列的匹配订阅者。
func (b *Bus) match(eventType string) []Subscription {
	b.mu.RLock()
	var flat []Subscription
	for pattern, subs := range b.subs {
		if matchPattern(pattern, eventType) {
			flat = append(flat, subs...)
		}
	}
	b.mu.RUnlock()

	sort.SliceStable(flat, func(i, j int) bool {
		return flat[i].Priority < flat[j].Priority
	})
	return flat
}

// SubscriptionCount 返回订阅总数，供 /healthz 与后台展示。
func (b *Bus) SubscriptionCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	n := 0
	for _, subs := range b.subs {
		n += len(subs)
	}
	return n
}

// matchPattern 实现两级通配匹配。
//
// 用 path.Match 的语义：'*' 不跨越 '.'，所以 "user.*" 匹配
// "user.register" 但不匹配 "user.register.after"。
// 这里额外支持尾部 "**" 表示跨级通配。
func matchPattern(pattern, eventType string) bool {
	if pattern == eventType {
		return true
	}
	if pattern == "*" {
		return true
	}
	// 尾部 ** —— 匹配任意深度后缀
	if len(pattern) > 2 && pattern[len(pattern)-2:] == "**" {
		prefix := pattern[:len(pattern)-2]
		return len(eventType) >= len(prefix) && eventType[:len(prefix)] == prefix
	}
	ok, err := path.Match(pattern, eventType)
	return err == nil && ok
}

package httpapi

import (
	"sync"
	"time"
)

// ipLimiter 是一个内存滑动窗口计数器。
//
// 选它而不是引入 golang.org/x/time/rate 的理由：
// 我们需要的是"按任意 key 计数"（key 里带 IP + 路径），
// 而且限流粒度是分钟级，滑动窗口计数器的精度足够。
// 少一个依赖，少一份需要理解的东西。
//
// 局限（已知且接受）：
//   - 单机内存态，多实例部署时每个实例各自计数。对"防刷"这个目标够用；
//     要全局精确限流需要 Redis，那是 M5 之后的事。
//   - 桶按窗口过期清理，内存占用与活跃 key 数成正比。
type ipLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	count   int
	resetAt time.Time
}

func newIPLimiter() *ipLimiter {
	l := &ipLimiter{buckets: make(map[string]*bucket)}
	go l.gcLoop()
	return l
}

// allow 判断该 key 在窗口内是否还有配额。
//
// limit 为窗口内允许的最大次数；window 为窗口长度。
// 返回 true 表示放行（并计入一次）。
func (l *ipLimiter) allow(key string, limit int, window time.Duration) bool {
	if limit <= 0 {
		return true
	}

	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok || now.After(b.resetAt) {
		l.buckets[key] = &bucket{count: 1, resetAt: now.Add(window)}
		return true
	}

	if b.count >= limit {
		return false
	}
	b.count++
	return true
}

// remaining 返回剩余配额，供响应头 X-RateLimit-Remaining 使用。
func (l *ipLimiter) remaining(key string, limit int) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok || time.Now().After(b.resetAt) {
		return limit
	}
	if b.count >= limit {
		return 0
	}
	return limit - b.count
}

// gcLoop 定期清理过期桶。
//
// 不清理的话，一次扫描器攻击就能留下几十万个 key，
// 把内存吃掉。这是自研限流器最常见的疏漏。
func (l *ipLimiter) gcLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		l.mu.Lock()
		for k, b := range l.buckets {
			if now.After(b.resetAt) {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

// stats 返回当前桶数量，用于 /healthz 的调试输出。
func (l *ipLimiter) stats() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

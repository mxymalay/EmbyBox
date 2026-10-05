package plugin

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// 权限常量表。
//
// 设计原则：权限按「能力域.动作」命名，粒度宁可粗一点。
// 太细的权限（如 user.read.username）会让插件作者疲于声明、
// 审核者疲于核对，最后大家都选择一次性全要——那还不如不设。
//
// 未声明的权限，调用直接失败并记审计日志。这是硬约束，
// 不是文档里的君子协定。
const (
	// ─── 媒体服务器 ───
	PermMediaServerRead  = "media-server.read"  // 查用户、片库、播放记录
	PermMediaServerWrite = "media-server.write" // 建号、改权限、禁用

	// ─── 用户 ───
	PermUserRead  = "user.read"
	PermUserWrite = "user.write"

	// ─── 通知 ───
	PermNotifySend = "notify.send"

	// ─── 网络 ───
	PermHTTPOutbound = "http.outbound" // 出站请求，需配合 allowed_hosts

	// ─── 存储 ───
	PermKVRead    = "kv.read"
	PermKVWrite   = "kv.write"
	PermDBMigrate = "db.migrate"

	// ─── 系统 ───
	PermScheduleRegister = "schedule.register"
	PermRouteRegister    = "route.register"
	PermUIRegister       = "ui.register"

	// ─── 计费与工单（M3/M4 起用）───
	PermPointsWrite  = "points.write"
	PermOrderRead    = "order.read"
	PermRequestWrite = "request.write"
)

// allPermissions 是内核认得的全部权限。
// 清单里出现未知权限会告警——通常意味着插件作者记错了名字。
var allPermissions = []string{
	PermMediaServerRead, PermMediaServerWrite,
	PermUserRead, PermUserWrite,
	PermNotifySend,
	PermHTTPOutbound,
	PermKVRead, PermKVWrite, PermDBMigrate,
	PermScheduleRegister, PermRouteRegister, PermUIRegister,
	PermPointsWrite, PermOrderRead, PermRequestWrite,
}

// KnownPermission 判断权限名是否在内核的权限表里。
func KnownPermission(p string) bool {
	for _, k := range allPermissions {
		if k == p {
			return true
		}
	}
	return false
}

// AllPermissions 返回全部已知权限（供后台展示"插件能拿到什么"）。
func AllPermissions() []string {
	out := make([]string, len(allPermissions))
	copy(out, allPermissions)
	return out
}

// PermissionSet 是一个插件被授予的权限集合。
//
// 并发安全：运行期会有多个 goroutine 查询它。
type PermissionSet struct {
	mu     sync.RWMutex
	grants map[string]bool
	// denied 记录被管理员显式拒绝的权限。
	// 与"未声明"区分开是为了 UI 能提示"该插件要了权限但被拒了"。
	denied map[string]bool
}

// NewPermissionSet 由声明清单构建权限集。
func NewPermissionSet(declared []string) *PermissionSet {
	ps := &PermissionSet{grants: make(map[string]bool), denied: make(map[string]bool)}
	for _, p := range declared {
		ps.grants[strings.TrimSpace(p)] = true
	}
	return ps
}

// Has 判断是否持有某权限。
func (p *PermissionSet) Has(perm string) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.grants[perm]
}

// Grant 运行期追加授权（管理员在后台点"允许"时调用）。
func (p *PermissionSet) Grant(perm string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.grants[perm] = true
	delete(p.denied, perm)
}

// Deny 撤销授权。
func (p *PermissionSet) Deny(perm string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.grants, perm)
	p.denied[perm] = true
}

// List 返回当前持有的权限，字典序。
func (p *PermissionSet) List() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]string, 0, len(p.grants))
	for k := range p.grants {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Denied 返回被显式拒绝的权限。
func (p *PermissionSet) Denied() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]string, 0, len(p.denied))
	for k := range p.denied {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrPermissionDenied 表示插件缺少所需权限。
type ErrPermissionDenied struct {
	Plugin     string
	Permission string
}

func (e *ErrPermissionDenied) Error() string {
	return fmt.Sprintf("plugin %s 缺少权限 %q（请在清单的 spec.permissions 中声明）",
		e.Plugin, e.Permission)
}

// RequirePermission 检查权限，不足时返回结构化错误。
//
// 调用点约定：所有访问外部资源或修改数据的入口，第一行就该是它。
func RequirePermission(pluginID string, ps *PermissionSet, perm string) error {
	if ps.Has(perm) {
		return nil
	}
	return &ErrPermissionDenied{Plugin: pluginID, Permission: perm}
}

// ValidateDeclaredPermissions 校验清单声明的权限是否都认得，
// 并把疑似拼错的权限收集起来交给调用方告警（不阻断启动）。
func ValidateDeclaredPermissions(m *Manifest) (unknown []string) {
	for _, p := range m.Spec.Permissions {
		if !KnownPermission(p) {
			unknown = append(unknown, p)
		}
	}
	return unknown
}

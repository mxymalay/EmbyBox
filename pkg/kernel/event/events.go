package event

// 事件类型常量全表。
//
// ★ 这张表就是"EmbyBox 能被扩展成什么样"的完整答案。★
//
// 想知道系统有哪些扩展点，读这一个文件就够了，不需要翻内核源码。
// docs/EVENT-CATALOG.md 由这份清单自动生成，保证文档与代码永不漂移。
//
// 命名规范：<领域>.<实体>.<时机>
//
// 时机只有四个标准词：
//
//	before   发生前。订阅者可以改写 Data，返回 error 可中断流程。
//	after    发生后。副作用居多，返回 error 只记日志，不中断。
//	check    校验点。订阅者投反对票（返回 error）即否决。
//	provide  数据源征集。收集所有订阅者的 Result，单个失败不影响其他。
const (
	// ───────────── 用户域 ─────────────

	// UserRegisterBefore 注册前校验。
	// before：可改写 Data["username"], Data["email"]；返回 error 拒绝注册。
	// 典型用途：邮箱域名白名单、邀请码校验、注册限流。
	UserRegisterBefore = "user.register.before"

	// UserRegisterAfter 注册成功后的副作用。
	// after：Data 含完整 user 对象。
	// 典型用途：发欢迎通知、赠初始积分、同步到外部系统。
	UserRegisterAfter = "user.register.after"

	// UserLoginBefore 登录前。
	// before：返回 error 拒绝登录（如账号被封）。
	UserLoginBefore = "user.login.before"

	// UserLoginCheck 登录凭证校验。
	// check：订阅者可以否决，也可以把自己当作一个认证源。
	UserLoginCheck = "user.login.check"

	// UserLoginAfter 登录成功后的副作用。
	// after：Data 含 user 与 client_ip。
	UserLoginAfter = "user.login.after"

	// UserExpireCheck 到期判定。
	// check：订阅者可以自定义"什么算过期"。
	UserExpireCheck = "user.expire.check"

	// UserExpired 用户被判定到期（状态已变为 expired）。
	// after：典型用途是发续费提醒。
	UserExpired = "user.expired"

	// UserPolicyProvide 权限策略征集。
	// provide：订阅者向 Result["policy"] 写入 contract.Policy。
	// ★ 这是"卖差价"的落地扩展点：会员等级插件在这里决定他看得到哪些片库。
	UserPolicyProvide = "user.policy.provide"

	// UserStatusChanged 用户状态发生变更。
	// after：Data 含 from/to/status_reason。用于审计与通知。
	UserStatusChanged = "user.status.changed"

	// ───────────── 媒体域 ─────────────

	// MediaServerReady 媒体服务器连接就绪。
	// after：Data["server"] 含 contract.ServerInfo。
	MediaServerReady = "media.server.ready"

	// MediaServerDegraded 媒体服务器降级或不可用。
	// after：Data["reason"] 说明原因。
	// ★ 降级必须可见：内核会把它计入 metrics 并推给通知渠道。
	MediaServerDegraded = "media.server.degraded"

	// ───────────── 系统域 ─────────────

	// SystemStartup 内核装配完成、即将对外提供服务。
	// after：插件可以做最后的初始化（如预热缓存）。
	SystemStartup = "system.startup"

	// SystemShutdown 即将停机。
	// after：插件应在此释放外部资源。
	SystemShutdown = "system.shutdown"

	// ScheduleJobProvide 定时任务征集。
	// provide：订阅者向 Result["jobs"] 写入 []JobSpec。
	ScheduleJobProvide = "schedule.job.provide"

	// RouteProvide HTTP 路由征集。
	// provide：订阅者向 Result["routes"] 写入 []RouteSpec。
	RouteProvide = "route.provide"

	// NotificationProvide 通知渠道征集。
	// provide：订阅者向 Result["notifiers"] 写入 contract.Notifier。
	NotificationProvide = "notification.provide"
)

// AllTypes 返回内核对全部已知事件类型的枚举。
//
// 用途：
//   - 启动时校验插件的 hooks 声明，订阅不存在的事件会警告
//   - 生成 docs/EVENT-CATALOG.md
//   - 后台"扩展点"页面展示
func AllTypes() []TypeInfo {
	return []TypeInfo{
		{Type: UserRegisterBefore, Domain: "user", Timing: TimingBefore, Summary: "注册前校验，可拒绝或改写输入"},
		{Type: UserRegisterAfter, Domain: "user", Timing: TimingAfter, Summary: "注册成功后副作用"},
		{Type: UserLoginBefore, Domain: "user", Timing: TimingBefore, Summary: "登录前校验，可拒绝"},
		{Type: UserLoginCheck, Domain: "user", Timing: TimingCheck, Summary: "凭证校验，可外挂认证源"},
		{Type: UserLoginAfter, Domain: "user", Timing: TimingAfter, Summary: "登录成功后副作用"},
		{Type: UserExpireCheck, Domain: "user", Timing: TimingCheck, Summary: "自定义到期判定"},
		{Type: UserExpired, Domain: "user", Timing: TimingAfter, Summary: "用户到期"},
		{Type: UserPolicyProvide, Domain: "user", Timing: TimingProvide, Summary: "权限策略征集（分级会员）"},
		{Type: UserStatusChanged, Domain: "user", Timing: TimingAfter, Summary: "状态变更审计"},
		{Type: MediaServerReady, Domain: "media", Timing: TimingAfter, Summary: "媒体服务器就绪"},
		{Type: MediaServerDegraded, Domain: "media", Timing: TimingAfter, Summary: "媒体服务器降级告警"},
		{Type: SystemStartup, Domain: "system", Timing: TimingAfter, Summary: "内核装配完成"},
		{Type: SystemShutdown, Domain: "system", Timing: TimingAfter, Summary: "即将停机"},
		{Type: ScheduleJobProvide, Domain: "system", Timing: TimingProvide, Summary: "注册定时任务"},
		{Type: RouteProvide, Domain: "system", Timing: TimingProvide, Summary: "注册 HTTP 路由"},
		{Type: NotificationProvide, Domain: "system", Timing: TimingProvide, Summary: "注册通知渠道"},
	}
}

// Timing 是事件时机。
type Timing string

const (
	TimingBefore  Timing = "before"
	TimingAfter   Timing = "after"
	TimingCheck   Timing = "check"
	TimingProvide Timing = "provide"
)

// TypeInfo 描述一个事件类型。
type TypeInfo struct {
	Type    string `json:"type"`
	Domain  string `json:"domain"`
	Timing  Timing `json:"timing"`
	Summary string `json:"summary"`
}

// IsKnown 判断事件类型是否在内核的事件表里。
// 未知事件不阻止派发（插件可能自定义事件），但装配时会告警。
func IsKnown(t string) bool {
	for _, info := range AllTypes() {
		if info.Type == t {
			return true
		}
	}
	return false
}

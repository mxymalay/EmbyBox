// Package contract 定义 EmbyBox 内核认识的**全部**抽象。
//
// 设计铁律：本包不允许出现任何具体外部系统的名字。
// 不出现 "emby"、"jellyfin"、"telegram"、"qbittorrent"——
// 这些都在 internal/ 里，通过实现本包接口接入内核。
//
// 内核只认三样东西：契约（本包）、注册表（registry）、事件（event）。
package contract

import "context"

// Capability 是所有能力的共同基座。
// 任何要注册进内核的东西都必须实现它。
type Capability interface {
	// Name 全局唯一标识，形如 "media-server.emby"、"notifier.telegram"。
	// 内核用它在注册表里寻址，日志和审计也用它。
	Name() string

	// Meta 返回描述性元数据。
	Meta() Meta
}

// Meta 描述一个能力的静态信息。
type Meta struct {
	// Kind 能力大类，决定它会被注册表归入哪个槽位。
	// 取值见 Kind* 常量。
	Kind Kind

	// Display 人类可读的名字，用于后台展示。
	Display string

	// Description 一句话说明。
	Description string

	// Version 实现版本，语义化版本。
	Version string

	// Priority 同 Kind 下多个实现时的择优权重，越大越优先。
	// 0 表示普通优先级。
	Priority int
}

// Kind 是能力大类的枚举。
type Kind string

const (
	KindMediaServer Kind = "media-server" // 媒体服务器：Emby / Jellyfin / Plex
	KindNotifier    Kind = "notifier"     // 通知渠道：Telegram / 邮件 / Webhook
	KindDownloader  Kind = "downloader"   // 下载器（M3 起用）
	KindResource    Kind = "resource"     // 资源提供者（M3 起用）
)

// Health 表示一个能力的健康状态。
type Health struct {
	// OK 是否健康。
	OK bool `json:"ok"`
	// Message 不健康时的原因，健康时可为空。
	Message string `json:"message,omitempty"`
	// CheckedAtUnix 上次探测时间（Unix 秒）。0 表示从未探测。
	CheckedAtUnix int64 `json:"checked_at_unix,omitempty"`
}

// HealthChecker 是可选接口：能力想被注册表纳入健康择优，就实现它。
type HealthChecker interface {
	HealthCheck(ctx context.Context) Health
}

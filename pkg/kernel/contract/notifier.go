package contract

import "context"

// Notifier 是通知渠道能力。
//
// EmbyOne 的所有对外消息（到期提醒、求片进度、告警）都通过它发出。
// 同一时刻可以有多个 Notifier 注册，内核按 Message.Channel 路由；
// Channel 为空时扇出到全部渠道。
type Notifier interface {
	Capability

	// Send 发送一条消息。
	//
	// 实现者注意：这是"尽力而为"的调用，失败必须返回 error 让上层记审计；
	// 不要在这里做重试——重试策略由调度器统一负责。
	Send(ctx context.Context, msg Message) error

	// Test 发送一条测试消息，用于后台"点一下看通不通"。
	Test(ctx context.Context) error
}

// Message 是一条待发送的通知。
type Message struct {
	// Channel 目标渠道标识，对应 Notifier.Name()。
	// 为空时由内核扇出到所有已注册渠道。
	Channel string `json:"channel,omitempty"`

	// To 接收者标识。语义由渠道决定：
	// Telegram 是 chat_id，邮件是地址，Webhook 可为空。
	To string `json:"to,omitempty"`

	// Subject 主题/标题。
	Subject string `json:"subject,omitempty"`

	// Body 正文，支持渠道自身的富文本语法。
	Body string `json:"body"`

	// Level 消息级别，决定是否需要告警渠道。
	Level Level `json:"level,omitempty"`

	// Meta 附加字段，供特定渠道使用（如 Telegram 的 inline keyboard）。
	Meta map[string]string `json:"meta,omitempty"`
}

// Level 是消息级别。
type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

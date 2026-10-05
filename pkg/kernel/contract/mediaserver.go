package contract

import (
	"context"
	"errors"
	"time"
)

// MediaServer 是媒体服务器能力。
//
// 它是"人"与"片"的交汇点：EmbyBox 通过它建号、改权限、查片库、取播放记录。
// 任何实现了它的提供者（Emby / Jellyfin / Plex / 自研）都能被内核调度。
//
// 实现者注意：
//   - 所有方法都必须是幂等的，除非注释明确说明不是。
//   - 网络调用必须尊重 ctx 的超时与取消。
//   - 不要在这里做本地落库，持久化是调用方的责任。
type MediaServer interface {
	Capability

	// Ping 探测服务可达性与凭证有效性，用于健康检查。
	Ping(ctx context.Context) error

	// ServerInfo 返回服务器基本信息（名称、版本、ID）。
	ServerInfo(ctx context.Context) (ServerInfo, error)

	// Users 列出全部用户。
	Users(ctx context.Context) ([]MSUser, error)

	// UserByID 按服务器侧 ID 查单个用户，不存在时返回 ErrNotFound。
	UserByID(ctx context.Context, id string) (MSUser, error)

	// CreateUser 创建用户。用户名已存在时返回 ErrAlreadyExists。
	CreateUser(ctx context.Context, spec UserSpec) (MSUser, error)

	// DeleteUser 删除用户。用户不存在时返回 nil（幂等）。
	DeleteUser(ctx context.Context, id string) error

	// SetDisabled 启用/禁用用户。这是"到期封号"落地到远端的唯一入口。
	SetDisabled(ctx context.Context, id string, disabled bool) error

	// SetPassword 重置用户密码。
	SetPassword(ctx context.Context, id string, password string) error

	// ApplyPolicy 把权限策略写到远端。
	//
	// 这是"卖差价"的落地入口：不同会员等级写入不同的 Policy，
	// 决定他能看哪些片库、能不能下载、能不能转码、能开几个并发。
	ApplyPolicy(ctx context.Context, id string, policy Policy) error
}

// ServerInfo 描述远端媒体服务器。
type ServerInfo struct {
	// ID 服务器唯一标识，用于识别"还是不是同一台服务器"。
	ID string `json:"id"`
	// Name 服务器名称。
	Name string `json:"name"`
	// Version 服务端版本号。
	Version string `json:"version"`
	// OS 服务端操作系统描述。
	OS string `json:"os,omitempty"`
}

// MSUser 是远端用户的投影。
type MSUser struct {
	// ID 远端用户 ID。EmbyBox 的 users.emby_id 存的就是它。
	ID string `json:"id"`
	// Name 用户名。
	Name string `json:"name"`
	// Disabled 远端是否处于禁用状态。
	Disabled bool `json:"disabled"`
	// HasPassword 是否设置了密码。
	HasPassword bool `json:"has_password"`
	// LastLogin 最近登录时间，零值表示未知。
	LastLogin time.Time `json:"last_login,omitempty"`
	// LastActivity 最近活动时间，零值表示未知。
	LastActivity time.Time `json:"last_activity,omitempty"`
}

// UserSpec 是创建用户的输入。
type UserSpec struct {
	// Name 用户名。
	Name string
	// Password 初始密码，空字符串表示不设密码（不推荐）。
	Password string
	// Policy 初始权限策略。
	Policy Policy
}

// Policy 是媒体服务器的权限策略。
//
// 字段刻意做得宽：不同媒体服务器只实现自己能表达的部分，
// 无法表达的部分由调用方记入审计并在 UI 上标注"该服务器不支持"。
type Policy struct {
	// Disabled 总开关：为 true 时用户完全无法播放。
	Disabled bool `json:"disabled"`

	// CanLogin 能否登录 EmbyBox 用户中心。
	//
	// 注意：这**不是**媒体服务器的属性，是 EmbyBox 自己的判定结果。
	// 放在这里是为了让"过期用户仍可登录续费"这条规则在策略里显式可见。
	// 提供者在 ApplyPolicy 时应忽略它。
	CanLogin bool `json:"can_login"`

	// Libraries 允许访问的片库 ID 列表。空切片表示"不限制"。
	// 这是分级会员的核心：银卡只能看 movie 库，金卡能看 movie+tv。
	Libraries []string `json:"libraries,omitempty"`

	// BlockedTags 禁止访问的标签，用于内容分级与家长控制。
	BlockedTags []string `json:"blocked_tags,omitempty"`

	// MaxParentalRating 最大允许的内容分级，0 表示不限制。
	MaxParentalRating int `json:"max_parental_rating,omitempty"`

	// AllowDownload 是否允许下载到本地。
	AllowDownload bool `json:"allow_download"`

	// AllowTranscode 是否允许服务端转码。
	// 关掉可以省大量 CPU，是低档会员的常见限制。
	AllowTranscode bool `json:"allow_transcode"`

	// MaxConcurrent 最大并发播放数，0 表示不限制。
	MaxConcurrent int `json:"max_concurrent,omitempty"`
}

// 契约层的哨兵错误。提供者必须用这些错误表达可预期的失败，
// 让上层能做分支处理，而不是去解析错误字符串。
var (
	// ErrNotFound 目标资源不存在。
	ErrNotFound = errors.New("contract: not found")

	// ErrAlreadyExists 目标资源已存在。
	ErrAlreadyExists = errors.New("contract: already exists")

	// ErrUnauthorized 凭证无效或权限不足。
	ErrUnauthorized = errors.New("contract: unauthorized")

	// ErrUnsupported 提供者不支持该操作。
	// 例如某个媒体服务器没有"下载权限"这个概念。
	ErrUnsupported = errors.New("contract: unsupported operation")

	// ErrUnavailable 远端不可达或暂时性故障，调用方可以重试。
	ErrUnavailable = errors.New("contract: temporarily unavailable")
)

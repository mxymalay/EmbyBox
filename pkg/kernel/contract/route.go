package contract

import (
	"context"
	"errors"
	"net/http"
)

// AuthLevel 是路由要求的认证级别。
type AuthLevel string

const (
	// AuthNone 公开访问。**谨慎使用**：公开端点是最容易被刷的入口。
	AuthNone AuthLevel = "none"
	// AuthUser 需要登录用户。
	AuthUser AuthLevel = "user"
	// AuthAdmin 需要管理员。
	AuthAdmin AuthLevel = "admin"
	// AuthAPIKey 需要管理 API Key（供外部系统集成）。
	AuthAPIKey AuthLevel = "apikey"
)

// Principal 是认证通过后的主体。
type Principal struct {
	// UserID 本地用户 ID。
	UserID string `json:"user_id"`
	// Username 用户名，仅用于日志展示。
	Username string `json:"username"`
	// Role 角色：admin | user。
	Role string `json:"role"`
	// AuthType 认证方式：password | session | apikey | emby。
	AuthType string `json:"auth_type"`
	// Expired 标记该用户已过期。
	//
	// ★ 这是"过期用户仍可登录续费"的实现基础：
	// 过期用户拿到 Principal 后，中间件只拦住"需要播放/写入"的路由，
	// 放行个人中心与续费相关路由。
	Expired bool `json:"expired"`
}

// IsAdmin 判断是否管理员。
func (p *Principal) IsAdmin() bool { return p != nil && p.Role == "admin" }

// Authenticator 是认证能力契约。
//
// identity 插件实现它，HTTP 层通过它鉴权。
// 内核不认识 session、JWT、Cookie 这些具体机制——那是插件的自由。
type Authenticator interface {
	// Authenticate 从请求中解析主体。
	//
	// 未登录返回 ErrUnauthenticated（不是普通 error），
	// 让 HTTP 层能区分"没登录"（401）与"认证过程出错"（500）。
	Authenticate(ctx context.Context, r *http.Request) (*Principal, error)
}

// ErrUnauthenticated 表示请求未携带有效凭证。
var ErrUnauthenticated = errors.New("contract: unauthenticated")

// ErrForbidden 表示已认证但权限不足。
var ErrForbidden = errors.New("contract: forbidden")

// Route 是一条由插件注册的 HTTP 路由。
//
// 插件通过订阅 RouteProvide 事件把这些交给内核，
// 而不是直接操作 http.ServeMux —— 这样内核可以统一套中间件、
// 统一做限流与审计，插件拿到的只是"处理这个请求"的职责。
type Route struct {
	// Method HTTP 方法。空表示不限。
	Method string
	// Path 路径，支持 Go 1.22+ 的模式语法，如 "/api/v1/users/{id}"。
	Path string
	// Handler 处理函数。
	Handler http.HandlerFunc
	// Auth 认证级别。
	Auth AuthLevel
	// Summary 一句话说明，会出现在自动生成的 API 文档里。
	Summary string
	// RateLimit 形如 "60/m"、"10/s"。空表示不限。
	RateLimit string
}

// RouteBundle 是插件在一次 RouteProvide 里提交的全部路由。
// 用具体类型放进 Result，避免订阅者与收集者之间的类型断言失误。
type RouteBundle struct {
	Routes []Route
}

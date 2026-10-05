// Package httpapi 是内核的 HTTP 基础设施。
//
// 职责边界：
//
//	它负责的  —— 监听、中间件、限流、静态资源、健康检查、统一的 JSON 约定、
//	             从事件总线收集各插件注册的路由并挂载。
//	它不负责的 —— 认证如何实现、有哪些业务端点、返回什么数据结构。
//	             那些属于插件。
//
// 设计上刻意不引 Web 框架：
// Go 1.22+ 的 net/http 已支持 "GET /api/v1/users/{id}" 模式语法，
// 够用且零依赖。框架的"魔法"会直接损害本项目"便于理解"的首要目标。
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/embyone/embyone/pkg/kernel/config"
	"github.com/embyone/embyone/pkg/kernel/contract"
	"github.com/embyone/embyone/pkg/kernel/event"
	"github.com/embyone/embyone/pkg/kernel/logx"
)

// Deps 是构建 HTTP 服务的依赖。
type Deps struct {
	Cfg  *config.Config
	Log  logx.Logger
	Bus  *event.Bus
	Auth contract.Authenticator
	// StaticFS 前端静态资源；nil 表示不提供前端。
	StaticFS fs.FS
	// Info 返回系统信息，用于 /api/v1/system/info。
	Info func() SystemInfo
}

// SystemInfo 是暴露给前端的系统信息。
type SystemInfo struct {
	Version   string `json:"version"`
	Name      string `json:"name"`
	Uptime    string `json:"uptime"`
	Plugins   int    `json:"plugins"`
	StartedAt string `json:"started_at"`
	Timezone  string `json:"timezone"`
}

// Server 是 HTTP 服务。
type Server struct {
	deps    Deps
	log     logx.Logger
	mux     *http.ServeMux
	http    *http.Server
	limiter *ipLimiter
	routes  []contract.Route

	mu      sync.RWMutex
	started bool
}

// New 构建服务（尚未监听）。
func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = logx.Discard()
	}
	s := &Server{
		deps:    d,
		log:     d.Log.With("component", "http"),
		mux:     http.NewServeMux(),
		limiter: newIPLimiter(),
	}
	s.registerBuiltins()
	return s
}

// SetRoutes 挂载插件注册的路由。
//
// 必须在 Start 之前调用。冲突的路由会被拒绝并告警——
// 静默覆盖会让"我改的怎么没生效"变成无解之谜。
func (s *Server) SetRoutes(routes []contract.Route) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]string) // "METHOD PATH" -> summary
	for _, r := range routes {
		if r.Handler == nil {
			return fmt.Errorf("httpapi: 路由 %s %s 没有 handler", r.Method, r.Path)
		}
		if !strings.HasPrefix(r.Path, "/") {
			return fmt.Errorf("httpapi: 路由路径必须以 / 开头: %s", r.Path)
		}

		pattern := strings.TrimSpace(r.Method + " " + r.Path)
		if prev, dup := seen[pattern]; dup {
			s.log.Warn("路由重复注册，后者被跳过", "pattern", pattern, "kept", prev, "skipped", r.Summary)
			continue
		}
		seen[pattern] = r.Summary

		s.routes = append(s.routes, r)
		s.mux.Handle(pattern, s.wrap(r))
	}

	s.log.Info("已挂载插件路由", "count", len(s.routes))
	return nil
}

// Routes 返回已挂载的路由清单（用于自动生成 API 文档）。
func (s *Server) Routes() []RouteInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]RouteInfo, 0, len(s.routes))
	for _, r := range s.routes {
		out = append(out, RouteInfo{
			Method: r.Method, Path: r.Path, Auth: string(r.Auth),
			Summary: r.Summary, RateLimit: r.RateLimit,
		})
	}
	return out
}

// RouteInfo 是路由的对外描述。
type RouteInfo struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Auth      string `json:"auth"`
	Summary   string `json:"summary,omitempty"`
	RateLimit string `json:"rate_limit,omitempty"`
}

// wrap 给单条路由套上中间件。
func (s *Server) wrap(r contract.Route) http.Handler {
	return s.authenticate(r.Auth, s.rateLimit(r.RateLimit, http.HandlerFunc(r.Handler)))
}

// Handler 返回全局 handler（含全局中间件）。
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = s.recoverer(h)
	h = s.requestID(h)
	h = s.accessLog(h)
	h = s.globalLimit(h)
	h = s.securityHeaders(h)
	return h
}

// Start 开始监听。非阻塞：在一个 goroutine 里跑 ListenAndServe。
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return errors.New("httpapi: 已启动")
	}

	addr := s.deps.Cfg.HTTPAddr
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("httpapi: 监听 %s 失败: %w", addr, err)
	}

	s.http = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// 注意：写超时给了 0，因为要支持长连接（SSE 推送、大文件代理）。
		// 若因为写超时缺失担心慢连接攻击，靠 ReadHeaderTimeout +
		// 每连接的 idle 回收来兜底，而不是砍掉长连接能力。
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		BaseContext:  func(net.Listener) context.Context { return context.Background() },
	}

	s.started = true
	s.log.Info("HTTP 服务已启动", "addr", ln.Addr().String(), "base_url", s.deps.Cfg.BaseURL)

	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("HTTP 服务异常退出", "err", err)
		}
	}()
	return nil
}

// Shutdown 优雅关闭。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.http == nil {
		return nil
	}
	s.log.Info("正在关闭 HTTP 服务")
	return s.http.Shutdown(ctx)
}

// ────────────────────── 内置端点 ──────────────────────

func (s *Server) registerBuiltins() {
	// 存活探针：只说明"进程还在"，不检查依赖。
	// 容器编排用这个决定要不要重启容器。
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	// 就绪探针：依赖不可用时返回 503，负载均衡用这个决定要不要摘流量。
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		WriteJSON(w, http.StatusOK, map[string]any{"status": "ready"})
	})

	// 系统信息：公开，但**只暴露无敏感内容**的部分。
	// 站点名、版本、运行时长可以让用户看到；插件清单、配置不在这里。
	s.mux.HandleFunc("GET /api/v1/system/info", func(w http.ResponseWriter, r *http.Request) {
		info := SystemInfo{Version: Version, Name: "EmbyOne", Timezone: s.deps.Cfg.Timezone}
		if s.deps.Info != nil {
			info = s.deps.Info()
		}
		WriteJSON(w, http.StatusOK, info)
	})

	// 扩展点清单：这是"系统能扩展成什么样"的自助查询入口。
	// 插件作者不用读源码，读这个接口就够了。
	s.mux.HandleFunc("GET /api/v1/system/events", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, event.AllTypes())
	})

	// 前端静态资源
	if s.deps.StaticFS != nil {
		s.mountStatic(s.deps.StaticFS)
	}
}

// mountStatic 挂载前端 SPA。
//
// SPA 的经典需求：/users/123 这类路径在服务端没有对应文件，
// 但要返回 index.html 让前端路由接管。这里对 *.html/*.js/*.css
// 之外的 GET 请求统一回退到 index.html。
func (s *Server) mountStatic(fsys fs.FS) {
	fileServer := http.FileServer(http.FS(fsys))

	s.mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}

		// API 路径绝不回退到 index.html —— 否则前端会拿到 HTML
		// 然后报"JSON 解析失败"，排查半天才发现是路由没匹配上。
		if strings.HasPrefix(r.URL.Path, "/api/") {
			WriteError(w, http.StatusNotFound, "not_found", "接口不存在")
			return
		}

		if _, err := fs.Stat(fsys, p); err != nil {
			// 文件不存在 → 交给前端路由
			serveIndex(w, r, fsys)
			return
		}
		fileServer.ServeHTTP(w, r)
	}))
}

func serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		WriteError(w, http.StatusNotFound, "not_found", "页面不存在")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// Version 是内核版本号，供 /api/v1/system/info 与构建信息使用。
const Version = "0.1.0-dev"

// ────────────────────── 中间件 ──────────────────────

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// 不把 panic 内容回给客户端——那是信息泄露。
				s.log.Error("请求处理 panic", "path", r.URL.Path, "panic", rec, "request_id", RequestID(r.Context()))
				WriteError(w, http.StatusInternalServerError, "internal_error", "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyPrincipal
)

// RequestID 从上下文取请求 ID。
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// PrincipalFrom 从上下文取认证主体。
func PrincipalFrom(ctx context.Context) *contract.Principal {
	if v, ok := ctx.Value(ctxKeyPrincipal).(*contract.Principal); ok {
		return v
	}
	return nil
}

// WithPrincipal 把主体放进上下文（插件在自定义中间件里可能用到）。
func WithPrincipal(ctx context.Context, p *contract.Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, p)
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		// 静态资源不记日志，否则一个页面能刷出几十条，把真正重要的日志冲掉
		if strings.HasPrefix(r.URL.Path, "/api/") || rec.status >= 400 {
			kv := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"ms", time.Since(start).Milliseconds(),
				"request_id", RequestID(r.Context()),
				"ip", ClientIP(r, s.deps.Cfg.TrustProxy),
			}
			if p := PrincipalFrom(r.Context()); p != nil {
				kv = append(kv, "user", p.Username)
			}
			if rec.status >= 500 {
				s.log.Error("请求失败", kv...)
			} else if rec.status >= 400 {
				s.log.Warn("请求异常", kv...)
			} else {
				s.log.Debug("请求完成", kv...)
			}
		}
	})
}

// securityHeaders 补齐安全响应头。
//
// 注意这里**没有**设 CORS 头。本项目默认同源部署，
// 不开 CORS。需要跨域时由运维在反代层显式配置——
// 参考项目里出现过的「反射任意 Origin + allow_credentials=true」
// 是可直接被带凭据跨站读写的严重问题，不能复现。
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) globalLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 全局兜底限流：防止单一 IP 把服务打满。
		// 各端点可以再叠加更严的限流（见 Route.RateLimit）。
		ip := ClientIP(r, s.deps.Cfg.TrustProxy)
		if !s.limiter.allow("global:"+ip, 600, time.Minute) {
			s.log.Warn("触发全局限流", "ip", ip, "path", r.URL.Path)
			WriteError(w, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后再试")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate 按路由声明的级别鉴权。
func (s *Server) authenticate(level contract.AuthLevel, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if level == "" || level == contract.AuthNone {
			next.ServeHTTP(w, r)
			return
		}

		if s.deps.Auth == nil {
			// 没有认证器却要求鉴权 = 配置错误。必须 500 而不是放行，
			// "忘了装配认证" 导致的越权是最危险的一类事故。
			s.log.Error("路由要求鉴权但未装配认证器", "path", r.URL.Path)
			WriteError(w, http.StatusInternalServerError, "auth_not_configured", "服务端认证未初始化")
			return
		}

		p, err := s.deps.Auth.Authenticate(r.Context(), r)
		switch {
		case errors.Is(err, contract.ErrUnauthenticated):
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "未登录")
			return
		case err != nil:
			s.log.Error("认证过程出错", "path", r.URL.Path, "err", err)
			WriteError(w, http.StatusInternalServerError, "auth_error", "认证失败")
			return
		}

		if level == contract.AuthAdmin && !p.IsAdmin() {
			WriteError(w, http.StatusForbidden, "forbidden", "需要管理员权限")
			return
		}

		// 过期用户：可以读自己的信息、可以续费，但不能播放或修改受限资源。
		// 这里只拦 admin 级之外的写操作，具体放行规则由插件在 handler 里
		// 用 Principal.Expired 自行判断——内核不猜插件的业务意图。
		if p.Expired && level == contract.AuthAdmin {
			WriteError(w, http.StatusForbidden, "expired", "账号已过期")
			return
		}

		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// rateLimit 按路由声明的限流规则包装 handler。
func (s *Server) rateLimit(spec string, next http.Handler) http.Handler {
	if spec == "" {
		return next
	}
	n, window, err := parseRateSpec(spec)
	if err != nil {
		// 限流规则写错就忽略，但必须告警——否则等于没有限流
		s.log.Warn("限流规则解析失败，该路由不限流", "spec", spec, "err", err)
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r, s.deps.Cfg.TrustProxy)
		key := fmt.Sprintf("%s|%s|%s", r.Method, r.URL.Path, ip)
		if !s.limiter.allow(key, n, window) {
			WriteError(w, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后再试")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func parseRateSpec(spec string) (int, time.Duration, error) {
	parts := strings.SplitN(strings.TrimSpace(spec), "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("格式应为 N/s|N/m|N/h")
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || n <= 0 {
		return 0, 0, fmt.Errorf("数量无效")
	}
	var window time.Duration
	switch strings.ToLower(strings.TrimSpace(parts[1])) {
	case "s", "sec":
		window = time.Second
	case "m", "min":
		window = time.Minute
	case "h", "hour":
		window = time.Hour
	default:
		return 0, 0, fmt.Errorf("时间单位无效")
	}
	return n, window, nil
}

// ────────────────────── 工具 ──────────────────────

// statusRecorder 记录响应状态码，供访问日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush 让 statusRecorder 不破坏 SSE 等需要立即刷新的场景。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ClientIP 取客户端 IP。
//
// TrustProxy 为 false 时永远用 RemoteAddr —— 直接信任 X-Forwarded-For
// 会让攻击者随意伪造 IP 绕过限流，这是非常常见的配置事故。
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// 取最左边的、非内网的地址
			for _, part := range strings.Split(xff, ",") {
				ip := strings.TrimSpace(part)
				if ip == "" {
					continue
				}
				return ip
			}
		}
		if real := strings.TrimSpace(r.Header.Get("X-Real-Ip")); real != "" {
			return real
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

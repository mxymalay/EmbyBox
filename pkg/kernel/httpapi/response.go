package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Response 是全站统一的响应信封。
//
// 约定：**所有 /api/ 下的响应都长这个样子**，包括错误。
// 前端只需写一次解析逻辑。
//
// HTTP 状态码与 code 字段同时表达结果：
//
//	HTTP 200 + code=200  成功
//	HTTP 401 + code=401  未登录
//	HTTP 400 + code=4000 业务校验失败（参数错误由 code 细分）
//
// 之所以同时给 HTTP 状态码，是为了让反代、监控、浏览器 DevTools
// 能直观看出成败，而不用解析 body。
type Response struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// 业务错误码。前 100 个对内核通用，1000+ 留给插件。
const (
	CodeOK           = 200
	CodeBadRequest   = 4000 // 参数错误
	CodeUnauthorized = 4010 // 未登录或凭证失效
	CodeForbidden    = 4030 // 权限不足
	CodeNotFound     = 4040 // 资源不存在
	CodeConflict     = 4090 // 状态冲突（如用户名已存在）
	CodeTooMany      = 4290 // 触发限流
	CodeInternal     = 5000 // 服务端错误
	CodeUnavailable  = 5030 // 依赖不可用
	CodeNotImpl      = 5010 // 功能未实现
)

// WriteJSON 写出成功响应。
func WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{
		Code:    codeFromStatus(status),
		Message: "success",
		Data:    data,
	})
}

// WriteEnvelope 写出带自定义 code 与 message 的响应。
func WriteEnvelope(w http.ResponseWriter, status, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{Code: code, Message: message, Data: data})
}

// WriteError 写出错误响应。
//
// kind 是给机器看的稳定标识（如 "invalid_credentials"），
// message 是给人看的中文提示。前端做国际化时对 kind 做映射，
// 而不是去匹配 message —— 后者一改文案就全坏。
func WriteError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{
		Code:    codeFromStatus(status),
		Message: message,
		Data:    map[string]any{"kind": kind},
	})
}

// WriteServerError 写 500，并且**只回笼统文案**。
// 真实错误写日志，不回客户端——错误详情是信息泄露的常见来源。
func WriteServerError(w http.ResponseWriter) {
	WriteError(w, http.StatusInternalServerError, "internal_error", "服务器内部错误，请查看服务端日志")
}

// codeFromStatus 把 HTTP 状态码映射到业务码。
func codeFromStatus(status int) int {
	switch status {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return CodeOK
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return CodeBadRequest
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusTooManyRequests:
		return CodeTooMany
	case http.StatusNotImplemented:
		return CodeNotImpl
	case http.StatusServiceUnavailable:
		return CodeUnavailable
	default:
		if status >= 500 {
			return CodeInternal
		}
		return CodeBadRequest
	}
}

// DecodeJSON 解析请求体。
//
// 两条硬规矩：
//  1. 限制体积。不限大小的 JSON body 是内存耗尽的入口。
//  2. 拒绝未知字段。配置类接口里把 enabled 拼成 enable，
//     如果被静默忽略，用户会以为设置生效了——这类 bug 极难排查。
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = 1 << 20 // 默认 1MB
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return err
	}
	// 确保 body 里没有第二个 JSON 对象
	if dec.More() {
		return errors.New("请求体包含多余的 JSON 内容")
	}
	return nil
}

// newRequestID 生成 16 位十六进制请求 ID。
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// MaskSecret 把敏感字符串打码，用于日志与后台展示。
//
// 只保留首尾各 2 字符：既能让管理员确认"是哪个 key"，
// 又不至于泄露完整值。长度不足时全部打码。
func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 6 {
		return strings.Repeat("*", len(s))
	}
	return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
}

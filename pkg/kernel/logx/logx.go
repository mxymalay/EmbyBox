// Package logx 是内核的结构化日志。
//
// 为什么不用标准库 log 或直接上 zap：
//
//	内核要保持零外部依赖，而"日志"是唯一一个每个模块都要用的东西。
//	定义一个小接口，需要高性能时在 main 里换成 zap 实现即可。
//
// 用法约定：所有日志必须带 kv 上下文，不要拼字符串。
//
//	log.Info("用户创建成功", "user_id", u.ID, "emby_id", u.EmbyID)
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Logger 是内核使用的最小日志接口。
type Logger interface {
	Debug(msg string, kv ...any)
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
	// With 返回带上了固定 kv 的派生 logger，用于给模块绑定身份。
	With(kv ...any) Logger
}

// slogLogger 是基于标准库 log/slog 的实现。
// 不引第三方依赖，同时支持 text/json 两种格式。
type slogLogger struct {
	l *slog.Logger
}

// Options 控制日志行为。
type Options struct {
	// Level 最低输出级别：debug / info / warn / error。
	Level string
	// Format 输出格式：text / json。
	Format string
	// Output 输出目标，nil 表示 stderr。
	Output io.Writer
	// AddSource 是否附带源码位置。
	AddSource bool
}

// New 按配置创建 logger。
func New(opt Options) Logger {
	out := opt.Output
	if out == nil {
		out = os.Stderr
	}

	level := parseLevel(opt.Level)

	hopt := &slog.HandlerOptions{Level: level, AddSource: opt.AddSource}

	var h slog.Handler
	if strings.EqualFold(opt.Format, "json") {
		h = slog.NewJSONHandler(out, hopt)
	} else {
		h = slog.NewTextHandler(out, hopt)
	}
	return &slogLogger{l: slog.New(h)}
}

// Default 返回默认配置的 logger（info 级别，text 格式）。
func Default() Logger { return New(Options{}) }

// Discard 返回一个丢弃全部日志的实现，用于测试。
func Discard() Logger {
	return &slogLogger{l: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func (s *slogLogger) Debug(msg string, kv ...any) { s.l.Debug(msg, kv...) }
func (s *slogLogger) Info(msg string, kv ...any)  { s.l.Info(msg, kv...) }
func (s *slogLogger) Warn(msg string, kv ...any)  { s.l.Warn(msg, kv...) }
func (s *slogLogger) Error(msg string, kv ...any) { s.l.Error(msg, kv...) }

func (s *slogLogger) With(kv ...any) Logger {
	return &slogLogger{l: s.l.With(kv...)}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ────────────────────────────────────────────────────────────
// 以下是给插件用的"重定向 std log"能力。
//
// 很多第三方库（包括我们自己要用的一些）会写标准库 log，
// 让它们混进 stderr 会破坏结构化输出，所以在这里统一截走。
// ────────────────────────────────────────────────────────────

var (
	redirectOnce sync.Once
	redirectLog  Logger
)

// RedirectStdLog 把标准库 log 的输出接到我们的 logger 上。
func RedirectStdLog(l Logger) {
	redirectOnce.Do(func() {
		redirectLog = l
		slog.SetDefault(slog.New(slog.NewTextHandler(&logWriter{l: l}, nil)))
	})
}

// logWriter 把标准库的整行输出转成一条 Info 日志。
type logWriter struct{ l Logger }

func (w *logWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if line != "" {
		w.l.Info(line, "source", "stdlog")
	}
	return len(p), nil
}

// Err 是给 "kv" 参数用的辅助：把 error 包成可读字段。
//
//	log.Warn("同步失败", "err", logx.Err(err))
func Err(err error) any {
	if err == nil {
		return nil
	}
	return fmt.Sprintf("%v", err)
}

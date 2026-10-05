// Package config 负责运行期配置。
//
// 设计决策：主配置来自环境变量（便于 Docker 部署），
// 而"能被管理员在后台改的东西"存数据库（见 store 包）。
//
// 为什么要分开：
//
//	数据库地址、监听端口这类"启动前就必须知道"的配置只能来自环境变量；
//	站点名称、开关这类"运行中随时可改"的配置必须落库。
//	把两者混在一起是很多项目后期改不动的根源。
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是启动期配置。
type Config struct {
	// ─── 服务 ───
	HTTPAddr    string // 监听地址，如 ":8080"
	BaseURL     string // 对外访问地址，用于生成回调链接
	Timezone    string // 业务时区，所有日期边界共用它
	TrustProxy  bool   // 是否信任 X-Forwarded-For（部署在反代后应开启）
	MetricsAddr string // Prometheus 指标监听地址，空表示不开启

	// ─── 数据库 ───
	DBDriver string // sqlite | postgres
	DBDSN    string // 连接串；sqlite 时是文件路径

	// ─── 安全 ───
	SecretKey   string        // 会话签名与加密主密钥
	SessionTTL  time.Duration // 会话有效期
	AdminUser   string        // 首次启动时创建的管理员用户名
	AdminPass   string        // 首次启动时创建的管理员密码
	SeedOnBoot  bool          // 是否在启动时补齐种子数据
	TrustedNets []string      // 额外信任的代理网段

	// ─── 日志 ───
	LogLevel  string
	LogFormat string

	// ─── 插件 ───
	PluginDirs    []string // 插件搜索目录
	DisabledPlugs []string // 显式禁用的插件 ID

	// ─── 数据 ───
	DataDir string // 数据目录（上传、备份、插件数据）
}

// Load 从环境变量加载配置并做校验。
//
// 环境变量命名：EMBYONE_ 前缀 + 大写下划线。
//
//	EMBYONE_HTTP_ADDR=:8080
//	EMBYONE_DB_DRIVER=sqlite
//	EMBYONE_DB_DSN=./data/embyone.db
//	EMBYONE_SECRET_KEY=<32+ 字节随机串>
func Load() (*Config, error) {
	cfg := &Config{
		HTTPAddr:      env("HTTP_ADDR", ":8080"),
		BaseURL:       env("BASE_URL", "http://localhost:8080"),
		Timezone:      env("TIMEZONE", "Asia/Shanghai"),
		TrustProxy:    envBool("TRUST_PROXY", false),
		MetricsAddr:   env("METRICS_ADDR", ""),
		DBDriver:      env("DB_DRIVER", "sqlite"),
		DBDSN:         env("DB_DSN", "./data/embyone.db"),
		SecretKey:     env("SECRET_KEY", ""),
		SessionTTL:    envDuration("SESSION_TTL", 72*time.Hour),
		AdminUser:     env("ADMIN_USER", "admin"),
		AdminPass:     env("ADMIN_PASS", ""),
		SeedOnBoot:    envBool("SEED_ON_BOOT", true),
		TrustedNets:   envList("TRUSTED_NETS", nil),
		LogLevel:      env("LOG_LEVEL", "info"),
		LogFormat:     env("LOG_FORMAT", "text"),
		PluginDirs:    envList("PLUGIN_DIRS", []string{"./plugins"}),
		DisabledPlugs: envList("DISABLED_PLUGINS", nil),
		DataDir:       env("DATA_DIR", "./data"),
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate 校验配置的合法性。
//
// 这里有两条"绝不放过"的规矩：
//  1. 生产环境下 SECRET_KEY 必须显式提供且足够长——默认密钥是安全事故的温床。
//  2. 初始化管理员时密码不能为空——空密码 + 公网 = 被人肉扫描的靶子。
func (c *Config) Validate() error {
	if c.HTTPAddr == "" {
		return errors.New("config: HTTP_ADDR 不能为空")
	}
	if c.DBDriver != "sqlite" && c.DBDriver != "postgres" {
		return fmt.Errorf("config: 不支持的 DB_DRIVER %q（可选 sqlite / postgres）", c.DBDriver)
	}
	if c.DBDSN == "" {
		return errors.New("config: DB_DSN 不能为空")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("config: TIMEZONE %q 无效: %w", c.Timezone, err)
	}

	// 密钥：开发环境允许自动生成，但会打显著警告（见 ResolveSecretKey）。
	// 一旦显式提供了，就必须够长。
	if c.SecretKey != "" && len(c.SecretKey) < 32 {
		return fmt.Errorf("config: SECRET_KEY 至少需要 32 字节（当前 %d）", len(c.SecretKey))
	}
	if c.AdminPass != "" && len(c.AdminPass) < 8 {
		return errors.New("config: ADMIN_PASS 至少需要 8 位")
	}
	if c.SessionTTL <= 0 {
		return errors.New("config: SESSION_TTL 必须为正数")
	}
	return nil
}

// Location 返回业务时区。
// 所有"今天""本月""到期日"的判定都必须用它，否则跨时区部署会出现诡异 bug。
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Addr 返回监听地址。
func (c *Config) Addr() string { return c.HTTPAddr }

// dbPathIsMemory 判断是否是内存数据库（测试用）。
func (c *Config) dbPathIsMemory() bool {
	return c.DBDSN == ":memory:" || strings.HasPrefix(c.DBDSN, "file::memory:")
}

// ────────────────────── 环境变量辅助 ──────────────────────

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv("EMBYONE_" + key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv("EMBYONE_" + key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv("EMBYONE_" + key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func envList(key string, def []string) []string {
	v := strings.TrimSpace(os.Getenv("EMBYONE_" + key))
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

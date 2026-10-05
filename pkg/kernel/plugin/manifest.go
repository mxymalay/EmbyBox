// Package plugin 负责插件的描述、发现、加载与权限校验。
//
// 四级插件模型的落点：
//
//	L0 声明式  plugin.yaml                    —— 本包的 Manifest 直接解析
//	L1 脚本    plugin.yaml + *.js             —— Manifest 里的 hooks.handler 指向脚本
//	L2 原生    Go 代码，编译进主程序           —— 通过 Builtin() 注册，清单是元信息
//	L3 外部    独立进程 + JSON-RPC            —— M7 阶段实现，本包预留 process 字段
//
// M0/M1 阶段实现 L0 + L2；L1 在 M6 接入脚本引擎；L3 在 M7。
package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest 是 plugin.yaml 的映射。
type Manifest struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`

	// Dir 是插件所在目录，加载时由 loader 填入，不来自 YAML。
	Dir string `yaml:"-"`
	// Builtin 标记这是编译进主程序的内置插件。
	Builtin bool `yaml:"-"`
}

// Metadata 是插件的身份信息。
type Metadata struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Author      string   `yaml:"author"`
	License     string   `yaml:"license"`
	Homepage    string   `yaml:"homepage"`
	Description string   `yaml:"description"`
	Icon        string   `yaml:"icon"`
	Tags        []string `yaml:"tags"`
}

// Spec 是插件的行为声明。
type Spec struct {
	Requires     []Requirement `yaml:"requires"`
	Permissions  []string      `yaml:"permissions"`
	AllowedHosts []string      `yaml:"allowed_hosts"`
	Config       []ConfigField `yaml:"config"`
	Hooks        []Hook        `yaml:"hooks"`
	Routes       []Route       `yaml:"routes"`
	Jobs         []Job         `yaml:"jobs"`
	Schema       *SchemaSpec   `yaml:"schema"`
	Execution    Execution     `yaml:"execution"`
}

// Requirement 是插件依赖。
type Requirement struct {
	ID      string `yaml:"id" json:"id"`
	Version string `yaml:"version" json:"version"`
}

// ConfigField 描述一个配置项，前端据此自动渲染表单。
type ConfigField struct {
	Key         string         `yaml:"key" json:"key"`
	Type        string         `yaml:"type" json:"type"` // string|secret|number|bool|enum|duration|multiselect|object|list
	Label       string         `yaml:"label" json:"label"`
	Description string         `yaml:"description" json:"description"`
	Required    bool           `yaml:"required" json:"required"`
	Default     any            `yaml:"default" json:"default"`
	Options     []ConfigOption `yaml:"options" json:"options,omitempty"`
	Fields      []ConfigField  `yaml:"fields" json:"fields,omitempty"` // type=object 时的子字段
	Item        *ConfigField   `yaml:"item" json:"item,omitempty"`     // type=list 时的元素定义
}

// ConfigOption 是 enum / multiselect 的取值。
type ConfigOption struct {
	Value string `yaml:"value" json:"value"`
	Label string `yaml:"label" json:"label"`
}

// Hook 是事件订阅声明。
type Hook struct {
	Event    string `yaml:"event" json:"event"`
	Handler  string `yaml:"handler" json:"handler,omitempty"`
	Template string `yaml:"template" json:"template,omitempty"`
	Priority int    `yaml:"priority" json:"priority"`
	Timeout  string `yaml:"timeout" json:"timeout,omitempty"`
	OnError  string `yaml:"onError" json:"on_error,omitempty"` // continue | abort
}

// Route 是 HTTP 路由声明。
type Route struct {
	Path      string   `yaml:"path" json:"path"`
	Methods   []string `yaml:"methods" json:"methods"`
	Handler   string   `yaml:"handler" json:"handler"`
	Auth      string   `yaml:"auth" json:"auth"` // none|user|admin|apikey
	RateLimit string   `yaml:"rateLimit" json:"rate_limit,omitempty"`
}

// Job 是定时任务声明。
type Job struct {
	ID        string `yaml:"id" json:"id"`
	Schedule  string `yaml:"schedule" json:"schedule"`
	Handler   string `yaml:"handler" json:"handler"`
	Timeout   string `yaml:"timeout" json:"timeout,omitempty"`
	Singleton bool   `yaml:"singleton" json:"singleton"`
}

// SchemaSpec 是声明式表结构（L0/L1 用）。
type SchemaSpec struct {
	Version int         `yaml:"version" json:"version"`
	Tables  []TableSpec `yaml:"tables" json:"tables"`
}

// TableSpec 是一张表。
type TableSpec struct {
	Name    string       `yaml:"name" json:"name"`
	Columns []ColumnSpec `yaml:"columns" json:"columns"`
	Indexes []IndexSpec  `yaml:"indexes" json:"indexes"`
}

// ColumnSpec 是一列。
type ColumnSpec struct {
	Name    string `yaml:"name" json:"name"`
	Type    string `yaml:"type" json:"type"` // text|integer|real|bool|datetime|json
	Primary bool   `yaml:"primary" json:"primary"`
	Unique  bool   `yaml:"unique" json:"unique"`
	NotNull bool   `yaml:"notNull" json:"not_null"`
	Default any    `yaml:"default" json:"default"`
}

// IndexSpec 是一个索引。
type IndexSpec struct {
	Columns []string `yaml:"columns" json:"columns"`
	Unique  bool     `yaml:"unique" json:"unique"`
}

// Execution 声明执行约束。
type Execution struct {
	// Mode single-instance 时内核会在启动期获取数据库锁，
	// 拿不到就拒绝启动——而不是让它在运行时变成偶发的重复扣费。
	Mode string `yaml:"mode" json:"mode"`
	// Lock db-advisory | none
	Lock string `yaml:"lock" json:"lock"`
}

// idPattern 约束插件 ID 的字符集。
// 限制得这么死是为了让 ID 能安全地拼接进表名（plugin_<id>__）。
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// LoadManifest 从目录读取 plugin.yaml。
func LoadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, "plugin.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		// 兼容 .yml 后缀
		path = filepath.Join(dir, "plugin.yml")
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("plugin: 读取 %s 失败: %w", path, err)
		}
	}

	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // 未知字段直接报错：拼错的配置项必须被发现，而不是静默忽略
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("plugin: 解析 %s 失败: %w", path, err)
	}

	m.Dir = dir
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("plugin %s: %w", m.Metadata.ID, err)
	}
	return &m, nil
}

// Validate 校验清单的合法性与自洽性。
func (m *Manifest) Validate() error {
	if m.APIVersion == "" {
		return errors.New("清单缺少 apiVersion")
	}
	if m.APIVersion != "embybox.io/v1" {
		return fmt.Errorf("不支持的 apiVersion %q（当前仅支持 embybox.io/v1）", m.APIVersion)
	}
	if m.Kind != "Plugin" {
		return fmt.Errorf("kind 必须为 Plugin，得到 %q", m.Kind)
	}
	if m.Metadata.ID == "" {
		return errors.New("清单缺少 metadata.id")
	}
	if !idPattern.MatchString(m.Metadata.ID) {
		return fmt.Errorf("metadata.id %q 不合法：必须是小写字母开头、只含小写字母数字与连字符、长度 2-63",
			m.Metadata.ID)
	}
	if m.Metadata.Name == "" {
		return errors.New("清单缺少 metadata.name")
	}
	if m.Metadata.Version == "" {
		return errors.New("清单缺少 metadata.version")
	}

	for i, h := range m.Spec.Hooks {
		if h.Event == "" {
			return fmt.Errorf("hooks[%d] 缺少 event", i)
		}
		if h.Handler == "" && h.Template == "" {
			return fmt.Errorf("hooks[%d] 必须提供 handler 或 template", i)
		}
		if h.OnError != "" && h.OnError != "continue" && h.OnError != "abort" {
			return fmt.Errorf("hooks[%d].onError 只能是 continue 或 abort，得到 %q", i, h.OnError)
		}
	}

	for i, r := range m.Spec.Routes {
		if r.Path == "" {
			return fmt.Errorf("routes[%d] 缺少 path", i)
		}
		if !strings.HasPrefix(r.Path, "/") {
			return fmt.Errorf("routes[%d].path 必须以 / 开头", i)
		}
		switch r.Auth {
		case "", "none", "user", "admin", "apikey":
		default:
			return fmt.Errorf("routes[%d].auth %q 不合法", i, r.Auth)
		}
	}

	for i, j := range m.Spec.Jobs {
		if j.ID == "" {
			return fmt.Errorf("jobs[%d] 缺少 id", i)
		}
		if j.Schedule == "" {
			return fmt.Errorf("jobs[%d] 缺少 schedule", i)
		}
	}

	for _, f := range m.Spec.Config {
		if err := validateConfigField(f, "config"); err != nil {
			return err
		}
	}

	if m.Spec.Execution.Mode != "" &&
		m.Spec.Execution.Mode != "single-instance" &&
		m.Spec.Execution.Mode != "multi-instance" {
		return fmt.Errorf("execution.mode %q 不合法", m.Spec.Execution.Mode)
	}

	return nil
}

var validFieldTypes = map[string]bool{
	"string": true, "secret": true, "number": true, "bool": true,
	"enum": true, "duration": true, "multiselect": true, "object": true, "list": true,
}

func validateConfigField(f ConfigField, path string) error {
	if f.Key == "" {
		return fmt.Errorf("%s 中存在缺少 key 的配置项", path)
	}
	if !validFieldTypes[f.Type] {
		return fmt.Errorf("%s.%s 的类型 %q 不合法（可选 string/secret/number/bool/enum/duration/multiselect/object/list）",
			path, f.Key, f.Type)
	}
	if (f.Type == "enum" || f.Type == "multiselect") && len(f.Options) == 0 {
		return fmt.Errorf("%s.%s 类型为 %s 但没有提供 options", path, f.Key, f.Type)
	}
	for _, sub := range f.Fields {
		if err := validateConfigField(sub, path+"."+f.Key); err != nil {
			return err
		}
	}
	if f.Item != nil {
		if err := validateConfigField(*f.Item, path+"."+f.Key+"[]"); err != nil {
			return err
		}
	}
	return nil
}

// TablePrefix 返回本插件的数据表前缀。
//
// 强制命名空间：插件表一律叫 plugin_<id 中连字符转下划线>__<表名>。
// 好处有三：一目了然谁的表、卸载可精确清理、迁移互不干扰。
func (m *Manifest) TablePrefix() string {
	return "plugin_" + strings.ReplaceAll(m.Metadata.ID, "-", "_") + "__"
}

// Summary 返回一行摘要，用于启动日志与后台列表。
func (m *Manifest) Summary() string {
	kind := "L2-原生"
	if !m.Builtin {
		kind = "L0-声明式"
		for _, h := range m.Spec.Hooks {
			if strings.HasSuffix(h.Handler, ".js") {
				kind = "L1-脚本"
				break
			}
		}
	}
	return fmt.Sprintf("%s v%s (%s)", m.Metadata.Name, m.Metadata.Version, kind)
}

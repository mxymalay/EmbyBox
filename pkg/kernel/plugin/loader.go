package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mxymalay/embybox/pkg/kernel/logx"
)

// Discovery 是一次插件发现的结果。
type Discovery struct {
	// Builtins 编译进主程序的内置插件（L2）。
	Builtins []Plugin

	// Declarative 从目录里扫描到的声明式插件（L0/L1）。
	// M0/M1 阶段它们只做「加载 + 校验 + 在后台可见」，
	// 钩子执行要等 M6 接入脚本引擎。
	Declarative []*Manifest

	// Warnings 记录非致命问题（未知事件名、未知权限、版本不受支持等）。
	// 这些不影响启动，但必须让管理员看到——静默忽略配置错误
	// 是"改了没生效"类问题的最大来源。
	Warnings []string
}

// Discover 收集内置插件并扫描外部插件目录。
//
// disabled 里的 ID 会被跳过（大小写不敏感）。
func Discover(dirs []string, disabled []string, log logx.Logger) (*Discovery, error) {
	if log == nil {
		log = logx.Discard()
	}

	dis := &Discovery{Builtins: Builtins()}

	disabledSet := make(map[string]bool, len(disabled))
	for _, d := range disabled {
		disabledSet[strings.ToLower(strings.TrimSpace(d))] = true
	}

	// 内置插件也可能被禁用
	filtered := dis.Builtins[:0]
	for _, p := range dis.Builtins {
		id := strings.ToLower(p.Manifest().Metadata.ID)
		if disabledSet[id] {
			log.Info("内置插件已被配置禁用", "plugin", id)
			continue
		}
		filtered = append(filtered, p)
	}
	dis.Builtins = filtered

	// 扫描外部目录
	seen := make(map[string]string) // id -> dir，用于检测重复
	for _, p := range dis.Builtins {
		seen[p.Manifest().Metadata.ID] = "(builtin)"
	}

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				// 目录不存在不是错误：很多部署根本没有第三方插件
				log.Debug("插件目录不存在，跳过", "dir", dir)
				continue
			}
			return nil, fmt.Errorf("plugin: 读取插件目录 %s 失败: %w", dir, err)
		}

		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			sub := filepath.Join(dir, e.Name())

			m, err := LoadManifest(sub)
			if err != nil {
				// 单个插件坏了不该拖垮整个系统启动，
				// 但要明确告警到日志里，并由 /api/admin/plugins 暴露出来
				dis.Warnings = append(dis.Warnings,
					fmt.Sprintf("插件目录 %s 加载失败：%v", sub, err))
				log.Warn("插件加载失败，已跳过", "dir", sub, "err", err)
				continue
			}

			if disabledSet[strings.ToLower(m.Metadata.ID)] {
				log.Info("外部插件已被配置禁用", "plugin", m.Metadata.ID)
				continue
			}

			if prev, dup := seen[m.Metadata.ID]; dup {
				dis.Warnings = append(dis.Warnings,
					fmt.Sprintf("插件 ID %q 重复：%s 与 %s，后者被跳过", m.Metadata.ID, prev, sub))
				log.Warn("插件 ID 重复，已跳过后者", "plugin", m.Metadata.ID, "kept", prev, "skipped", sub)
				continue
			}
			seen[m.Metadata.ID] = sub

			dis.Declarative = append(dis.Declarative, m)
			log.Info("发现声明式插件", "plugin", m.Metadata.ID, "summary", m.Summary(), "dir", sub)
		}
	}

	// 静态检查：未知事件、未知权限
	for _, m := range dis.Declarative {
		if unknown := ValidateDeclaredPermissions(m); len(unknown) > 0 {
			dis.Warnings = append(dis.Warnings, fmt.Sprintf(
				"插件 %s 声明了内核不认识的权限：%s（可能是拼写错误）",
				m.Metadata.ID, strings.Join(unknown, ", ")))
		}
	}

	// 内置插件同样检查
	for _, p := range dis.Builtins {
		m := p.Manifest()
		if unknown := ValidateDeclaredPermissions(m); len(unknown) > 0 {
			dis.Warnings = append(dis.Warnings, fmt.Sprintf(
				"内置插件 %s 声明了内核不认识的权限：%s",
				m.Metadata.ID, strings.Join(unknown, ", ")))
		}
	}

	sort.Slice(dis.Declarative, func(i, j int) bool {
		return dis.Declarative[i].Metadata.ID < dis.Declarative[j].Metadata.ID
	})

	return dis, nil
}

// ResolveOrder 按 requires 做拓扑排序，返回插件 ID 的执行顺序。
//
// 规则：
//   - 依赖在前，被依赖者在后。
//   - 同层按 ID 字典序，保证每次启动顺序一致（便于对比日志）。
//   - 缺依赖 → 报错。**不做静默降级**：缺依赖的插件跑起来只会产生
//     更难排查的运行时错误。
//   - 环依赖 → 报错，并在错误里打印环上的节点。
func ResolveOrder(manifests []*Manifest) ([]string, error) {
	byID := make(map[string]*Manifest, len(manifests))
	for _, m := range manifests {
		byID[m.Metadata.ID] = m
	}

	state := make(map[string]int) // 0=未访问 1=访问中 2=已完成
	var order []string
	var stack []string

	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case 2:
			return nil
		case 1:
			// 找到环，把环上的路径打印出来
			cycle := append([]string{}, stack...)
			cycle = append(cycle, id)
			return fmt.Errorf("plugin: 检测到循环依赖：%s", strings.Join(cycle, " -> "))
		}

		m, ok := byID[id]
		if !ok {
			return fmt.Errorf("plugin: 依赖的插件 %q 不存在", id)
		}

		state[id] = 1
		stack = append(stack, id)

		// 依赖按 ID 排序，保证顺序稳定
		deps := make([]string, 0, len(m.Spec.Requires))
		for _, r := range m.Spec.Requires {
			deps = append(deps, r.ID)
		}
		sort.Strings(deps)

		for _, dep := range deps {
			if err := visit(dep); err != nil {
				return err
			}
		}

		stack = stack[:len(stack)-1]
		state[id] = 2
		order = append(order, id)
		return nil
	}

	ids := make([]string, 0, len(manifests))
	for _, m := range manifests {
		ids = append(ids, m.Metadata.ID)
	}
	sort.Strings(ids)

	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// CheckVersionRequirement 判断实际版本是否满足声明的要求。
//
// 支持的写法：">=1.0.0"、"1.2.0"（精确）、""（任意）。
// 刻意不支持复杂范围——插件依赖不是包管理器，简单可预期更重要。
func CheckVersionRequirement(actual, requirement string) bool {
	requirement = strings.TrimSpace(requirement)
	if requirement == "" {
		return true
	}
	if strings.HasPrefix(requirement, ">=") {
		return compareSemver(actual, strings.TrimSpace(requirement[2:])) >= 0
	}
	return compareSemver(actual, requirement) == 0
}

// compareSemver 比较语义化版本，返回 -1 / 0 / 1。
// 解析失败的段按 0 处理，绝不 panic——版本号是外部输入。
func compareSemver(a, b string) int {
	pa := parseSemver(a)
	pb := parseSemver(b)
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

func parseSemver(s string) [3]int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		n := 0
		for _, r := range parts[i] {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}

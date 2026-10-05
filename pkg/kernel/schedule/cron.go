package schedule

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// NextRun 计算给定调度表达式在当前时间之后的下一次执行时刻。
//
// 支持的写法：
//
//	@every 30s / @every 5m / @every 1h30m   固定间隔
//	@hourly @daily @weekly @monthly         别名
//	分 时 日 月 周                            5 字段 cron
//
// 字段内支持：*、数字、*/n、a-b、a-b/n、a,b,c
//
// 实现选择"逐分钟向前试探"而不是"按字段递推"：
// 后者快但边界情况（月末、闰年、周与日同时限定）极易写错，
// 而调度表达式算错一天比慢 5 毫秒严重得多。
// 上限两年，超出则报错——那说明表达式本身不可满足。
func NextRun(spec string, from time.Time) (time.Time, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return time.Time{}, fmt.Errorf("调度表达式为空")
	}

	if strings.HasPrefix(spec, "@") {
		return nextRunAlias(spec, from)
	}

	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return time.Time{}, fmt.Errorf("cron 表达式需要 5 个字段（分 时 日 月 周），得到 %d 个", len(fields))
	}

	minute, err := parseField(fields[0], 0, 59)
	if err != nil {
		return time.Time{}, fmt.Errorf("分钟字段: %w", err)
	}
	hour, err := parseField(fields[1], 0, 23)
	if err != nil {
		return time.Time{}, fmt.Errorf("小时字段: %w", err)
	}
	dom, err := parseField(fields[2], 1, 31)
	if err != nil {
		return time.Time{}, fmt.Errorf("日期字段: %w", err)
	}
	month, err := parseField(fields[3], 1, 12)
	if err != nil {
		return time.Time{}, fmt.Errorf("月份字段: %w", err)
	}
	// cron 里 0 和 7 都表示周日，统一归一到 0
	dow, err := parseField(fields[4], 0, 7)
	if err != nil {
		return time.Time{}, fmt.Errorf("星期字段: %w", err)
	}
	if dow.has(7) {
		dow.set(0)
	}

	// 从下一分钟整点开始找，避免"刚好卡在当前这一分钟"导致立刻重复触发
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(2, 0, 0)

	for t.Before(limit) {
		if matchTime(t, minute, hour, dom, month, dow) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("表达式 %q 在两年内无可执行时间", spec)
}

func nextRunAlias(spec string, from time.Time) (time.Time, error) {
	if strings.HasPrefix(spec, "@every ") {
		raw := strings.TrimSpace(strings.TrimPrefix(spec, "@every "))
		// 允许 "1h30m" 这种复合写法
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("@every 的间隔 %q 无效（示例：30s / 5m / 1h30m）", raw)
		}
		return from.Add(d).Truncate(time.Second), nil
	}

	switch spec {
	case "@hourly":
		return from.Truncate(time.Hour).Add(time.Hour), nil
	case "@daily", "@midnight":
		y, m, d := from.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, from.Location()).AddDate(0, 0, 1), nil
	case "@weekly":
		y, m, d := from.Date()
		base := time.Date(y, m, d, 0, 0, 0, 0, from.Location())
		// 归到下一个周一
		delta := (8 - int(base.Weekday())) % 7
		if delta == 0 {
			delta = 7
		}
		return base.AddDate(0, 0, delta), nil
	case "@monthly":
		y, m, _ := from.Date()
		return time.Date(y, m, 1, 0, 0, 0, 0, from.Location()).AddDate(0, 1, 0), nil
	default:
		return time.Time{}, fmt.Errorf("不支持的别名 %q（可用：@hourly @daily @weekly @monthly @every <duration>）", spec)
	}
}

func matchTime(t time.Time, minute, hour, dom, month, dow fieldSet) bool {
	return minute.has(t.Minute()) &&
		hour.has(t.Hour()) &&
		dom.has(t.Day()) &&
		month.has(int(t.Month())) &&
		dow.has(int(t.Weekday()))
}

// fieldSet 是一个 cron 字段的取值集合。
// 用 [61]bool 而不是 map，因为这个操作在试探循环里会被调用上百万次。
type fieldSet struct {
	values [61]bool
	wild   bool
}

func (f fieldSet) has(v int) bool {
	if v < 0 || v > 60 {
		return false
	}
	return f.values[v]
}

func (f *fieldSet) set(v int) {
	if v >= 0 && v <= 60 {
		f.values[v] = true
	}
}

// parseField 解析单个 cron 字段。
func parseField(s string, min, max int) (fieldSet, error) {
	var fs fieldSet
	s = strings.TrimSpace(s)
	if s == "" {
		return fs, fmt.Errorf("字段为空")
	}
	if s == "*" {
		fs.wild = true
		for i := min; i <= max; i++ {
			fs.set(i)
		}
		return fs, nil
	}

	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return fs, fmt.Errorf("存在空的取值")
		}

		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			stepStr := strings.TrimSpace(part[i+1:])
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return fs, fmt.Errorf("步长 %q 无效", stepStr)
			}
			step = n
			part = strings.TrimSpace(part[:i])
		}

		var lo, hi int
		switch {
		case part == "*":
			lo, hi = min, max
		case strings.Contains(part, "-"):
			bounds := strings.SplitN(part, "-", 2)
			a, err1 := strconv.Atoi(strings.TrimSpace(bounds[0]))
			b, err2 := strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err1 != nil || err2 != nil {
				return fs, fmt.Errorf("范围 %q 无效", part)
			}
			if a > b {
				return fs, fmt.Errorf("范围 %q 的起点大于终点", part)
			}
			lo, hi = a, b
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return fs, fmt.Errorf("取值 %q 不是数字", part)
			}
			lo, hi = n, n
		}

		if lo < min || hi > max {
			return fs, fmt.Errorf("取值 %d-%d 超出允许范围 %d-%d", lo, hi, min, max)
		}

		for v := lo; v <= hi; v += step {
			fs.set(v)
		}
	}
	return fs, nil
}

func pid() int { return os.Getpid() }

// Describe 生成表达式的自然语言描述，供后台展示。
// 让管理员看到"每 5 分钟"而不是 "*​/5 * * * *"，能省下大量确认时间。
func Describe(spec string) string {
	spec = strings.TrimSpace(spec)
	switch spec {
	case "@hourly":
		return "每小时"
	case "@daily", "@midnight":
		return "每天 00:00"
	case "@weekly":
		return "每周一 00:00"
	case "@monthly":
		return "每月 1 日 00:00"
	}
	if strings.HasPrefix(spec, "@every ") {
		raw := strings.TrimSpace(strings.TrimPrefix(spec, "@every "))
		if d, err := time.ParseDuration(raw); err == nil {
			return "每 " + humanDuration(d)
		}
		return spec
	}
	return spec
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%d 小时", h)
		}
		return fmt.Sprintf("%d 小时 %d 分钟", h, m)
	}
}

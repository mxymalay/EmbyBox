// Package schedule 是内核的调度器。
//
// 形态选择：**数据库租约队列**，而不是进程内 cron。
//
// 判断依据来自竞品实测：一个项目用硬编码 cron 表达式，改调度必须重启；
// 另一个用数据库队列，支持运行期改调度、dry_run 演练、熔断与告警。
// 后者才是运维友好的形态，而且它天然支持多实例（同一任务只会被一个实例认领）。
//
// 本包只提供"何时执行"的骨架，"执行什么"由插件通过 JobSpec 注册。
package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/embyone/embyone/pkg/kernel/event"
	"github.com/embyone/embyone/pkg/kernel/logx"
	"github.com/embyone/embyone/pkg/kernel/store"
)

// JobSpec 是一个待注册的任务。
type JobSpec struct {
	// ID 全局唯一，建议形如 "<plugin-id>.<job-name>"。
	ID string
	// PluginID 归属插件，用于展示与权限判定。
	PluginID string
	// Name 人类可读的名称。
	Name string
	// Schedule 调度表达式，支持 5 字段 cron 与 @every 语法。
	Schedule string
	// Singleton 为 true 时同一时刻只允许一个实例执行（靠租约保证）。
	Singleton bool
	// Timeout 单次执行超时，0 表示用默认值。
	Timeout time.Duration
	// MaxRetries 失败重试次数，仅对可安全重放的任务设置。
	MaxRetries int
	// Handler 执行体。
	Handler func(ctx context.Context, jc JobContext) error
}

// JobContext 是执行期提供给 handler 的上下文。
type JobContext struct {
	// JobID 任务 ID。
	JobID string
	// DryRun 为 true 时 handler 必须只做检查、不产生副作用。
	//
	// ★ dry_run 是运维刚需：任何会删数据或发钱的任务，
	// 都应该允许管理员先空跑一次看会发生什么。
	DryRun bool
	// Log 已绑定 job 身份的 logger。
	Log logx.Logger
	// Params 运行期参数，来自 jobs.params。
	Params map[string]any
}

// Deps 是调度器的依赖。
type Deps struct {
	DB  *store.DB
	Bus *event.Bus
	Log logx.Logger
	// Timezone 业务时区。cron 的字段按此时区解释。
	Timezone *time.Location
	// ScanInterval 扫描间隔，0 表示 5 秒。
	ScanInterval time.Duration
	// WorkerCount 并发执行的任务数上限。
	WorkerCount int
}

// Scheduler 是调度器。
type Scheduler struct {
	deps Deps
	log  logx.Logger

	mu   sync.RWMutex
	spec map[string]JobSpec

	stopCh chan struct{}
	wg     sync.WaitGroup
	sem    chan struct{}
}

// New 创建调度器。
func New(d Deps) *Scheduler {
	if d.Log == nil {
		d.Log = logx.Discard()
	}
	if d.Timezone == nil {
		d.Timezone = time.UTC
	}
	if d.ScanInterval <= 0 {
		d.ScanInterval = 5 * time.Second
	}
	if d.WorkerCount <= 0 {
		d.WorkerCount = 4
	}

	return &Scheduler{
		deps:   d,
		log:    d.Log.With("component", "schedule"),
		spec:   make(map[string]JobSpec),
		stopCh: make(chan struct{}),
		sem:    make(chan struct{}, d.WorkerCount),
	}
}

// Register 注册一个任务。
//
// 同时完成两件事：内存登记（供本次运行使用）+ 落库（供后台展示与持久化调度状态）。
func (s *Scheduler) Register(spec JobSpec) error {
	if spec.ID == "" {
		return errors.New("schedule: 任务 ID 为空")
	}
	if spec.Handler == nil {
		return fmt.Errorf("schedule: 任务 %s 没有 handler", spec.ID)
	}

	// 先解析一次表达式，把配置错误挡在启动阶段。
	// 让错误表达式跑到运行期才失败，意味着站长只能从"任务没执行"里猜原因。
	next, err := NextRun(spec.Schedule, time.Now().In(s.deps.Timezone))
	if err != nil {
		return fmt.Errorf("schedule: 任务 %s 的调度表达式 %q 无效: %w", spec.ID, spec.Schedule, err)
	}

	s.mu.Lock()
	if _, dup := s.spec[spec.ID]; dup {
		s.mu.Unlock()
		return fmt.Errorf("schedule: 任务 ID %q 重复", spec.ID)
	}
	s.spec[spec.ID] = spec
	s.mu.Unlock()

	if spec.Timeout <= 0 {
		spec.Timeout = 5 * time.Minute
	}

	return s.deps.DB.InTx(context.Background(), func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339)

		singleton := 1
		if !spec.Singleton {
			singleton = 0
		}

		// 已存在则只更新元信息，**不重置 next_run_at** ——
		// 重启不应该导致任务被推迟执行。
		res, err := tx.ExecContext(context.Background(), s.deps.DB.Rebind(`
			UPDATE jobs SET plugin_id = ?, name = ?, schedule = ?,
			                singleton = ?, timeout_secs = ?, max_retries = ?, updated_at = ?
			WHERE id = ?`),
			spec.PluginID, spec.Name, spec.Schedule, singleton,
			int(spec.Timeout.Seconds()), spec.MaxRetries, now, spec.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}

		_, err = tx.ExecContext(context.Background(), s.deps.DB.Rebind(`
			INSERT INTO jobs (id, plugin_id, name, schedule, singleton, timeout_secs,
			                  max_retries, params, next_run_at, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, '{}', ?, 1, ?, ?)`),
			spec.ID, spec.PluginID, spec.Name, spec.Schedule, singleton,
			int(spec.Timeout.Seconds()), spec.MaxRetries,
			next.UTC().Format(time.RFC3339), now, now)
		return err
	})
}

// Start 启动调度循环。
func (s *Scheduler) Start(ctx context.Context) {
	s.log.Info("调度器启动", "任务数", s.TaskCount(), "扫描间隔", s.deps.ScanInterval.String())

	s.wg.Add(1)
	go s.loop(ctx)
}

// Stop 停止调度并等待在跑的任务收尾。
func (s *Scheduler) Stop(ctx context.Context) {
	close(s.stopCh)

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()

	select {
	case <-done:
		s.log.Info("调度器已停止")
	case <-ctx.Done():
		s.log.Warn("调度器停止超时，仍有任务在执行")
	case <-time.After(30 * time.Second):
		s.log.Warn("调度器停止超时（30s），强制退出")
	}
}

// TaskCount 返回已注册任务数。
func (s *Scheduler) TaskCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.spec)
}

// loop 是主扫描循环。
func (s *Scheduler) loop(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.deps.ScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scanOnce(ctx)
		}
	}
}

// scanOnce 找出到期的任务并派发。
func (s *Scheduler) scanOnce(ctx context.Context) {
	due, err := s.dueJobs(ctx, time.Now().UTC())
	if err != nil {
		s.log.Error("扫描到期任务失败", "err", err)
		return
	}

	for _, d := range due {
		s.mu.RLock()
		spec, ok := s.spec[d.JobID]
		s.mu.RUnlock()
		if !ok {
			// 库里有但内存没注册：通常是插件被禁用了。
			// 把 next_run_at 推远，避免它每次扫描都被选中。
			s.log.Warn("数据库中残留未注册的任务，已暂停", "job", d.JobID)
			_ = s.postpone(ctx, d.JobID, 24*time.Hour)
			continue
		}

		// 抢占租约。抢不到说明别的实例正在跑，跳过。
		claimed, err := s.claim(ctx, d.JobID)
		if err != nil {
			s.log.Error("抢占任务租约失败", "job", d.JobID, "err", err)
			continue
		}
		if !claimed {
			continue
		}

		s.wg.Add(1)
		go func(spec JobSpec, jobID string) {
			defer s.wg.Done()
			s.sem <- struct{}{}
			defer func() { <-s.sem }()
			s.run(ctx, spec, jobID)
		}(spec, d.JobID)
	}
}

// run 执行单个任务。
func (s *Scheduler) run(ctx context.Context, spec JobSpec, jobID string) {
	start := time.Now()

	// 每次执行用独立的超时上下文，避免任务卡死把 worker 占满
	jobCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	params, dryRun := s.loadParams(ctx, jobID)

	jc := JobContext{
		JobID:  jobID,
		DryRun: dryRun,
		Log:    s.log.With("job", jobID),
		Params: params,
	}

	runID := s.beginRun(ctx, jobID, dryRun)
	if dryRun {
		jc.Log.Info("以 dry-run 模式执行（不产生副作用）")
	}

	err := s.execute(jobCtx, spec, jc)
	duration := time.Since(start)

	s.finishRun(ctx, runID, jobID, duration, err)

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			s.log.Error("任务执行超时", "job", jobID, "timeout", spec.Timeout.String())
		} else {
			s.log.Error("任务执行失败", "job", jobID, "err", err, "耗时", duration.String())
		}
		return
	}
	s.log.Info("任务执行完成", "job", jobID, "耗时", duration.String(), "dry_run", dryRun)
}

// execute 带一次 panic 保护地调用 handler。
//
// 单个任务 panic 不能杀掉整个调度器 —— 否则一个插件写错
// 就会让全站定时任务停摆。
func (s *Scheduler) execute(ctx context.Context, spec JobSpec, jc JobContext) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("任务 panic: %v", rec)
		}
	}()
	return spec.Handler(ctx, jc)
}

// ────────────────────── 数据库交互 ──────────────────────

type dueJob struct {
	JobID string
}

func (s *Scheduler) dueJobs(ctx context.Context, now time.Time) ([]dueJob, error) {
	rows, err := s.deps.DB.QueryContext(ctx, s.deps.DB.Rebind(`
		SELECT id FROM jobs
		WHERE enabled = 1
		  AND next_run_at IS NOT NULL
		  AND next_run_at <= ?
		  AND (lease_until IS NULL OR lease_until < ?)
		ORDER BY next_run_at
		LIMIT 50`),
		now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []dueJob
	for rows.Next() {
		var j dueJob
		if err := rows.Scan(&j.JobID); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// claim 用条件更新抢占租约。
//
// 关键在 WHERE 里的条件：只有租约已过期或无主时才更新。
// 这样多个实例并发扫描时，只有一个能抢到（RowsAffected==1）。
func (s *Scheduler) claim(ctx context.Context, jobID string) (bool, error) {
	now := time.Now().UTC()
	leaseUntil := now.Add(10 * time.Minute).Format(time.RFC3339)

	res, err := s.deps.DB.ExecContext(ctx, s.deps.DB.Rebind(`
		UPDATE jobs
		SET lease_owner = ?, lease_until = ?
		WHERE id = ?
		  AND (lease_until IS NULL OR lease_until < ?)`),
		ownerID(), leaseUntil, jobID, now.Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *Scheduler) loadParams(ctx context.Context, jobID string) (map[string]any, bool) {
	var raw string
	err := s.deps.DB.QueryRowContext(ctx,
		s.deps.DB.Rebind(`SELECT params FROM jobs WHERE id = ?`), jobID).Scan(&raw)
	if err != nil || raw == "" || raw == "{}" {
		return map[string]any{}, false
	}

	var params map[string]any
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		s.log.Warn("任务 params 解析失败，按空参数执行", "job", jobID, "err", err)
		return map[string]any{}, false
	}

	dryRun := false
	if v, ok := params["dry_run"].(bool); ok {
		dryRun = v
	}
	return params, dryRun
}

func (s *Scheduler) beginRun(ctx context.Context, jobID string, dryRun bool) int64 {
	now := time.Now().UTC()
	d := 0
	if dryRun {
		d = 1
	}

	res, err := s.deps.DB.ExecContext(ctx, s.deps.DB.Rebind(
		`INSERT INTO job_runs (job_id, started_at, status, dry_run) VALUES (?, ?, 'running', ?)`),
		jobID, now.Format(time.RFC3339), d)
	if err != nil {
		s.log.Warn("写入任务执行记录失败", "job", jobID, "err", err)
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

func (s *Scheduler) finishRun(ctx context.Context, runID int64, jobID string, dur time.Duration, runErr error) {
	now := time.Now().UTC()
	status := "success"
	errMsg := ""
	if runErr != nil {
		status = "failed"
		errMsg = runErr.Error()
	}

	if runID > 0 {
		_, _ = s.deps.DB.ExecContext(ctx, s.deps.DB.Rebind(
			`UPDATE job_runs SET finished_at = ?, status = ?, duration_ms = ?, error = ? WHERE id = ?`),
			now.Format(time.RFC3339), status, dur.Milliseconds(), errMsg, runID)
	}

	// 计算下次执行时间
	s.mu.RLock()
	spec, ok := s.spec[jobID]
	s.mu.RUnlock()

	next := now.Add(24 * time.Hour)
	if ok {
		if n, err := NextRun(spec.Schedule, now.In(s.deps.Timezone)); err == nil {
			next = n.UTC()
		}
	}

	failInc := 0
	if runErr != nil {
		failInc = 1
	}

	_, _ = s.deps.DB.ExecContext(ctx, s.deps.DB.Rebind(`
		UPDATE jobs
		SET next_run_at = ?, lease_owner = '', lease_until = NULL,
		    last_status = ?, last_error = ?, last_run_at = ?, last_duration_ms = ?,
		    run_count = run_count + 1, fail_count = fail_count + ?, updated_at = ?
		WHERE id = ?`),
		next.Format(time.RFC3339), status, truncate(errMsg, 500), now.Format(time.RFC3339),
		dur.Milliseconds(), failInc, now.Format(time.RFC3339), jobID)
}

func (s *Scheduler) postpone(ctx context.Context, jobID string, by time.Duration) error {
	next := time.Now().UTC().Add(by).Format(time.RFC3339)
	_, err := s.deps.DB.ExecContext(ctx, s.deps.DB.Rebind(
		`UPDATE jobs SET next_run_at = ?, lease_owner = '', lease_until = NULL WHERE id = ?`),
		next, jobID)
	return err
}

var (
	ownerOnce  sync.Once
	ownerValue string
)

func ownerID() string {
	ownerOnce.Do(func() {
		ownerValue = fmt.Sprintf("sched-%d-%d", time.Now().UnixNano()%100000, pid())
	})
	return ownerValue
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

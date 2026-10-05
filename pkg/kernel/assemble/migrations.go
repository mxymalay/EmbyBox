package assemble

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/embyone/embyone/pkg/kernel/store"
)

// KernelMigrations 返回内核自身的迁移。
//
// 版本号约定：**1-999 归内核，1000+ 归插件**。
// 这个边界必须守住，否则将来内核加表时会和已有插件撞号，
// 而撞号的后果是迁移表里已有记录 → 新迁移被静默跳过 → 表结构不对。
//
// 内核只建"横切关注点"的表，任何业务表（用户、订单、工单）
// 都属于某个插件，由插件自己声明迁移。
func KernelMigrations() []store.Migration {
	return []store.Migration{
		{
			Version: 1,
			Name:    "内核基础表：KV 与运行期设置",
			Owner:   "kernel",
			SQL: `
-- kv 是有命名空间的键值表。
-- namespace 由插件 ID 或内核子系统名填充，实现逻辑隔离。
CREATE TABLE IF NOT EXISTS kv (
    namespace  TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    expires_at TEXT,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (namespace, key)
);
CREATE INDEX IF NOT EXISTS idx_kv_expires ON kv (expires_at);
-- settings 是"管理员能在后台改"的运行期配置。
-- 与启动期配置（环境变量）严格分开：这里的值改了立即生效，不需要重启。
CREATE TABLE IF NOT EXISTS settings (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    value_type  TEXT NOT NULL DEFAULT 'string',
    description TEXT NOT NULL DEFAULT '',
    updated_by  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL
);
`,
		},
		{
			Version: 2,
			Name:    "审计日志",
			Owner:   "kernel",
			SQL: `
-- 审计日志是本项目的"问责基础"。
-- 参考项目里普遍缺失，导致多管理员场景下出了事查不出是谁干的。
CREATE TABLE IF NOT EXISTS audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_type TEXT NOT NULL DEFAULT 'system',
    actor_id   TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    ip         TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    ok         INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_actor   ON audit_log (actor_type, actor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_action  ON audit_log (action, created_at DESC);
`,
		},
		{
			Version: 3,
			Name:    "调度任务",
			Owner:   "kernel",
			SQL: `
-- 调度用数据库租约队列，而不是进程内 cron。
-- 原因：站长要能在后台改调度时间而不用重启，且多实例部署时
-- 同一个任务只能有一个实例在跑。
CREATE TABLE IF NOT EXISTS jobs (
    id               TEXT PRIMARY KEY,
    plugin_id        TEXT NOT NULL DEFAULT 'kernel',
    name             TEXT NOT NULL DEFAULT '',
    schedule         TEXT NOT NULL,
    singleton        INTEGER NOT NULL DEFAULT 1,
    enabled          INTEGER NOT NULL DEFAULT 1,
    timeout_secs     INTEGER NOT NULL DEFAULT 300,
    max_retries      INTEGER NOT NULL DEFAULT 0,
    params           TEXT NOT NULL DEFAULT '{}',
    next_run_at      TEXT,
    lease_owner      TEXT NOT NULL DEFAULT '',
    lease_until      TEXT,
    last_status      TEXT NOT NULL DEFAULT '',
    last_error       TEXT NOT NULL DEFAULT '',
    last_run_at      TEXT,
    last_duration_ms INTEGER NOT NULL DEFAULT 0,
    run_count        INTEGER NOT NULL DEFAULT 0,
    fail_count       INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_next ON jobs (enabled, next_run_at);
-- 每次执行的历史，用于排障与"这任务最近是不是一直失败"
CREATE TABLE IF NOT EXISTS job_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id      TEXT NOT NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    status      TEXT NOT NULL DEFAULT 'running',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    dry_run     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_job_runs_job ON job_runs (job_id, id DESC);
`,
		},
		{
			Version: 4,
			Name:    "插件单实例锁",
			Owner:   "kernel",
			SQL: `
-- 声明了 execution.mode=single-instance 的插件在启动时必须拿到锁。
--
-- 为什么要在启动期硬失败而不是运行期兜底：
-- 重复扣费、重复发码这类事故一旦发生就无法撤销，
-- 宁可第二个实例起不来，也不要它带着错误的假设跑起来。
--
-- expires_at 让进程被 kill 后锁能自动过期，不需要人工清理。
CREATE TABLE IF NOT EXISTS instance_locks (
    name        TEXT PRIMARY KEY,
    owner       TEXT NOT NULL,
    acquired_at TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);
`,
		},
	}
}

// instanceLockTTL 是实例锁的有效期。
//
// 给得比较长（15 分钟）是因为单实例插件通常不会频繁重启；
// 进程被 kill -9 之后，最坏情况下要等这么久才能启动新实例。
// 如果这个等待不可接受，把插件改成 multi-instance 并在业务层做幂等。
const instanceLockTTL = 15 * time.Minute

// acquireInstanceLock 尝试获取命名实例锁。
//
// 返回 (true, nil) 表示拿到或成功续期（同一进程重启）；
// 返回 (false, nil) 表示锁被**另一个**实例持有——这是正常结果，
// 不是错误，由调用方决定是拒绝启动还是降级为只读。
//
// 实现说明：用「先清理过期 → 再判定归属 → 插入或续期」三步。
// SQLite 没有 SELECT FOR UPDATE，但写连接被限制为单条
// （store.Open 里 MaxOpenConns=1），加上主键约束，足以保证互斥。
// 接入 PostgreSQL 后此处应改为 pg_advisory_lock。
func acquireInstanceLock(ctx context.Context, db *store.DB, name string) (bool, error) {
	now := time.Now().UTC()
	expires := now.Add(instanceLockTTL)
	owner := instanceOwner()

	held := false
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		// 1. 清理所有已过期的锁
		if _, err := tx.ExecContext(ctx,
			db.Rebind(`DELETE FROM instance_locks WHERE expires_at < ?`),
			now.Format(time.RFC3339)); err != nil {
			return fmt.Errorf("清理过期锁失败: %w", err)
		}

		// 2. 看当前归属
		var current string
		err := tx.QueryRowContext(ctx,
			db.Rebind(`SELECT owner FROM instance_locks WHERE name = ?`), name).Scan(&current)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			// 无锁，直接拿
			if _, err := tx.ExecContext(ctx, db.Rebind(
				`INSERT INTO instance_locks (name, owner, acquired_at, expires_at) VALUES (?, ?, ?, ?)`),
				name, owner, now.Format(time.RFC3339), expires.Format(time.RFC3339)); err != nil {
				return fmt.Errorf("写入实例锁失败: %w", err)
			}
			held = true
			return nil

		case err != nil:
			return fmt.Errorf("查询实例锁失败: %w", err)

		case current == owner:
			// 是自己（同进程重启后残留），续期即可
			if _, err := tx.ExecContext(ctx, db.Rebind(
				`UPDATE instance_locks SET expires_at = ? WHERE name = ?`),
				expires.Format(time.RFC3339), name); err != nil {
				return fmt.Errorf("续期实例锁失败: %w", err)
			}
			held = true
			return nil

		default:
			// 被别的实例持有。这不是错误，用 held=false 表达。
			held = false
			return nil
		}
	})
	if err != nil {
		return false, err
	}
	return held, nil
}

// releaseInstanceLock 释放锁（优雅停机时调用）。
func releaseInstanceLock(ctx context.Context, db *store.DB, name string) {
	if db == nil {
		return
	}
	_, _ = db.ExecContext(ctx, db.Rebind(
		`DELETE FROM instance_locks WHERE name = ? AND owner = ?`), name, instanceOwner())
}

var (
	instanceOwnerOnce  sync.Once
	instanceOwnerValue string
)

// instanceOwner 返回本进程的标识。
// 用 hostname + pid，排障时能一眼看出是哪个进程占着锁。
func instanceOwner() string {
	instanceOwnerOnce.Do(func() {
		h, err := os.Hostname()
		if err != nil || h == "" {
			h = "unknown"
		}
		instanceOwnerValue = fmt.Sprintf("%s#%d", h, os.Getpid())
	})
	return instanceOwnerValue
}

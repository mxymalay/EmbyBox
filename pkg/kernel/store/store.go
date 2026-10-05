// Package store 提供存储抽象与版本化迁移。
//
// 设计决策：
//
//  1. 不引 ORM。表结构由 SQL 迁移文件唯一定义，Go 侧只写显式 SQL。
//     ORM 的隐式行为（自动建表、级联、懒加载）是可读性与可审计性的敌人。
//  2. SQLite 是默认形态（单机零配置），PostgreSQL 是可选形态（多实例）。
//     两边通过 Dialect 处理占位符与少量语法差异。
//  3. 迁移带序号、只前进、记录在 schema_migrations 表。
//     生产环境禁止"自动改表"，改结构必须写迁移文件。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无 CGO

	"github.com/mxymalay/embybox/pkg/kernel/logx"
)

// Dialect 是数据库方言。
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// DB 包装 *sql.DB，附带方言与日志。
type DB struct {
	*sql.DB

	dialect Dialect
	log     logx.Logger
}

// Options 控制连接行为。
type Options struct {
	// Driver sqlite | postgres
	Driver string
	// DSN 连接串；sqlite 时是文件路径
	DSN string
	// MaxOpenConns 最大连接数，0 表示用默认值
	MaxOpenConns int
	// Log logger
	Log logx.Logger
}

// Open 打开数据库连接。
func Open(ctx context.Context, opt Options) (*DB, error) {
	if opt.Log == nil {
		opt.Log = logx.Discard()
	}

	var dialect Dialect
	var driverName string

	switch opt.Driver {
	case "sqlite", "":
		dialect = DialectSQLite
		driverName = "sqlite"
		// SQLite 目录可能不存在，帮用户建好，否则会报 "unable to open database file"，
		// 这个报错对新手极不友好。
		if err := ensureDirForDSN(opt.DSN); err != nil {
			return nil, err
		}
	case "postgres":
		dialect = DialectPostgres
		driverName = "postgres"
		return nil, errors.New("store: postgres 驱动尚未接入，M0 阶段仅支持 sqlite；" +
			"接口已按方言抽象，接入时只需在 dialect.go 补齐差异")
	default:
		return nil, fmt.Errorf("store: 不支持的驱动 %q", opt.Driver)
	}

	sqlDB, err := sql.Open(driverName, opt.DSN)
	if err != nil {
		return nil, fmt.Errorf("store: 打开数据库失败: %w", err)
	}

	if dialect == DialectSQLite {
		// SQLite 的并发写是串行的，且默认不开启 WAL。
		// 不设这两个参数时，多 goroutine 写入会随机报 "database is locked"。
		if _, err := sqlDB.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("store: 开启 WAL 失败: %w", err)
		}
		if _, err := sqlDB.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("store: 开启外键约束失败: %w", err)
		}
		// busy_timeout 让并发写等待而不是立刻失败
		if _, err := sqlDB.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("store: 设置 busy_timeout 失败: %w", err)
		}
		// 单写多读：把最大连接数限制住，避免写锁竞争放大
		sqlDB.SetMaxOpenConns(1)
	} else if opt.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(opt.MaxOpenConns)
	}

	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("store: 数据库不可用: %w", err)
	}

	return &DB{DB: sqlDB, dialect: dialect, log: opt.Log}, nil
}

// Dialect 返回方言。
func (d *DB) Dialect() Dialect { return d.dialect }

// Log 返回绑定了 store 身份的 logger。
func (d *DB) Log() logx.Logger { return d.log }

// InTx 在一个事务里执行 fn。
//
// 用法约定：**所有多步骤的状态变更都必须走它**。
// "先查再改"不加事务在并发下必然出错，这是本项目最容易被忽略的坑。
//
// 返回 error 则回滚；panic 也会回滚后重新抛出。
func (d *DB) InTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			d.log.Warn("事务回滚失败", "err", rbErr)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交事务失败: %w", err)
	}
	return nil
}

// Rebind 把 SQL 里的 ? 占位符转成当前方言需要的写法。
// SQLite 用 ?，PostgreSQL 用 $1 $2...
func (d *DB) Rebind(query string) string {
	if d.dialect != DialectPostgres {
		return query
	}
	var sb strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			sb.WriteString(fmt.Sprintf("$%d", n))
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func ensureDirForDSN(dsn string) error {
	if dsn == "" || dsn == ":memory:" || strings.HasPrefix(dsn, "file::memory:") {
		return nil
	}
	if strings.HasPrefix(dsn, "file:") {
		dsn = strings.TrimPrefix(dsn, "file:")
		if i := strings.Index(dsn, "?"); i >= 0 {
			dsn = dsn[:i]
		}
	}
	dir := filepath.Dir(dsn)
	if dir == "." || dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: 创建数据目录 %s 失败: %w", dir, err)
	}
	return nil
}

// ───────────────────────── 迁移 ─────────────────────────

// Migration 是一个迁移单元。
//
//	Version  序号，必须全局唯一且单调递增。多个插件的迁移共享同一序号空间，
//	         所以内核占用 1-999，插件从 1000 起按注册顺序分配。
//	Name     描述，出现在日志和审计里。
//	SQL      语句文本。SQLite 不支持事务内 DDL，故每条语句独立执行。
type Migration struct {
	Version int
	Name    string
	SQL     string
	// Owner 迁移来源（"kernel" 或插件 ID），用于排障。
	Owner string
}

// Migrate 执行全部未执行的迁移。
//
// 幂等：已执行的版本会被跳过。可以安全地在每次启动时调用。
func (d *DB) Migrate(ctx context.Context, ms []Migration) error {
	if err := d.ensureMigrationTable(ctx); err != nil {
		return err
	}

	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return err
	}

	sorted := make([]Migration, len(ms))
	copy(sorted, ms)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })

	// 同一版本号出现两次一定是开发事故，早失败比晚失败好
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Version == sorted[i-1].Version {
			return fmt.Errorf("store: 迁移版本号重复: %d（%s 与 %s）",
				sorted[i].Version, sorted[i-1].Owner, sorted[i].Owner)
		}
	}

	for _, m := range sorted {
		if applied[m.Version] {
			continue
		}
		d.log.Info("执行迁移", "version", m.Version, "name", m.Name, "owner", m.Owner)

		for _, stmt := range splitStatements(m.SQL) {
			if _, err := d.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("store: 迁移 %d(%s) 执行失败: %w\n语句: %s",
					m.Version, m.Name, err, truncate(stmt, 200))
			}
		}

		if _, err := d.ExecContext(ctx,
			d.Rebind(`INSERT INTO schema_migrations (version, name, owner, applied_at) VALUES (?, ?, ?, ?)`),
			m.Version, m.Name, m.Owner, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("store: 记录迁移 %d 失败: %w", m.Version, err)
		}
	}
	return nil
}

func (d *DB) ensureMigrationTable(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			owner      TEXT NOT NULL DEFAULT '',
			applied_at TEXT NOT NULL
		)`)
	if err != nil {
		return fmt.Errorf("store: 创建迁移表失败: %w", err)
	}
	return nil
}

func (d *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := d.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: 读取迁移记录失败: %w", err)
	}
	defer rows.Close()

	out := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// AppliedMigrations 返回已执行迁移的清单，供后台展示。
func (d *DB) AppliedMigrations(ctx context.Context) ([]AppliedMigration, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT version, name, owner, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var m AppliedMigration
		if err := rows.Scan(&m.Version, &m.Name, &m.Owner, &m.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AppliedMigration 是已执行迁移的记录。
type AppliedMigration struct {
	Version   int    `json:"version"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	AppliedAt string `json:"applied_at"`
}

// splitStatements 按分号切分 SQL 语句，跳过空语句与注释行。
//
// 这是一个刻意的简化实现：不支持语句内的分号（如字符串字面量里的分号）。
// 迁移文件里请避免这种写法，或把语句拆成多条。
func splitStatements(sqlText string) []string {
	var out []string
	var cur strings.Builder

	for _, line := range strings.Split(sqlText, "\n") {
		trimmed := strings.TrimSpace(line)
		// 整行注释直接跳过，避免注释里的分号干扰切分
		if strings.HasPrefix(trimmed, "--") || trimmed == "" {
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}

	for _, stmt := range strings.Split(cur.String(), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

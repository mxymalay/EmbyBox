package assemble

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/mxymalay/embybox/pkg/kernel/store"
)

// openTestDB 创建一个内存数据库用于测试。
//
// 用 :memory: 而不是临时文件：测试更快，且不会在磁盘上留垃圾。
// store.Open 会把最大连接数限制为 1，所以内存库不会因为多连接而"消失"。
func openTestDB(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(context.Background(), store.Options{
		Driver: "sqlite",
		DSN:    ":memory:",
	})
	if err != nil {
		t.Fatalf("打开内存数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestKernelMigrations_AppliesToEmptyDB 验证迁移在空库上能跑通。
func TestKernelMigrations_AppliesToEmptyDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.Migrate(ctx, KernelMigrations()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 内核迁移建的表，一个都不能少
	wantTables := []string{
		"schema_migrations",
		"kv",
		"settings",
		"audit_log",
		"jobs",
		"job_runs",
		"instance_locks",
	}
	for _, name := range wantTables {
		if !tableExists(t, db, name) {
			t.Errorf("表 %s 没有被创建", name)
		}
	}

	// 迁移记录应完整
	applied, err := db.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("读取迁移记录失败: %v", err)
	}
	if len(applied) != len(KernelMigrations()) {
		t.Errorf("迁移记录数 %d，期望 %d", len(applied), len(KernelMigrations()))
	}
}

// TestKernelMigrations_Idempotent 验证重复执行是幂等的。
//
// 每次启动都会调 Migrate，如果它不幂等，第二次启动就会炸。
func TestKernelMigrations_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	for i := 1; i <= 3; i++ {
		if err := db.Migrate(ctx, KernelMigrations()); err != nil {
			t.Fatalf("第 %d 次迁移失败: %v", i, err)
		}
	}

	applied, err := db.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("读取迁移记录失败: %v", err)
	}
	if len(applied) != len(KernelMigrations()) {
		t.Errorf("重复执行后迁移记录变成了 %d 条，期望 %d 条（不幂等）",
			len(applied), len(KernelMigrations()))
	}
}

// TestKernelMigrations_OnPopulatedDB 验证在有数据的库上也能跑通。
//
// 这是竞品最容易踩的坑：迁移脚本只在空库上测过，
// 上线后遇到有数据的库就失败（比如加一个 NOT NULL 列但没给默认值）。
func TestKernelMigrations_OnPopulatedDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	all := KernelMigrations()
	if len(all) < 2 {
		t.Skip("内核迁移不足两条，无法测试增量场景")
	}

	// 1. 先用前一半迁移建库
	firstHalf := all[:len(all)/2]
	if err := db.Migrate(ctx, firstHalf); err != nil {
		t.Fatalf("前半段迁移失败: %v", err)
	}

	// 2. 灌入数据
	if _, err := db.ExecContext(ctx,
		db.Rebind(`INSERT INTO kv (namespace, key, value, updated_at) VALUES (?, ?, ?, ?)`),
		"test", "k1", "v1", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("写入测试数据失败: %v", err)
	}

	// 3. 跑全部迁移（含后半段）
	if err := db.Migrate(ctx, all); err != nil {
		t.Fatalf("在后半段迁移时失败（说明迁移不兼容已有数据）: %v", err)
	}

	// 4. 老数据必须还在
	var value string
	if err := db.QueryRowContext(ctx,
		db.Rebind(`SELECT value FROM kv WHERE namespace = ? AND key = ?`),
		"test", "k1").Scan(&value); err != nil {
		t.Fatalf("迁移后读不到老数据: %v", err)
	}
	if value != "v1" {
		t.Errorf("老数据被改动了：期望 v1，实际 %s", value)
	}
}

// TestKernelMigrations_NoDuplicateVersion 验证版本号不重复。
//
// 版本号撞号的后果极其隐蔽：迁移表里已有记录 → 新迁移被静默跳过 →
// 表结构不对但没有任何报错。必须在测试阶段就挡住。
func TestKernelMigrations_NoDuplicateVersion(t *testing.T) {
	seen := make(map[int]string)
	for _, m := range KernelMigrations() {
		if prev, dup := seen[m.Version]; dup {
			t.Errorf("迁移版本号 %d 重复：%q 与 %q", m.Version, prev, m.Name)
		}
		seen[m.Version] = m.Name
	}
}

// TestKernelMigrations_VersionRange 验证内核迁移占用 1-999 号段。
//
// 1000+ 保留给插件。越界会导致插件迁移与内核迁移撞号。
func TestKernelMigrations_VersionRange(t *testing.T) {
	for _, m := range KernelMigrations() {
		if m.Version < 1 || m.Version > 999 {
			t.Errorf("迁移 %q 的版本号 %d 越界：内核应使用 1-999，1000+ 保留给插件",
				m.Name, m.Version)
		}
		if m.Owner != "kernel" {
			t.Errorf("迁移 %q 的 owner 是 %q，内核迁移应为 kernel", m.Name, m.Owner)
		}
	}
}

// TestInstanceLock_MutualExclusion 验证实例锁的互斥语义。
//
// 这是"宁可第二个实例起不来，也不要它带着错误假设运行"的落地保证。
func TestInstanceLock_MutualExclusion(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.Migrate(ctx, KernelMigrations()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	const lockName = "test-plugin"

	// 第一次获取：应当成功
	held, err := acquireInstanceLock(ctx, db, lockName)
	if err != nil {
		t.Fatalf("获取锁出错: %v", err)
	}
	if !held {
		t.Fatal("首次获取锁应当成功，实际失败")
	}

	// 同一进程再获取：应当成功（视为续期）
	held, err = acquireInstanceLock(ctx, db, lockName)
	if err != nil {
		t.Fatalf("续期出错: %v", err)
	}
	if !held {
		t.Error("同一进程重复获取应当视为续期成功")
	}

	// 模拟另一个进程持有锁
	if _, err := db.ExecContext(ctx, db.Rebind(
		`UPDATE instance_locks SET owner = ?, expires_at = ? WHERE name = ?`),
		"otherhost#9999",
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		lockName); err != nil {
		t.Fatalf("模拟他人持锁失败: %v", err)
	}

	held, err = acquireInstanceLock(ctx, db, lockName)
	if err != nil {
		t.Fatalf("被他人持锁时不应返回错误，实际: %v", err)
	}
	if held {
		t.Error("锁被他人持有时不应获取成功")
	}

	// 锁过期后应当能重新获取
	if _, err := db.ExecContext(ctx, db.Rebind(
		`UPDATE instance_locks SET expires_at = ? WHERE name = ?`),
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		lockName); err != nil {
		t.Fatalf("设置过期锁失败: %v", err)
	}

	held, err = acquireInstanceLock(ctx, db, lockName)
	if err != nil {
		t.Fatalf("过期锁场景出错: %v", err)
	}
	if !held {
		t.Error("锁过期后应当能重新获取")
	}
}

// TestStore_RejectsDuplicateMigrationVersion 验证 store 层会拒绝重复版本号。
func TestStore_RejectsDuplicateMigrationVersion(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	dup := []store.Migration{
		{Version: 1, Name: "a", Owner: "kernel", SQL: "CREATE TABLE t1 (id INTEGER);"},
		{Version: 1, Name: "b", Owner: "kernel", SQL: "CREATE TABLE t2 (id INTEGER);"},
	}

	err := db.Migrate(ctx, dup)
	if err == nil {
		t.Fatal("重复的迁移版本号应当被拒绝")
	}
}

// ────────────────────── 辅助 ──────────────────────

func tableExists(t *testing.T, db *store.DB, name string) bool {
	t.Helper()

	var found string
	err := db.QueryRowContext(context.Background(),
		db.Rebind(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`),
		name).Scan(&found)

	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("查询表 %s 失败: %v", name, err)
	}
	return found == name
}

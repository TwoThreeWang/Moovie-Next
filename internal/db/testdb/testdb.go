// Package testdb 给需要真实数据库的测试提供连接。
//
// 每个测试进程（也就是每个包）使用自己的 schema，所以 go test ./... 可以并行跑各个包；
// 同一个包里，每个顶层测试开始前清空所有表。
package testdb

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"sync"
	"testing"

	"moovie/internal/config"
	"moovie/internal/db"
)

var (
	once     sync.Once
	shared   *db.Pool
	setupErr error
	skip     string
	lastTest string
	tables   []string
	schema   string
	address  string
)

// URL 返回测试库的连接地址。需要第二个连接（比如模拟旧库）的测试用它。
func URL(t *testing.T) string {
	t.Helper()
	Pool(t)
	return address
}

// Schema 返回当前测试进程使用的 schema 名。
func Schema() string { return schema }

// Pool 返回测试库连接。没有配置数据库时跳过当前测试。
func Pool(t *testing.T) *db.Pool {
	t.Helper()
	once.Do(setup)
	if skip != "" {
		t.Skip(skip)
	}
	if setupErr != nil {
		t.Fatalf("准备测试库失败: %v", setupErr)
	}
	root, _, _ := strings.Cut(t.Name(), "/")
	if root != lastTest {
		lastTest = root
		if _, err := shared.Exec(context.Background(), `TRUNCATE `+strings.Join(tables, ", ")+` RESTART IDENTITY CASCADE`); err != nil {
			t.Fatalf("清空测试库失败: %v", err)
		}
	}
	return shared
}

func setup() {
	config.LoadDotEnv()
	// 优先用专门的测试库；没有配置时用开发库。两种情况下测试都只在自己的 schema 里活动，
	// 不会读写 public 里的表。
	raw := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if raw == "" {
		raw = config.DatabaseURL()
	}
	if raw == "" {
		skip = "没有配置数据库，跳过需要数据库的测试"
		return
	}
	address = raw
	ctx := context.Background()
	hash := fnv.New32a()
	wd, _ := os.Getwd()
	hash.Write([]byte(wd))
	schema = fmt.Sprintf("t_%08x", hash.Sum32())

	admin, err := db.Open(ctx, raw, db.Options{MaxConns: 1})
	if err != nil {
		setupErr = err
		return
	}
	_, err = admin.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE; CREATE SCHEMA `+schema)
	admin.Close()
	if err != nil {
		setupErr = fmt.Errorf("创建测试 schema: %w", err)
		return
	}
	pool, err := db.Open(ctx, raw, db.Options{MaxConns: 8, SearchPath: schema + ",public"})
	if err != nil {
		setupErr = err
		return
	}
	if err := db.Migrate(ctx, pool); err != nil {
		setupErr = err
		return
	}
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = $1 AND tablename <> 'schema_migrations'`, schema)
	if err != nil {
		setupErr = err
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			setupErr = err
			return
		}
		tables = append(tables, schema+"."+name)
	}
	shared = pool
}

// User 插入指定 ID 的测试用户。
func User(t *testing.T, pool *db.Pool, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if _, err := pool.Exec(context.Background(), `INSERT INTO users (id, email, username, password_hash)
VALUES ($1, $2, $3, 'x') ON CONFLICT (id) DO NOTHING`, id, fmt.Sprintf("u%d@test.local", id), fmt.Sprintf("u%d", id)); err != nil {
			t.Fatalf("插入测试用户 %d: %v", id, err)
		}
	}
	if _, err := pool.Exec(context.Background(), `SELECT setval('users_id_seq', GREATEST((SELECT max(id) FROM users), 1))`); err != nil {
		t.Fatalf("调整用户序列: %v", err)
	}
}

// Media 插入指定 ID 的测试作品，标题为"作品<ID>"。
func Media(t *testing.T, pool *db.Pool, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if _, err := pool.Exec(context.Background(), `INSERT INTO media (id, title, metadata_status)
VALUES ($1, $2, 'complete') ON CONFLICT (id) DO NOTHING`, id, fmt.Sprintf("作品%d", id)); err != nil {
			t.Fatalf("插入测试作品 %d: %v", id, err)
		}
	}
	if _, err := pool.Exec(context.Background(), `SELECT setval('media_id_seq', GREATEST((SELECT max(id) FROM media), 1))`); err != nil {
		t.Fatalf("调整作品序列: %v", err)
	}
}

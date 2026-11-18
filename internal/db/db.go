// Package db 负责数据库连接和表结构迁移。
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Pool 是全站共用的连接池类型。
type Pool = pgxpool.Pool

// Querier 是 Pool 和事务都满足的最小接口，Store 只依赖它。
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Options 是打开连接池时的可选项。
type Options struct {
	MaxConns int
	// SearchPath 非空时，每个连接都把它设为 search_path。测试用它把各个包隔离到独立的 schema。
	SearchPath string
}

// Open 创建连接池并确认数据库可用。
func Open(ctx context.Context, url string, options Options) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("解析数据库地址: %w", err)
	}
	if options.MaxConns > 0 {
		cfg.MaxConns = int32(options.MaxConns)
	}
	if options.SearchPath != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = options.SearchPath
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("创建连接池: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}
	return pool, nil
}

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,40}$`)

// OpenApp 按应用配置打开连接池。schema 非空时先确保它存在，并让所有连接默认使用它。
func OpenApp(ctx context.Context, url, schema string, maxConns int) (*Pool, error) {
	options := Options{MaxConns: maxConns}
	if schema != "" {
		if !schemaName.MatchString(schema) {
			return nil, fmt.Errorf("DB_SCHEMA 只能用小写字母、数字和下划线: %q", schema)
		}
		bootstrap, err := Open(ctx, url, Options{MaxConns: 1})
		if err != nil {
			return nil, err
		}
		_, err = bootstrap.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+schema)
		bootstrap.Close()
		if err != nil {
			return nil, fmt.Errorf("创建 schema %s: %w", schema, err)
		}
		options.SearchPath = schema + ",public"
	}
	return Open(ctx, url, options)
}

// migrateLockKey 是迁移用的咨询锁编号，保证多个进程同时启动时只有一个在建表。
const migrateLockKey = 7226001

// Migrate 按文件名顺序执行还没执行过的迁移。
func Migrate(ctx context.Context, pool *Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("获取迁移锁: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockKey)

	// 扩展装在 public 里，所有 schema 共用。
	for _, extension := range []string{"pg_trgm", "vector"} {
		if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS `+extension+` WITH SCHEMA public`); err != nil {
			return fmt.Errorf("启用扩展 %s: %w", extension, err)
		}
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("创建迁移记录表: %w", err)
	}
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		var applied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("执行迁移 %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("提交迁移 %s: %w", version, err)
		}
	}
	return nil
}

// InTx 在事务里运行 fn，出错回滚，成功提交。
func InTx(ctx context.Context, pool *Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return tx.Commit(ctx)
}

// IsUniqueViolation 判断错误是不是唯一约束冲突。
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return asPgError(err, &pgErr) && pgErr.Code == "23505"
}

func asPgError(err error, target **pgconn.PgError) bool {
	for err != nil {
		if pgErr, ok := err.(*pgconn.PgError); ok {
			*target = pgErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

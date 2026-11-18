// testclean 删除测试留下的临时 schema（名字是 t_ 加 8 位十六进制）。不碰 public 和其他 schema。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"moovie/internal/config"
	"moovie/internal/db"
)

func main() {
	config.LoadDotEnv()
	address := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if address == "" {
		address = config.DatabaseURL()
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, address, db.Options{MaxConns: 1})
	if err != nil {
		fmt.Println("连接失败:", err)
		os.Exit(1)
	}
	defer pool.Close()
	rows, err := pool.Query(ctx, `SELECT nspname FROM pg_namespace WHERE nspname ~ '^t_[0-9a-f]{8}$'`)
	if err != nil {
		fmt.Println("查询失败:", err)
		os.Exit(1)
	}
	var names []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		names = append(names, name)
	}
	rows.Close()
	for _, name := range names {
		if _, err := pool.Exec(ctx, `DROP SCHEMA `+name+` CASCADE`); err != nil {
			fmt.Println("删除失败:", name, err)
			os.Exit(1)
		}
	}
	fmt.Printf("已删除 %d 个测试 schema\n", len(names))
}

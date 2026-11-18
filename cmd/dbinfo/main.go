// dbinfo 打印数据库的基本情况：版本、可用的扩展、已有的 schema 和表。只读，不修改任何东西。
package main

import (
	"context"
	"fmt"
	"os"

	"moovie/internal/config"
	"moovie/internal/db"
)

func main() {
	config.LoadDotEnv()
	address := config.DatabaseURL()
	if address == "" {
		fmt.Println("没有配置数据库地址")
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, address, db.Options{MaxConns: 1})
	if err != nil {
		fmt.Println("连接失败:", err)
		os.Exit(1)
	}
	defer pool.Close()
	var version, database, zone string
	pool.QueryRow(ctx, `SELECT split_part(version(), ' on ', 1), current_database(), current_setting('TimeZone')`).Scan(&version, &database, &zone)
	fmt.Printf("%s，数据库 %s，时区 %s\n", version, database, zone)
	for _, query := range []struct{ title, sql string }{
		{"可用扩展", `SELECT name || ' ' || default_version || CASE WHEN installed_version IS NULL THEN '' ELSE '（已安装）' END FROM pg_available_extensions WHERE name IN ('vector', 'pg_trgm') ORDER BY name`},
		{"schema", `SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema' ORDER BY nspname`},
		{"表（schema.表名）", `SELECT schemaname || '.' || tablename FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema') ORDER BY 1`},
	} {
		rows, err := pool.Query(ctx, query.sql)
		if err != nil {
			fmt.Println(query.title, "查询失败:", err)
			continue
		}
		fmt.Printf("%s:", query.title)
		count := 0
		for rows.Next() {
			var value string
			rows.Scan(&value)
			fmt.Printf(" %s", value)
			count++
		}
		rows.Close()
		if count == 0 {
			fmt.Print(" （无）")
		}
		fmt.Println()
	}
}

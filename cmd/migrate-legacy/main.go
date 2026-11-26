// migrate-legacy 把旧版 Moovie 的数据迁移到新库。
//
// 用法：
//
//	LEGACY_DATABASE_URL=postgres://…/moovie_v2 go run ./cmd/migrate-legacy -dry-run   # 试运行：只看报告，不写入
//	LEGACY_DATABASE_URL=postgres://…/moovie_v2 go run ./cmd/migrate-legacy            # 正式迁移
//
// 也可以用 LEGACY_DB_NAME 只指定旧库的库名（主机、账号沿用新库的 DB_* 配置）。
// 新库的地址和 web 服务相同（DATABASE_URL 或 DB_*，以及可选的 DB_SCHEMA）。
// 旧库只读；新库的写入在一个事务里完成，可以反复运行。
//
// 临时：-sources 文件名 只把旧库里启用的播放源导出成订阅文件，不迁移任何数据。
// 导出的文件放到示例订阅的外部地址（见 internal/app/example_subscription.go），之后会删掉。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"moovie/internal/config"
	"moovie/internal/db"
	"moovie/internal/legacy"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "只输出报告，不写入新库")
	sourcesFile := flag.String("sources", "", "临时：只把旧库的播放源导出成订阅文件，写到这个路径")
	flag.Parse()
	config.LoadDotEnv()
	target := config.DatabaseURL()
	source := strings.TrimSpace(os.Getenv("LEGACY_DATABASE_URL"))
	if source == "" && os.Getenv("LEGACY_DB_NAME") != "" && target != "" {
		parsed, _ := url.Parse(target)
		parsed.Path = "/" + os.Getenv("LEGACY_DB_NAME")
		source = parsed.String()
	}
	if *sourcesFile != "" {
		exportSources(source, *sourcesFile)
		return
	}
	if source == "" || target == "" {
		fail("需要配置旧库（LEGACY_DATABASE_URL 或 LEGACY_DB_NAME）和新库（DATABASE_URL 或 DB_*）")
	}
	if sameDatabase(source, target) && os.Getenv("DB_SCHEMA") == "" {
		fail("旧库和新库是同一个数据库，并且没有设置 DB_SCHEMA。新版的表会和旧表冲突，请换一个库或设置 DB_SCHEMA")
	}
	ctx := context.Background()
	src, err := db.Open(ctx, source, db.Options{MaxConns: 2})
	if err != nil {
		fail("连接旧库: " + err.Error())
	}
	defer src.Close()
	dst, err := db.OpenApp(ctx, target, os.Getenv("DB_SCHEMA"), 2)
	if err != nil {
		fail("连接新库: " + err.Error())
	}
	defer dst.Close()
	if err := db.Migrate(ctx, dst); err != nil {
		fail("建表: " + err.Error())
	}
	report, err := legacy.Run(ctx, src, dst, legacy.Options{DryRun: *dryRun, Progress: os.Stderr})
	if report != nil {
		report.Print(os.Stdout)
	}
	if err != nil {
		fail("迁移失败，新库没有任何改动: " + err.Error())
	}
	if *dryRun {
		fmt.Println("\n试运行：以上是迁移结果的预览，新库没有任何改动。")
		return
	}
	fmt.Println("\n迁移完成。")
}

// exportSources 是临时功能，见 internal/legacy/sources.go。
func exportSources(source, path string) {
	if source == "" {
		fail("需要配置旧库（LEGACY_DATABASE_URL 或 LEGACY_DB_NAME）")
	}
	ctx := context.Background()
	src, err := db.Open(ctx, source, db.Options{MaxConns: 1})
	if err != nil {
		fail("连接旧库: " + err.Error())
	}
	defer src.Close()
	subscription, skipped, err := legacy.ExportSources(ctx, src)
	if err != nil {
		fail("读取播放源: " + err.Error())
	}
	encoded, _ := json.MarshalIndent(subscription, "", "  ")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		fail("写文件: " + err.Error())
	}
	fmt.Printf("导出 %d 个源到 %s。\n", len(subscription.Sources), path)
	for _, reason := range skipped {
		fmt.Println("跳过：" + reason)
	}
}

func sameDatabase(a, b string) bool {
	pa, errA := url.Parse(a)
	pb, errB := url.Parse(b)
	return errA == nil && errB == nil && pa.Host == pb.Host && pa.Path == pb.Path
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}

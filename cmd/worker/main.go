// worker 单独运行后台任务（元数据采集、榜单、向量、豆瓣同步）。web 进程设置 JOBS_IN_WEB=false 时使用。
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"moovie/internal/app"
	"moovie/internal/config"
	"moovie/internal/db"
	"moovie/internal/jobs"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.Load()
	if err != nil {
		log.Error("读取配置失败", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := db.OpenApp(ctx, cfg.DatabaseURL, cfg.DBSchema, cfg.DBMaxConns)
	if err != nil {
		log.Error("连接数据库失败", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		log.Error("迁移失败", "error", err)
		os.Exit(1)
	}
	application, err := app.New(cfg, pool, log, app.Options{})
	if err != nil {
		log.Error("初始化失败", "error", err)
		os.Exit(1)
	}
	worker := jobs.NewWorker(application.Queue, cfg.WorkerConcurrency, log)
	application.RegisterJobs(worker)
	log.Info("后台任务已启动", "concurrency", cfg.WorkerConcurrency)
	worker.Run(ctx)
}

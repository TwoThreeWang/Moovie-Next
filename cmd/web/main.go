// web 是网站的 HTTP 服务。JOBS_IN_WEB=true 时同一个进程里也跑后台任务。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"moovie/internal/app"
	"moovie/internal/config"
	"moovie/internal/db"
	"moovie/internal/jobs"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("启动失败", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.OpenApp(ctx, cfg.DatabaseURL, cfg.DBSchema, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	application, err := app.New(cfg, pool, log, app.Options{})
	if err != nil {
		return err
	}
	// 热搜的计数先在内存里累加，每分钟写一次库；服务停下后再写一次，不丢最后这一分钟。
	go application.Trends.Run(ctx, log)
	defer application.Trends.Flush(context.Background())
	if cfg.JobsInWeb {
		worker := jobs.NewWorker(application.Queue, cfg.WorkerConcurrency, log)
		application.RegisterJobs(worker)
		go worker.Run(ctx)
	}

	server := &http.Server{
		Addr: ":" + cfg.Port, Handler: application.Router(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
	}
	failed := make(chan error, 1)
	go func() {
		log.Info("服务已启动", "address", "http://localhost:"+cfg.Port, "env", cfg.Env, "jobs", cfg.JobsInWeb)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

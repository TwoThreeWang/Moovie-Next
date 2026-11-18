// hongguo-seed 在本地把红果短剧官网的全部短剧抓一遍，存成快照文件，再导入作品库。
// 线上只做增量（见 internal/ingest/hongguo.go），全量这一次放在本地做：不占线上的执行位，
// 万一被风控也只影响本机的 IP。
//
// 用法：
//
//	go run ./cmd/hongguo-seed crawl                       # 抓到 data/hongguo.jsonl，中断后再运行会接着抓
//	go run ./cmd/hongguo-seed crawl -limit 50             # 先抓 50 部试试
//	go run ./cmd/hongguo-seed import                      # 导入 DATABASE_URL（或 DB_*）指向的库
//
// 快照文件一行一部短剧（ingest.ShortRecord 的 JSON）：抓到的资料，或者"这个 ID 没有剧"。
// 抓失败的不写，下次运行重试。导入按 series_id 有则更新、无则插入，可以反复运行。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"moovie/internal/config"
	"moovie/internal/db"
	"moovie/internal/ingest"
	"moovie/internal/jobs"
	"moovie/internal/media"
	"moovie/internal/providers"
)

const defaultFile = "data/hongguo.jsonl"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch os.Args[1] {
	case "crawl":
		flags := flag.NewFlagSet("crawl", flag.ExitOnError)
		output := flags.String("o", defaultFile, "快照文件")
		interval := flags.Duration("interval", time.Second, "两次请求之间至少间隔多久")
		limit := flags.Int("limit", 0, "这次最多抓多少部（0 表示不限）")
		important := flags.Bool("important", false, "只抓重点剧目（约 2000 部）")
		flags.Parse(os.Args[2:])
		if err := crawl(ctx, *output, *interval, *limit, *important); err != nil {
			fail(err.Error())
		}
	case "import":
		flags := flag.NewFlagSet("import", flag.ExitOnError)
		input := flags.String("i", defaultFile, "快照文件")
		workers := flags.Int("workers", 8, "同时写库的连接数")
		flags.Parse(os.Args[2:])
		if err := importFile(ctx, *input, *workers); err != nil {
			fail(err.Error())
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法：hongguo-seed crawl [-o 文件] [-interval 1s] [-limit N] [-important]\n      hongguo-seed import [-i 文件] [-workers 8]")
	os.Exit(2)
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}

// ---------- 抓取 ----------

func crawl(ctx context.Context, path string, interval time.Duration, limit int, importantOnly bool) error {
	done, err := prepareFile(path)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	client := providers.NewHongguo(&http.Client{Timeout: 30 * time.Second})
	client.Pace = providers.NewPacer(interval)
	client.Pace.Name = "红果"

	// 1. 剧种：详情页不给剧种，只有分类页和分剧种的榜单给。先把这些列表读一遍（一百来次请求）。
	forms := map[string]string{}
	if !importantOnly {
		fmt.Println("读取分类页和分剧种的榜单，记下剧种……")
		for _, form := range providers.HongguoForms {
			for page, pages := 1, 1; page <= pages && page <= 50; page++ {
				cards, total, err := retry(ctx, client, func() ([]providers.HongguoCard, int, error) { return client.Category(ctx, form.Name, page) })
				if err != nil {
					fmt.Printf("  %s第 %d 页没取到，跳过: %v\n", form.Name, page, err)
					break
				}
				pages = total
				for _, card := range cards {
					forms[card.ID] = form.Name
				}
			}
			for _, prefix := range []string{"hot-", "new-"} {
				cards, _, err := retry(ctx, client, func() ([]providers.HongguoCard, int, error) {
					cards, err := client.Rank(ctx, prefix+form.Slug+"-drama", 1)
					return cards, 0, err
				})
				if err != nil {
					continue
				}
				for _, card := range cards {
					forms[card.ID] = form.Name
				}
			}
		}
		fmt.Printf("  知道剧种的有 %d 部\n", len(forms))
	}

	// 2. 要抓的 ID：重点剧目在前，然后是站点地图里的全部。
	fmt.Println("读取站点地图……")
	ids, err := allIDs(ctx, client, importantOnly)
	if err != nil {
		return err
	}
	todo := make([]string, 0, len(ids))
	for _, id := range ids {
		if !done[id] {
			todo = append(todo, id)
		}
	}
	if limit > 0 && len(todo) > limit {
		todo = todo[:limit]
	}
	fmt.Printf("一共 %d 部，已经抓过 %d 部，这次抓 %d 部（每次请求至少间隔 %s，按 Ctrl-C 可以随时停，下次接着抓）\n",
		len(ids), len(done), len(todo), interval)

	// 3. 逐部抓详情，抓到一部写一行。
	start := time.Now()
	var saved, missing, failed int
	for index, id := range todo {
		if ctx.Err() != nil {
			break
		}
		record := ingest.ShortRecord{ID: id, Form: forms[id]}
		series, _, err := retry(ctx, client, func() (*providers.HongguoSeries, int, error) {
			series, err := client.Series(ctx, id)
			return series, 0, err
		})
		switch {
		case errors.Is(err, providers.ErrNotFound):
			record.Missing = true
			missing++
		case err != nil:
			if ctx.Err() == nil {
				fmt.Printf("  %s 没抓到（下次再试）: %v\n", id, err)
				failed++
			}
			continue
		default:
			record.Series = series
			saved++
		}
		line, _ := json.Marshal(record)
		if _, err := file.Write(append(line, '\n')); err != nil {
			return fmt.Errorf("写快照文件: %w", err)
		}
		if count := index + 1; count%200 == 0 {
			elapsed := time.Since(start)
			left := time.Duration(float64(elapsed) / float64(count) * float64(len(todo)-count))
			fmt.Printf("  %d / %d，用时 %s，预计还要 %s\n", count, len(todo), elapsed.Round(time.Second), left.Round(time.Minute))
		}
	}
	fmt.Printf("这次抓到 %d 部，确认没有的 %d 个，失败 %d 个，用时 %s。快照文件：%s\n",
		saved, missing, failed, time.Since(start).Round(time.Second), path)
	return nil
}

// retry 执行一次请求；被限流（包括整体暂停）时等对方恢复再试，其他错误直接返回。
func retry[T any](ctx context.Context, client *providers.Hongguo, request func() (T, int, error)) (T, int, error) {
	for {
		value, n, err := request()
		if !errors.Is(err, providers.ErrThrottled) || ctx.Err() != nil {
			return value, n, err
		}
		wait := 10 * time.Second
		if _, until := client.Pace.Status(); !until.IsZero() {
			wait = time.Until(until) + time.Second
		}
		fmt.Printf("  被限流了，%s 后再试: %v\n", wait.Round(time.Second), err)
		select {
		case <-ctx.Done():
			return value, n, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func allIDs(ctx context.Context, client *providers.Hongguo, importantOnly bool) ([]string, error) {
	paths := []string{providers.ImportantSitemap}
	if !importantOnly {
		maps, _, err := retry(ctx, client, func() ([]string, int, error) {
			maps, err := client.Sitemaps(ctx)
			return maps, 0, err
		})
		if err != nil {
			return nil, fmt.Errorf("站点地图索引: %w", err)
		}
		paths = append(paths, maps...)
	}
	seen := map[string]bool{}
	ids := []string{}
	for _, path := range paths {
		list, _, err := retry(ctx, client, func() ([]string, int, error) {
			list, _, err := client.SitemapIDs(ctx, path, "")
			return list, 0, err
		})
		if err != nil {
			return nil, fmt.Errorf("站点地图 %s: %w", path, err)
		}
		for _, id := range list {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

// prepareFile 读出快照文件里已经有的 ID。上次是被强行打断的、最后一行没写完时，把那半行截掉。
func prepareFile(path string) (map[string]bool, error) {
	done := map[string]bool{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
			return nil, err
		}
		return done, nil
	}
	if err != nil {
		return nil, err
	}
	if end := bytes.LastIndexByte(data, '\n'); end+1 != len(data) {
		if err := os.Truncate(path, int64(end+1)); err != nil {
			return nil, err
		}
		data = data[:end+1]
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		var record struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(line, &record) == nil && record.ID != "" {
			done[record.ID] = true
		}
	}
	return done, nil
}

func dirOf(path string) string {
	for index := len(path) - 1; index >= 0; index-- {
		if path[index] == '/' {
			return path[:index]
		}
	}
	return "."
}

// ---------- 导入 ----------

func importFile(ctx context.Context, path string, workers int) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	config.LoadDotEnv()
	address := config.DatabaseURL()
	if address == "" {
		return errors.New("需要配置数据库（DATABASE_URL 或 DB_*）")
	}
	workers = max(1, workers)
	pool, err := db.OpenApp(ctx, address, os.Getenv("DB_SCHEMA"), workers+1)
	if err != nil {
		return fmt.Errorf("连接数据库: %w", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("建表: %w", err)
	}
	service := &ingest.Service{Media: media.NewStore(pool), Queue: jobs.NewQueue(pool), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	records := make(chan ingest.ShortRecord, workers*4)
	var imported, missing, failed atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for record := range records {
				if err := service.ImportShort(ctx, record); err != nil {
					if ctx.Err() == nil {
						fmt.Printf("  %s 导入失败: %v\n", record.ID, err)
					}
					failed.Add(1)
				} else if record.Missing {
					missing.Add(1)
				} else {
					imported.Add(1)
				}
			}
		}()
	}
	start := time.Now()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	read, broken := 0, 0
	for scanner.Scan() && ctx.Err() == nil {
		var record ingest.ShortRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil || !media.ValidHongguoID(record.ID) {
			broken++
			continue
		}
		records <- record
		if read++; read%5000 == 0 {
			fmt.Printf("  已读 %d 行，用时 %s\n", read, time.Since(start).Round(time.Second))
		}
	}
	close(records)
	wg.Wait()
	if err := scanner.Err(); err != nil {
		return err
	}
	fmt.Printf("导入 %d 部短剧，%d 个确认没有的 ID 记成 not_found，失败 %d 个，格式不对跳过 %d 行，用时 %s。\n",
		imported.Load(), missing.Load(), failed.Load(), broken, time.Since(start).Round(time.Second))
	return ctx.Err()
}

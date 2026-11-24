package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"moovie/internal/db/testdb"
	"moovie/internal/jobs"
)

func newWorker(t *testing.T) (*jobs.Queue, *jobs.Worker) {
	queue := jobs.NewQueue(testdb.Pool(t))
	return queue, jobs.NewWorker(queue, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestEnqueueDeduplicatesActiveJobs(t *testing.T) {
	queue, worker := newWorker(t)
	ctx := context.Background()
	ran := 0
	worker.Handle("demo", func(context.Context, jobs.Job) error { ran++; return nil })

	if queued, err := queue.Enqueue(ctx, "demo", "1", jobs.Options{}); err != nil || !queued {
		t.Fatalf("第一次应该入队: %v %v", queued, err)
	}
	if queued, _ := queue.Enqueue(ctx, "demo", "1", jobs.Options{}); queued {
		t.Error("同一个对象已经在排队，不应该重复入队")
	}
	if queued, _ := queue.Enqueue(ctx, "demo", "2", jobs.Options{}); !queued {
		t.Error("不同对象应该能入队")
	}
	for range 2 {
		if worked, err := worker.RunOne(ctx); err != nil || !worked {
			t.Fatalf("应该执行一个任务: %v %v", worked, err)
		}
	}
	if worked, _ := worker.RunOne(ctx); worked || ran != 2 {
		t.Errorf("队列应该空了，共执行 2 次；实际 worked=%v ran=%d", worked, ran)
	}
	// 上一个已经完成，可以再次入队。
	if queued, _ := queue.Enqueue(ctx, "demo", "1", jobs.Options{}); !queued {
		t.Error("完成之后应该能再次入队")
	}
}

func TestFailedJobsRetryThenFail(t *testing.T) {
	queue, worker := newWorker(t)
	pool := testdb.Pool(t)
	ctx := context.Background()
	attempts := 0
	worker.Handle("flaky", func(context.Context, jobs.Job) error { attempts++; return errors.New("上游出错") })
	queue.Enqueue(ctx, "flaky", "x", jobs.Options{})

	var waits []time.Duration
	for range jobs.MaxAttempts {
		// 失败后会按退避时间重新排队；记下要等多久，再把时间拨回来。
		pool.Exec(ctx, `UPDATE jobs SET run_at = now() WHERE status = 'queued'`)
		if worked, err := worker.RunOne(ctx); err != nil || !worked {
			t.Fatalf("应该执行: %v %v", worked, err)
		}
		if job, _, _ := queue.Latest(ctx, "flaky", "x"); job.Status == "queued" {
			waits = append(waits, time.Until(job.RunAt).Round(time.Minute))
		}
	}
	want := []time.Duration{time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Errorf("重试间隔应该逐次加长 %v，实际 %v", want, waits)
	}
	job, found, err := queue.Latest(ctx, "flaky", "x")
	if err != nil || !found || job.Status != "failed" || job.Attempts != jobs.MaxAttempts || job.LastError != "上游出错" {
		t.Fatalf("重试用完后应该标记为失败: %+v err=%v", job, err)
	}
	// 失败冷却期：各个入口都排不进去；管理员强制可以；成功一次就清掉失败记录。
	if queued, _ := queue.Enqueue(ctx, "flaky", "x", jobs.Options{}); queued {
		t.Error("失败冷却期内不应该再入队")
	}
	if queued, _ := queue.Enqueue(ctx, "periodic", "", jobs.Options{}); !queued {
		t.Error("周期任务不受冷却期影响")
	}
	pool.Exec(ctx, `DELETE FROM jobs WHERE kind = 'periodic'`)
	if count, err := queue.Retry(ctx, 0); err != nil || count != 1 {
		t.Fatalf("应该重新排队 1 个: %d %v", count, err)
	}
	counts, _ := queue.Counts(ctx)
	if counts["queued"] != 1 || counts["failed"] != 0 {
		t.Errorf("重试后的状态统计不对: %v", counts)
	}
}

func TestSkipPanicAndUnknownKind(t *testing.T) {
	queue, worker := newWorker(t)
	ctx := context.Background()
	worker.Handle("skip", func(context.Context, jobs.Job) error { return jobs.ErrSkip })
	worker.Handle("panic", func(context.Context, jobs.Job) error { panic("炸了") })
	queue.Enqueue(ctx, "skip", "", jobs.Options{Priority: 3})
	queue.Enqueue(ctx, "panic", "", jobs.Options{Priority: 2})
	queue.Enqueue(ctx, "nobody", "", jobs.Options{Priority: 1})
	for range 3 {
		if _, err := worker.RunOne(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if job, _, _ := queue.Latest(ctx, "skip", ""); job.Status != "done" {
		t.Errorf("ErrSkip 应该直接标记完成，实际 %s", job.Status)
	}
	if job, _, _ := queue.Latest(ctx, "panic", ""); job.Status != "queued" || job.LastError == "" {
		t.Errorf("panic 应该被当作普通失败并重试，实际 %s %q", job.Status, job.LastError)
	}
	if job, _, _ := queue.Latest(ctx, "nobody", ""); job.Status != "failed" {
		t.Errorf("没有注册处理函数的任务应该直接失败，实际 %s", job.Status)
	}
}

func TestProgressAndHousekeep(t *testing.T) {
	queue, worker := newWorker(t)
	pool := testdb.Pool(t)
	ctx := context.Background()
	worker.Handle("long", func(ctx context.Context, job jobs.Job) error {
		return queue.SetProgress(ctx, job.ID, map[string]int{"processed": 5})
	})
	queue.Enqueue(ctx, "long", "u1", jobs.Options{Payload: map[string]string{"a": "b"}})
	worker.RunOne(ctx)
	job, _, _ := queue.Latest(ctx, "long", "u1")
	if string(job.Progress) != `{"processed": 5}` {
		t.Errorf("进度没有保存: %s", job.Progress)
	}
	// 租约过期的运行中任务应该被放回队列。
	pool.Exec(ctx, `INSERT INTO jobs (kind, subject, status, locked_until) VALUES ('stuck', '', 'running', now() - interval '1 minute')`)
	if err := queue.Housekeep(ctx); err != nil {
		t.Fatal(err)
	}
	if job, _, _ := queue.Latest(ctx, "stuck", ""); job.Status != "queued" {
		t.Errorf("租约过期的任务应该回到队列，实际 %s", job.Status)
	}
}

// 已经在排队的任务遇到更急的请求：不新建，把原来那个提前。
func TestEnqueueRaisesPriorityOfQueuedJob(t *testing.T) {
	queue, worker := newWorker(t)
	ctx := context.Background()
	var order []string
	worker.Handle("refresh", func(_ context.Context, job jobs.Job) error { order = append(order, job.Subject); return nil })
	queue.Enqueue(ctx, "refresh", "imported", jobs.Options{})
	queue.Enqueue(ctx, "refresh", "other", jobs.Options{})
	queue.Enqueue(ctx, "refresh", "viewed", jobs.Options{})
	if queued, err := queue.Enqueue(ctx, "refresh", "viewed", jobs.Options{Priority: 20}); err != nil || queued {
		t.Fatalf("已经在排队，不应该新建: %v %v", queued, err)
	}
	if queued, _ := queue.Enqueue(ctx, "refresh", "viewed", jobs.Options{Priority: -5}); queued {
		t.Error("更低的优先级不应该改动已有的任务")
	}
	for range 3 {
		worker.RunOne(ctx)
	}
	if len(order) != 3 || order[0] != "viewed" {
		t.Errorf("有人在等的那个应该先做，实际顺序 %v", order)
	}
}

// 一条通道暂停时，它的任务原样留在队列里；别的任务照常执行。
func TestHeldLaneKeepsJobsAndDoesNotBlockOthers(t *testing.T) {
	queue, worker := newWorker(t)
	ctx := context.Background()
	ran := map[string]int{}
	var mu sync.Mutex
	count := func(_ context.Context, job jobs.Job) error { mu.Lock(); ran[job.Kind]++; mu.Unlock(); return nil }
	worker.Handle("douban", count)
	worker.Handle("embed", count)
	worker.Lane(jobs.Lane{Kinds: []string{"douban"}, Loops: 2, Hold: func() time.Duration { return time.Hour }})
	for _, subject := range []string{"1", "2", "3"} {
		queue.Enqueue(ctx, "douban", subject, jobs.Options{Priority: 20})
		queue.Enqueue(ctx, "embed", subject, jobs.Options{})
	}
	runCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	worker.Run(runCtx)

	mu.Lock()
	defer mu.Unlock()
	if ran["embed"] != 3 || ran["douban"] != 0 {
		t.Fatalf("暂停的通道不该执行，其他任务应该全部完成: %v", ran)
	}
	var queued, attempts int
	testdb.Pool(t).QueryRow(ctx, `SELECT count(*), COALESCE(sum(attempts), 0) FROM jobs WHERE kind = 'douban' AND status = 'queued'`).Scan(&queued, &attempts)
	if queued != 3 || attempts != 0 {
		t.Errorf("暂停期间任务应该原样留在队列里、不消耗重试次数：排队 %d 个，已尝试 %d 次", queued, attempts)
	}
}

// 长任务优先级再高，也占不满一条通道：留出的执行位照样做短任务。
func TestLongJobsLeaveReservedLoops(t *testing.T) {
	queue, worker := newWorker(t)
	ctx := context.Background()
	var mu sync.Mutex
	running, most, quick := 0, 0, 0
	worker.Handle("slow", func(ctx context.Context, _ jobs.Job) error {
		mu.Lock()
		running++
		most = max(most, running)
		mu.Unlock()
		<-ctx.Done()
		mu.Lock()
		running--
		mu.Unlock()
		return ctx.Err()
	})
	worker.Handle("quick", func(context.Context, jobs.Job) error { mu.Lock(); quick++; mu.Unlock(); return nil })
	worker.Lane(jobs.Lane{Kinds: []string{"slow", "quick"}, Loops: 3, Long: []string{"slow"}, Reserved: 1})
	for _, subject := range []string{"1", "2", "3"} {
		queue.Enqueue(ctx, "slow", subject, jobs.Options{Priority: 20})
	}
	queue.Enqueue(ctx, "quick", "1", jobs.Options{})
	queue.Enqueue(ctx, "quick", "2", jobs.Options{})
	runCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	worker.Run(runCtx)

	mu.Lock()
	defer mu.Unlock()
	if most != 2 || quick != 2 {
		t.Errorf("长任务最多同时跑 2 个，短任务应该照常做完：长任务最多 %d 个，短任务做了 %d 个", most, quick)
	}
}

// 执行情况从任务表里统计：做完了几个、最近一次什么时候做完、排队的任务最近一次为什么出错。
func TestActivity(t *testing.T) {
	queue, worker := newWorker(t)
	ctx := context.Background()
	worker.Handle("douban", func(_ context.Context, job jobs.Job) error {
		if job.Subject == "bad" {
			return errors.New("豆瓣返回 HTTP 403")
		}
		return nil
	})
	if activity, err := queue.Activity(ctx, "douban"); err != nil || activity.Done != 0 || activity.LastDone != nil || activity.LastError != "" {
		t.Fatalf("还没有任务时应该是空的: %+v %v", activity, err)
	}
	queue.Enqueue(ctx, "douban", "good", jobs.Options{Priority: 1})
	queue.Enqueue(ctx, "douban", "bad", jobs.Options{})
	queue.Enqueue(ctx, "other", "x", jobs.Options{})
	worker.RunOne(ctx)
	worker.RunOne(ctx)
	activity, err := queue.Activity(ctx, "douban")
	if err != nil || activity.Done != 1 || activity.LastDone == nil || activity.LastError != "豆瓣返回 HTTP 403" {
		t.Errorf("应该做完 1 个、记下最近的错误: %+v %v", activity, err)
	}
}

// 被对方限流的失败：任务放回队列稍后再做，不算一次失败，重试次数不会被耗光。
func TestPostponedFailureDoesNotCountAsAttempt(t *testing.T) {
	queue, worker := newWorker(t)
	pool := testdb.Pool(t)
	ctx := context.Background()
	throttled := errors.New("被限流了")
	worker.Handle("douban", func(context.Context, jobs.Job) error { return throttled })
	worker.Lane(jobs.Lane{Kinds: []string{"douban"}, Postpone: func(err error) bool { return errors.Is(err, throttled) }})
	queue.Enqueue(ctx, "douban", "x", jobs.Options{})
	for range 5 {
		pool.Exec(ctx, `UPDATE jobs SET run_at = now() WHERE status = 'queued'`)
		if worked, err := worker.RunOne(ctx); err != nil || !worked {
			t.Fatalf("应该执行: %v %v", worked, err)
		}
	}
	job, _, _ := queue.Latest(ctx, "douban", "x")
	if job.Status != "queued" || job.Attempts != 0 || job.LastError != "被限流了" || !job.RunAt.After(time.Now().Add(30*time.Second)) {
		t.Errorf("被限流 5 次后任务应该还在排队、重试次数为 0、推迟到一分钟后: %+v", job)
	}
}

// 管理员强制入队会清掉失败记录；任务成功一次也会清掉，之后各个入口又能正常排进来。
func TestForceAndSuccessClearCooling(t *testing.T) {
	queue, worker := newWorker(t)
	pool := testdb.Pool(t)
	ctx := context.Background()
	worker.Handle("refresh", func(context.Context, jobs.Job) error { return nil })
	pool.Exec(ctx, `INSERT INTO jobs (kind, subject, status, finished_at) VALUES ('refresh', '7', 'failed', now())`)
	if queued, _ := queue.Enqueue(ctx, "refresh", "7", jobs.Options{}); queued {
		t.Fatal("冷却期内不应该入队")
	}
	if queued, err := queue.Enqueue(ctx, "refresh", "7", jobs.Options{Force: true}); err != nil || !queued {
		t.Fatalf("强制入队应该成功: %v %v", queued, err)
	}
	pool.Exec(ctx, `INSERT INTO jobs (kind, subject, status, finished_at) VALUES ('refresh', '7', 'failed', now())`)
	worker.RunOne(ctx)
	var failed int
	pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE status = 'failed'`).Scan(&failed)
	if failed != 0 {
		t.Errorf("成功之后失败记录应该清掉，还剩 %d 条", failed)
	}
	if queued, _ := queue.Enqueue(ctx, "refresh", "7", jobs.Options{}); !queued {
		t.Error("成功之后应该能正常入队")
	}
}

// 推迟只在任务入队 3 天内有效，之后按普通失败算，免得一直被拒绝的对象永远在队列里转。
func TestPostponeHasALimit(t *testing.T) {
	queue, worker := newWorker(t)
	pool := testdb.Pool(t)
	ctx := context.Background()
	throttled := errors.New("被限流了")
	worker.Handle("douban", func(context.Context, jobs.Job) error { return throttled })
	worker.Lane(jobs.Lane{Kinds: []string{"douban"}, Postpone: func(err error) bool { return errors.Is(err, throttled) }})
	queue.Enqueue(ctx, "douban", "old", jobs.Options{})
	pool.Exec(ctx, `UPDATE jobs SET created_at = now() - interval '4 days'`)
	worker.RunOne(ctx)
	if job, _, _ := queue.Latest(ctx, "douban", "old"); job.Attempts != 1 {
		t.Errorf("入队超过 3 天的任务被限流应该算一次失败，实际已尝试 %d 次", job.Attempts)
	}
}

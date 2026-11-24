// Package jobs 是基于数据库的后台任务队列。
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"moovie/internal/db"
)

// Job 是一个待执行或已执行的任务。
type Job struct {
	ID        int64
	Kind      string
	Subject   string
	Payload   json.RawMessage
	Priority  int
	Status    string
	Attempts  int
	RunAt     time.Time
	Progress  json.RawMessage
	LastError string
	CreatedAt time.Time
	Finished  *time.Time
}

// Handler 执行一种任务。返回 error 表示失败，会按退避时间重试。
type Handler func(ctx context.Context, job Job) error

// ErrSkip 让任务直接标记为完成而不重试，用于"这个对象已经不存在"这类情况。
var ErrSkip = errors.New("跳过任务")

// retryDelays 是失败后第几次重试要等多久。越往后等得越久：对方短时间出故障时很快就能补上，
// 一直失败的也不会一直打扰对方。全部用完（约 31 小时）就标记为失败。
var retryDelays = []time.Duration{time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}

// MaxAttempts 是一个任务最多执行几次。
var MaxAttempts = len(retryDelays) + 1

// postponeLimit 是"稍后再做、不算失败"最多能拖多久。超过之后再推迟就按普通失败算，
// 免得某个对象一直让对方拒绝（比如豆瓣对某个条目总是返回 403），任务永远在队列里转。
const postponeLimit = 3 * 24 * time.Hour

// defaultLease 要长于 AI 请求的 10 分钟超时，避免请求仍在执行时被回收并重复调用。
const defaultLease = 15 * time.Minute

// Queue 读写 jobs 表。
type Queue struct{ pool *db.Pool }

// NewQueue 创建 Queue。
func NewQueue(pool *db.Pool) *Queue { return &Queue{pool: pool} }

// Options 是入队时的可选项。
type Options struct {
	Payload  any
	Priority int
	Delay    time.Duration
	// Force 为 true 时不管冷却期，照样入队（管理员手动操作用）。
	Force bool
}

// Enqueue 把任务放进队列，返回是否新建了一个。
//
//   - 同一个 kind + subject 已经在排队或运行时不重复入队。如果排着的那个优先级更低，
//     就把它提到这次的优先级（并换成这次的参数）：比如一部作品本来排在批量导入的队尾，
//     这时有人打开了它的页面，它应该马上被处理。
//   - 冷却期：同一个对象的这种任务重试用完失败了，失败记录保留的 14 天里不再入队
//     （周期任务、Force 除外）。这样一个一直出错的对象不会被各个入口反复送去请求对方；
//     成功一次就会清掉失败记录。
func (q *Queue) Enqueue(ctx context.Context, kind, subject string, options Options) (bool, error) {
	payload := []byte("{}")
	if options.Payload != nil {
		encoded, err := json.Marshal(options.Payload)
		if err != nil {
			return false, err
		}
		payload = encoded
	}
	if options.Force && subject != "" {
		if _, err := q.pool.Exec(ctx, `DELETE FROM jobs WHERE kind = $1 AND subject = $2 AND status = 'failed'`, kind, subject); err != nil {
			return false, err
		}
	}
	var inserted bool
	err := q.pool.QueryRow(ctx, `INSERT INTO jobs (kind, subject, payload, priority, run_at)
SELECT $1, $2, $3, $4, now() + make_interval(secs => $5)
WHERE $2 = '' OR NOT EXISTS (SELECT 1 FROM jobs WHERE kind = $1 AND subject = $2 AND status = 'failed')
ON CONFLICT (kind, subject) WHERE status IN ('queued', 'running')
DO UPDATE SET priority = EXCLUDED.priority, payload = EXCLUDED.payload
WHERE jobs.status = 'queued' AND jobs.priority < EXCLUDED.priority
RETURNING xmax = 0`,
		kind, subject, payload, options.Priority, options.Delay.Seconds()).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return inserted, err
}

const jobColumns = `id, kind, subject, payload, priority, status, attempts, run_at, progress, last_error, created_at, finished_at`

func scanJob(row pgx.Row) (Job, error) {
	var job Job
	err := row.Scan(&job.ID, &job.Kind, &job.Subject, &job.Payload, &job.Priority, &job.Status, &job.Attempts,
		&job.RunAt, &job.Progress, &job.LastError, &job.CreatedAt, &job.Finished)
	return job, err
}

// claim 取出一个到期的任务并标记为运行中。没有任务时返回 pgx.ErrNoRows。
// only 为 true 时只取 kinds 里的类型，为 false 时只取 kinds 以外的类型。
func (q *Queue) claim(ctx context.Context, lease time.Duration, kinds []string, only bool) (Job, error) {
	if kinds == nil {
		kinds = []string{}
	}
	return scanJob(q.pool.QueryRow(ctx, `UPDATE jobs SET status = 'running', attempts = attempts + 1,
locked_until = now() + make_interval(secs => $1)
WHERE id = (
  SELECT id FROM jobs WHERE status = 'queued' AND run_at <= now() AND (kind = ANY($2)) = $3
  ORDER BY priority DESC, run_at, id LIMIT 1 FOR UPDATE SKIP LOCKED
) RETURNING `+jobColumns, lease.Seconds(), kinds, only))
}

// postpone 把任务放回队列，过一会儿再做，这一次不算失败。
func (q *Queue) postpone(ctx context.Context, job Job, reason error, wait time.Duration) error {
	_, err := q.pool.Exec(ctx, `UPDATE jobs SET status = 'queued', attempts = attempts - 1, locked_until = NULL,
last_error = $2, run_at = now() + make_interval(secs => $3) WHERE id = $1`, job.ID, clip(reason.Error()), wait.Seconds())
	return err
}

func clip(message string) string {
	if len(message) > 1000 {
		return message[:1000]
	}
	return message
}

// finish 记录任务结果。失败且还有重试次数时重新排队，间隔逐次加长。
func (q *Queue) finish(ctx context.Context, job Job, runErr error) error {
	if runErr == nil || errors.Is(runErr, ErrSkip) {
		_, err := q.pool.Exec(ctx, `WITH done AS (
  UPDATE jobs SET status = 'done', finished_at = now(), locked_until = NULL, last_error = '' WHERE id = $1 RETURNING kind, subject
)
DELETE FROM jobs j USING done WHERE j.kind = done.kind AND j.subject = done.subject AND done.subject <> '' AND j.status = 'failed'`, job.ID)
		return err
	}
	message := clip(runErr.Error())
	if job.Attempts >= MaxAttempts {
		_, err := q.pool.Exec(ctx, `UPDATE jobs SET status = 'failed', finished_at = now(), locked_until = NULL, last_error = $2 WHERE id = $1`, job.ID, message)
		return err
	}
	backoff := retryDelays[min(job.Attempts, len(retryDelays))-1]
	_, err := q.pool.Exec(ctx, `UPDATE jobs SET status = 'queued', locked_until = NULL, last_error = $2,
run_at = now() + make_interval(secs => $3) WHERE id = $1`, job.ID, message, backoff.Seconds())
	return err
}

// SetProgress 更新任务进度，供页面轮询显示。
func (q *Queue) SetProgress(ctx context.Context, jobID int64, progress any) error {
	encoded, err := json.Marshal(progress)
	if err != nil {
		return err
	}
	_, err = q.pool.Exec(ctx, `UPDATE jobs SET progress = $2, locked_until = now() + make_interval(secs => $3) WHERE id = $1`, jobID, encoded, defaultLease.Seconds())
	return err
}

// Latest 返回某个对象最近一次的某种任务。
func (q *Queue) Latest(ctx context.Context, kind, subject string) (Job, bool, error) {
	job, err := scanJob(q.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE kind = $1 AND subject = $2 ORDER BY id DESC LIMIT 1`, kind, subject))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	return job, err == nil, err
}

// List 列出任务，status 为空表示全部。
func (q *Queue) List(ctx context.Context, status string, limit, offset int) ([]Job, error) {
	rows, err := q.pool.Query(ctx, `SELECT `+jobColumns+` FROM jobs WHERE ($1 = '' OR status = $1) ORDER BY id DESC LIMIT $2 OFFSET $3`, status, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

// Counts 返回各状态的任务数量。
func (q *Queue) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := q.pool.Query(ctx, `SELECT status, count(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}

// Backlog 返回这些类型的任务还有多少在排队，以及最早的一个是什么时候入队的（没有时为 nil）。
func (q *Queue) Backlog(ctx context.Context, kinds ...string) (count int, oldest *time.Time, err error) {
	err = q.pool.QueryRow(ctx, `SELECT count(*), min(created_at) FROM jobs WHERE status = 'queued' AND kind = ANY($1)`, kinds).Scan(&count, &oldest)
	return count, oldest, err
}

// Activity 是几种任务最近实际做成了多少。它从任务表里统计，不管任务在哪个进程里执行：
// 请求节奏（providers.Pacer）记在各个进程的内存里，网站进程看不到 worker 进程被限流了。
type Activity struct {
	Done      int        // 最近 1 小时做完的数量
	LastDone  *time.Time // 最近一次做完的时间；完成记录只留 3 天，更早的为 nil
	LastError string     // 排队中的任务里，最近一次出错（或因限流推迟）的原因
}

// Activity 返回这些类型的任务最近的执行情况。
func (q *Queue) Activity(ctx context.Context, kinds ...string) (Activity, error) {
	var activity Activity
	err := q.pool.QueryRow(ctx, `SELECT
  count(*) FILTER (WHERE status = 'done' AND finished_at > now() - interval '1 hour'),
  max(finished_at) FILTER (WHERE status = 'done'),
  COALESCE((SELECT last_error FROM jobs WHERE kind = ANY($1) AND status = 'queued' AND last_error <> ''
    ORDER BY run_at DESC LIMIT 1), '')
FROM jobs WHERE kind = ANY($1)`, kinds).Scan(&activity.Done, &activity.LastDone, &activity.LastError)
	return activity, err
}

// lastScheduled 返回每种周期任务最近一次入队的时间。
func (q *Queue) lastScheduled(ctx context.Context) (map[string]time.Time, error) {
	rows, err := q.pool.Query(ctx, `SELECT kind, max(created_at) FROM jobs WHERE subject = '' GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	last := map[string]time.Time{}
	for rows.Next() {
		var kind string
		var at time.Time
		if err := rows.Scan(&kind, &at); err != nil {
			return nil, err
		}
		last[kind] = at
	}
	return last, rows.Err()
}

// Retry 把失败的任务重新排队。id 为 0 表示重试所有失败任务。
func (q *Queue) Retry(ctx context.Context, id int64) (int64, error) {
	tag, err := q.pool.Exec(ctx, `UPDATE jobs j SET status = 'queued', attempts = 0, run_at = now(), finished_at = NULL
WHERE status = 'failed' AND ($1 = 0 OR id = $1)
AND NOT EXISTS (SELECT 1 FROM jobs o WHERE o.kind = j.kind AND o.subject = j.subject AND o.status IN ('queued', 'running'))`, id)
	return tag.RowsAffected(), err
}

// Housekeep 把租约过期的任务放回队列，并清理已结束的任务：成功的留 3 天，失败的留 14 天方便排查。
func (q *Queue) Housekeep(ctx context.Context) error {
	if _, err := q.pool.Exec(ctx, `UPDATE jobs SET status = 'queued', locked_until = NULL
WHERE status = 'running' AND locked_until < now()`); err != nil {
		return err
	}
	_, err := q.pool.Exec(ctx, `DELETE FROM jobs WHERE (status = 'done' AND finished_at < now() - interval '3 days')
OR (status = 'failed' AND finished_at < now() - interval '14 days')`)
	return err
}

// ---------- Worker ----------

// Lane 是一条独立的执行通道：只做 Kinds 里的任务，有自己的执行位。没放进任何通道的类型走默认通道。
// 分开之后，一类任务被对方限流、或者本身就慢，不会把别的任务堵在后面。
type Lane struct {
	Kinds []string
	Loops int
	// Long 是 Kinds 里一跑就很久的任务，Reserved 个执行位不接它们，留给其余的短任务。
	// 优先级不会打断正在跑的任务，不留位置的话，几个长任务就能把执行位全占住，
	// 有人在等的短任务也只能干等它们跑完。Reserved 至少给长任务留一个位置。
	Long     []string
	Reserved int
	// Hold 返回大于 0 表示这条通道现在先别取任务（比如对方让我们暂停），过这么久再看。
	// 任务留在队列里原样不动，不会被白白取出来失败一次。
	Hold func() time.Duration
	// Postpone 判断一次失败是不是"对方让我们慢一点"。是的话任务放回队列稍后再做，不算一次失败
	// （任务入队超过 3 天后不再这样处理，按普通失败算）。
	Postpone func(error) bool
}

// Worker 轮询队列并执行任务。
type Worker struct {
	queue       *Queue
	handlers    map[string]Handler
	schedules   []schedule
	lanes       []Lane
	concurrency int // 默认通道的执行位数量
	poll        time.Duration
	lease       time.Duration
	log         *slog.Logger
}

type schedule struct {
	kind     string
	every    time.Duration
	priority int
}

// NewWorker 创建 Worker。
func NewWorker(queue *Queue, concurrency int, log *slog.Logger) *Worker {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Worker{queue: queue, handlers: map[string]Handler{}, concurrency: concurrency, poll: 2 * time.Second, lease: defaultLease, log: log}
}

// Handle 注册一种任务的处理函数。
func (w *Worker) Handle(kind string, handler Handler) { w.handlers[kind] = handler }

// Lane 增加一条独立的执行通道。
func (w *Worker) Lane(lane Lane) { w.lanes = append(w.lanes, lane) }

// Every 注册周期任务：每隔 every 按 priority 入队一次（subject 为空）。上一次还没跑完时不会重复入队。
func (w *Worker) Every(kind string, every time.Duration, priority int) {
	w.schedules = append(w.schedules, schedule{kind: kind, every: every, priority: priority})
}

// Run 阻塞运行，直到 ctx 取消。
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	start := func(run func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run()
		}()
	}
	var taken []string
	for _, lane := range w.lanes {
		taken = append(taken, lane.Kinds...)
		loops := max(1, lane.Loops)
		reserved := min(lane.Reserved, loops-1)
		if len(lane.Long) == 0 {
			reserved = 0
		}
		short := slices.DeleteFunc(slices.Clone(lane.Kinds), func(kind string) bool { return slices.Contains(lane.Long, kind) })
		for index := range loops {
			kinds := lane.Kinds
			if index < reserved {
				kinds = short
			}
			start(func() { w.loop(ctx, kinds, true, lane.Hold) })
		}
	}
	for range w.concurrency {
		start(func() { w.loop(ctx, taken, false, nil) })
	}
	start(func() { w.scheduler(ctx) })
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context, kinds []string, only bool, hold func() time.Duration) {
	for ctx.Err() == nil {
		if hold != nil {
			if wait := hold(); wait > 0 {
				sleep(ctx, min(wait, time.Minute))
				continue
			}
		}
		worked, err := w.runOne(ctx, kinds, only)
		if err != nil && ctx.Err() == nil {
			w.log.Error("任务队列出错", "error", err)
		}
		if !worked {
			sleep(ctx, w.poll)
		}
	}
}

func sleep(ctx context.Context, wait time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

// laneOf 返回这种任务所在的通道，默认通道返回 nil。
func (w *Worker) laneOf(kind string) *Lane {
	for index := range w.lanes {
		for _, candidate := range w.lanes[index].Kinds {
			if candidate == kind {
				return &w.lanes[index]
			}
		}
	}
	return nil
}

// RunOne 执行一个到期的任务，不分通道。没有任务时返回 false。
func (w *Worker) RunOne(ctx context.Context) (bool, error) { return w.runOne(ctx, nil, false) }

func (w *Worker) runOne(ctx context.Context, kinds []string, only bool) (bool, error) {
	job, err := w.queue.claim(ctx, w.lease, kinds, only)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	handler, ok := w.handlers[job.Kind]
	var runErr error
	if !ok {
		runErr = fmt.Errorf("没有注册任务类型 %q", job.Kind)
		job.Attempts = MaxAttempts
	} else {
		runErr = safely(ctx, handler, job)
	}
	if lane := w.laneOf(job.Kind); runErr != nil && lane != nil && lane.Postpone != nil && lane.Postpone(runErr) &&
		time.Since(job.CreatedAt) < postponeLimit {
		wait := time.Minute
		if lane.Hold != nil {
			wait = max(wait, lane.Hold())
		}
		w.log.Info("任务推迟", "kind", job.Kind, "subject", job.Subject, "wait", wait.Round(time.Second), "reason", runErr)
		return true, w.queue.postpone(context.WithoutCancel(ctx), job, runErr, wait)
	}
	if runErr != nil && !errors.Is(runErr, ErrSkip) {
		w.log.Warn("任务失败", "kind", job.Kind, "subject", job.Subject, "attempt", job.Attempts, "error", runErr)
	}
	return true, w.queue.finish(context.WithoutCancel(ctx), job, runErr)
}

// safely 执行处理函数，把 panic 变成普通错误，避免一个任务拖垮整个进程。
func safely(ctx context.Context, handler Handler, job Job) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("任务 panic: %v", recovered)
		}
	}()
	return handler(ctx, job)
}

func (w *Worker) scheduler(ctx context.Context) {
	// 从数据库里读上一次入队的时间，而不是从零开始记：否则每次重启都会把所有周期任务立刻再跑一遍。
	last, err := w.queue.lastScheduled(ctx)
	if err != nil {
		w.log.Error("读取周期任务的上次时间失败", "error", err)
		last = map[string]time.Time{}
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		now := time.Now()
		for _, item := range w.schedules {
			if now.Sub(last[item.kind]) < item.every {
				continue
			}
			last[item.kind] = now
			if _, err := w.queue.Enqueue(ctx, item.kind, "", Options{Priority: item.priority}); err != nil && ctx.Err() == nil {
				w.log.Error("周期任务入队失败", "kind", item.kind, "error", err)
			}
		}
		if err := w.queue.Housekeep(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("任务队列清理失败", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

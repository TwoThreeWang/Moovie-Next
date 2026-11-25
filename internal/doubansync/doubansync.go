// Package doubansync 把用户在豆瓣上的想看和看过同步到本站。
package doubansync

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"moovie/internal/auth"
	"moovie/internal/db"
	"moovie/internal/ingest"
	"moovie/internal/jobs"
	"moovie/internal/library"
	"moovie/internal/media"
	"moovie/internal/providers"
)

// 任务类型。
const (
	KindSync  = "douban.sync"       // 同步一个用户，subject 是用户 ID
	KindDaily = "douban.sync.daily" // 每天给所有绑定了豆瓣的用户各排一次增量同步
)

const (
	pageSize = 50
	// 为什么这样排见 ingest.PriorityViewing 那一组的注释。
	manualPriority = ingest.PriorityManual    // 用户点"同步"，看着进度条在等
	dailyPriority  = ingest.PriorityDailySync // 每天自动的那次：没人等，但不做的话新标记就不会出现
	importPriority = ingest.PriorityImport    // 同步时新发现的作品，补全资料的优先级
)

// payload 是同步任务的参数。
type payload struct {
	// Full 为 true 时把用户的全部标记从头到尾读一遍；否则读到"整页都已经导入过"就停。
	Full bool `json:"full"`
}

// Interests 是读取豆瓣用户标记的能力，由 providers.Douban 实现。
type Interests interface {
	Interests(ctx context.Context, userID, status string, start, count int) ([]providers.DoubanInterest, int, error)
	RecentInterests(ctx context.Context, userID string) ([]providers.DoubanRecent, error)
}

// Catalog 把豆瓣条目收进作品库，由 ingest.Service 实现。
type Catalog interface {
	EnsureDouban(ctx context.Context, item providers.DoubanItem, chartType string, priority int) (int64, error)
}

// Progress 是同步任务的进度，页面轮询时显示。
type Progress struct {
	Total     int `json:"total"`
	Processed int `json:"processed"`
	Imported  int `json:"imported"`
	// Failed 是读到了、但没能收进作品库的标记数（数据库出错之类，下次还能成功的）。
	// 有失败的同步之后，下一次同步要完整地读一遍，见 Sync。
	Failed int `json:"failed"`
}

// Service 执行豆瓣同步。
type Service struct {
	Pool    *db.Pool
	Users   *auth.Store
	Douban  Interests
	Catalog Catalog
	Queue   *jobs.Queue
}

var userIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,40}$`)

// ErrInvalidUser 表示豆瓣 ID 不合法，或者这个用户不存在、资料不公开。
var ErrInvalidUser = errors.New("豆瓣 ID 不正确，或者这个账号的标记没有公开")

// Bind 绑定豆瓣账号。会先读一条标记，确认这个账号存在并且公开。
func (s *Service) Bind(ctx context.Context, userID int64, doubanUserID string) error {
	if !userIDPattern.MatchString(doubanUserID) {
		return ErrInvalidUser
	}
	if _, _, err := s.Douban.Interests(ctx, doubanUserID, "done", 0, 1); err != nil {
		return ErrInvalidUser
	}
	if err := s.Users.Update(ctx, userID, "douban_user_id", doubanUserID); err != nil {
		return err
	}
	return s.forgetSyncs(ctx, userID)
}

// Unbind 解除绑定。已经同步过来的标记保留。
func (s *Service) Unbind(ctx context.Context, userID int64) error {
	if err := s.Users.Update(ctx, userID, "douban_user_id", ""); err != nil {
		return err
	}
	return s.forgetSyncs(ctx, userID)
}

// forgetSyncs 删掉这个用户已完成的同步记录。增量同步拿"上一次同步是什么时候"去对照 RSS，
// 换了豆瓣账号以后，旧账号的同步时间对新账号没有意义，不删的话新账号较早的标记会被当成已经同步过。
func (s *Service) forgetSyncs(ctx context.Context, userID int64) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM jobs WHERE kind = $1 AND subject = $2 AND status = 'done'`, KindSync, strconv.FormatInt(userID, 10))
	return err
}

// Start 把一次完整同步放进队列（用户自己点的）。已经在排队或运行时返回 false；
// 如果排着的是每天自动的那次，它会被提前并换成完整同步。
func (s *Service) Start(ctx context.Context, userID int64) (bool, error) {
	return s.Queue.Enqueue(ctx, KindSync, strconv.FormatInt(userID, 10), jobs.Options{Payload: payload{Full: true}, Priority: manualPriority})
}

// StartAll 给所有绑定了豆瓣的用户各排一次增量同步。豆瓣的标记按时间倒序排列，
// 没有新标记的用户只要读第一页就结束，每人每天两次请求（看过、想看各一次）。
// 最近由浏览器插件同步过的用户跳过（见 extSyncFresh）。
func (s *Service) StartAll(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM users WHERE douban_user_id <> '' AND restriction <> 'ban'
AND (douban_ext_synced_at IS NULL OR douban_ext_synced_at < now() - make_interval(secs => $1)) ORDER BY id`, extSyncFresh.Seconds())
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.Queue.Enqueue(ctx, KindSync, strconv.FormatInt(id, 10), jobs.Options{Payload: payload{}, Priority: dailyPriority}); err != nil {
			return err
		}
	}
	return nil
}

// Register 把任务处理函数注册到 Worker。
func (s *Service) Register(worker *jobs.Worker) {
	worker.Handle(KindSync, func(ctx context.Context, job jobs.Job) error {
		userID, err := strconv.ParseInt(job.Subject, 10, 64)
		if err != nil {
			return jobs.ErrSkip
		}
		var options payload
		json.Unmarshal(job.Payload, &options)
		// 这个任务以前跑过一部分（中途出错或被限流，正在重试）：前面几页已经导入，
		// "读到整页都已导入就停"会停在第一页，没读到的那些就漏了，所以重试时完整地读。
		var earlier Progress
		json.Unmarshal(job.Progress, &earlier)
		full := options.Full || earlier.Processed > 0
		return s.Sync(ctx, userID, full, func(progress Progress) { s.Queue.SetProgress(ctx, job.ID, progress) })
	})
	worker.Handle(KindDaily, func(ctx context.Context, _ jobs.Job) error { return s.StartAll(ctx) })
	worker.Every(KindDaily, 24*time.Hour, 0)
}

// Sync 同步一个用户的豆瓣标记。本站已有的标记不会被覆盖，只有"想看"会升级成"看过"。
//
// full 为 false 时是增量同步：豆瓣的标记按时间倒序排列，读到一整页都已经导入过，后面的就不用再读了。
// 增量同步先看用户的 RSS（见 recentStatuses），上一次同步之后没有新的看过或想看，就一页都不用读。
// 第一次同步、或者上一次中途失败过，要用完整同步，否则后面没读到的那些会被漏掉。
func (s *Service) Sync(ctx context.Context, userID int64, full bool, report func(Progress)) error {
	user, err := s.Users.ByID(ctx, userID)
	if errors.Is(err, auth.ErrNotFound) || (err == nil && user.DoubanUserID == "") {
		return jobs.ErrSkip
	}
	if err != nil {
		return err
	}
	progress := Progress{}
	wanted := map[string]bool{"done": true, "mark": true}
	if !full {
		last, lastFailed, err := s.lastSync(ctx, userID)
		if err != nil {
			return err
		}
		// 上一次同步有没导入成功的：那几条在列表里的位置可能很靠后，"读到整页都已导入就停"读不到它们，
		// RSS 里它们也早于上一次同步、会被当成处理过了。所以完整地读一遍，直到有一次同步没有失败。
		full = lastFailed > 0
		if !full && last != nil {
			recent, known, err := s.recentStatuses(ctx, *last, user.DoubanUserID)
			if err != nil {
				return err
			}
			if known {
				wanted = recent
			}
		}
	}
	for _, pass := range []struct{ doubanStatus, status string }{{"done", library.StatusWatched}, {"mark", library.StatusWish}} {
		if !wanted[pass.doubanStatus] {
			continue
		}
		for start := 0; start < 20000; start += pageSize {
			interests, total, err := s.Douban.Interests(ctx, user.DoubanUserID, pass.doubanStatus, start, pageSize)
			if err != nil {
				return err
			}
			if start == 0 {
				progress.Total += total
			}
			page, changed, err := s.importPage(ctx, userID, pass.status, interests)
			if err != nil {
				return err
			}
			progress.Processed += page.Processed
			progress.Imported += page.Imported
			progress.Failed += page.Failed
			if report != nil {
				report(progress)
			}
			if len(interests) < pageSize || start+pageSize >= total || (!full && changed == 0) {
				break
			}
		}
	}
	return nil
}

// importPage 导入一页标记。changed 是这一页里新导入的加上没能处理的：为 0 说明这一页都已经导入过，增量同步可以停了。
func (s *Service) importPage(ctx context.Context, userID int64, status string, interests []providers.DoubanInterest) (progress Progress, changed int, err error) {
	for _, interest := range interests {
		progress.Processed++
		if !media.ValidDoubanID(interest.Item.ID) {
			continue // 不是合法的条目 ID，再试多少次也收不进来，不算失败
		}
		mediaID, err := s.Catalog.EnsureDouban(ctx, interest.Item, "", importPriority)
		if err != nil {
			progress.Failed++
			changed++ // 没能处理的也算"这一页还没读完整"，不能据此提前结束
			continue
		}
		imported, err := s.importOne(ctx, userID, mediaID, status, interest)
		if err != nil {
			return progress, changed, err
		}
		if imported {
			progress.Imported++
			changed++
		}
	}
	return progress, changed, nil
}

// ---------- 浏览器插件 ----------
//
// 装了插件的用户，标记由插件在他自己的浏览器里、用他自己的豆瓣登录状态去读，一页一页交给服务器导入（ImportPage），
// 不占服务器的豆瓣通道，标记不公开的账号也能同步。上面服务器自己读的那一套保持不变，没装插件的用户照常用。

// extSyncFresh 是插件同步过之后，服务器每天的自动同步跳过这个用户多久。插件每天会自己同步一次，
// 留三天余量给"这几天没开电脑"；插件卸载了，过三天服务器自动接手。
const extSyncFresh = 3 * 24 * time.Hour

// BindFromExtension 绑定插件读到的豆瓣账号（插件所在浏览器登录着的那个）。不用像 Bind 那样先去豆瓣确认账号公开：
// 插件用登录状态读，不公开的也能读。换了账号时和 Bind 一样清掉旧账号的同步记录。
func (s *Service) BindFromExtension(ctx context.Context, userID int64, doubanUserID string) error {
	if !userIDPattern.MatchString(doubanUserID) {
		return ErrInvalidUser
	}
	user, err := s.Users.ByID(ctx, userID)
	if err != nil {
		return err
	}
	if user.DoubanUserID == doubanUserID {
		return nil
	}
	if err := s.Users.Update(ctx, userID, "douban_user_id", doubanUserID); err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE users SET douban_ext_synced_at = NULL WHERE id = $1`, userID); err != nil {
		return err
	}
	return s.forgetSyncs(ctx, userID)
}

// ImportPage 导入插件交来的一页标记。status 是豆瓣的 done（看过）或 mark（想看）。
// 返回值里 changed 为 0 表示这一页都已经导入过，插件做增量同步时读到这里就可以停。
func (s *Service) ImportPage(ctx context.Context, userID int64, status string, interests []providers.DoubanInterest) (progress Progress, changed int, err error) {
	local := map[string]string{"done": library.StatusWatched, "mark": library.StatusWish}[status]
	if local == "" {
		return progress, 0, ErrInvalidUser
	}
	return s.importPage(ctx, userID, local, interests)
}

// ExtensionSynced 记下插件完整同步了一次（没有失败的条目），之后三天服务器不再替这个用户去豆瓣读。
func (s *Service) ExtensionSynced(ctx context.Context, userID int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE users SET douban_ext_synced_at = now() WHERE id = $1`, userID)
	return err
}

// ExtensionSyncedAt 返回插件最近一次完整同步的时间，没有时为 nil。
func (s *Service) ExtensionSyncedAt(ctx context.Context, userID int64) (*time.Time, error) {
	var at *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT douban_ext_synced_at FROM users WHERE id = $1`, userID).Scan(&at)
	return at, err
}

// feedSlack 是对照 RSS 时间时留的余量，抵消豆瓣和本机的时钟差。
const feedSlack = time.Hour

// lastSync 返回最近一次成功的同步是什么时候开始的、有几条没导入成功。没有成功同步过时 last 为 nil。
//
// 时间取任务的 run_at（排定的开始时间，不晚于真正开始读的时间），这样同步途中新标记的也会被下一次看到。
func (s *Service) lastSync(ctx context.Context, userID int64) (last *time.Time, failed int, err error) {
	var progress []byte
	err = s.Pool.QueryRow(ctx, `SELECT run_at, progress FROM jobs WHERE kind = $1 AND subject = $2 AND status = 'done'
ORDER BY run_at DESC LIMIT 1`, KindSync, strconv.FormatInt(userID, 10)).Scan(&last, &progress)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var previous Progress
	json.Unmarshal(progress, &previous)
	return last, previous.Failed, nil
}

// recentStatuses 用 RSS 判断上一次同步（last）之后，哪些状态（done 看过、mark 想看）有新的标记。
// known 为 false 表示 RSS 说明不了问题，调用方照原来的办法去翻标记列表：
// RSS 读不到或者是空的、有动态的时间解析不出来，
// 或者 RSS 里最早的一条也比上一次同步晚（RSS 只有最近十来条，更早的动态可能被挤掉了）。
// 调用方要保证上一次同步没有失败的条目，否则早于 last 的那几条会被当成处理过了。
func (s *Service) recentStatuses(ctx context.Context, last time.Time, doubanUserID string) (statuses map[string]bool, known bool, err error) {
	recent, err := s.Douban.RecentInterests(ctx, doubanUserID)
	if errors.Is(err, providers.ErrThrottled) {
		return nil, false, err // 被限流了，翻标记列表也一样会被拒绝，任务稍后重试
	}
	if err != nil || len(recent) == 0 {
		return nil, false, nil
	}
	cutoff := last.Add(-feedSlack)
	statuses = map[string]bool{}
	covered := false
	for _, item := range recent {
		if item.At.IsZero() {
			return nil, false, nil
		}
		if !item.At.After(cutoff) {
			covered = true
			continue
		}
		if item.Status != "" {
			statuses[item.Status] = true
		}
	}
	if !covered {
		return nil, false, nil
	}
	return statuses, true, nil
}

func (s *Service) importOne(ctx context.Context, userID, mediaID int64, status string, interest providers.DoubanInterest) (bool, error) {
	at := interest.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	rating := min(5, max(0, interest.Rating))
	comment := interest.Comment
	if status == library.StatusWish {
		rating, comment = 0, ""
	}
	if runes := []rune(comment); len(runes) > 2000 {
		comment = string(runes[:2000])
	}
	tag, err := s.Pool.Exec(ctx, `INSERT INTO user_movies (user_id, media_id, status, rating, comment, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6)
ON CONFLICT (user_id, media_id) DO UPDATE SET status = 'watched', rating = EXCLUDED.rating,
comment = EXCLUDED.comment, updated_at = EXCLUDED.updated_at
WHERE user_movies.status = 'wish' AND EXCLUDED.status = 'watched'`, userID, mediaID, status, rating, comment, at)
	return tag.RowsAffected() == 1, err
}

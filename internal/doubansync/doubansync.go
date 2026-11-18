// Package doubansync 把用户在豆瓣上的想看和看过同步到本站。
package doubansync

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"moovie/internal/auth"
	"moovie/internal/db"
	"moovie/internal/jobs"
	"moovie/internal/library"
	"moovie/internal/providers"
)

// 任务类型。
const (
	KindSync  = "douban.sync"       // 同步一个用户，subject 是用户 ID
	KindDaily = "douban.sync.daily" // 每天给所有绑定了豆瓣的用户各排一次增量同步
)

const (
	pageSize = 50
	// 用户点"同步"是有人在等，排在前面；每天自动的那次没人等，排在最后。
	manualPriority = 5
	dailyPriority  = -2
	// 同步时新发现的作品，补全资料的优先级（对应 ingest.PriorityImport）。
	importPriority = 0
)

// payload 是同步任务的参数。
type payload struct {
	// Full 为 true 时把用户的全部标记从头到尾读一遍；否则读到"整页都已经导入过"就停。
	Full bool `json:"full"`
}

// Interests 是读取豆瓣用户标记的能力，由 providers.Douban 实现。
type Interests interface {
	Interests(ctx context.Context, userID, status string, start, count int) ([]providers.DoubanInterest, int, error)
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
	return s.Users.Update(ctx, userID, "douban_user_id", doubanUserID)
}

// Unbind 解除绑定。已经同步过来的标记保留。
func (s *Service) Unbind(ctx context.Context, userID int64) error {
	return s.Users.Update(ctx, userID, "douban_user_id", "")
}

// Start 把一次完整同步放进队列（用户自己点的）。已经在排队或运行时返回 false；
// 如果排着的是每天自动的那次，它会被提前并换成完整同步。
func (s *Service) Start(ctx context.Context, userID int64) (bool, error) {
	return s.Queue.Enqueue(ctx, KindSync, strconv.FormatInt(userID, 10), jobs.Options{Payload: payload{Full: true}, Priority: manualPriority})
}

// StartAll 给所有绑定了豆瓣的用户各排一次增量同步。豆瓣的标记按时间倒序排列，
// 没有新标记的用户只要读第一页就结束，每人每天两次请求（看过、想看各一次）。
func (s *Service) StartAll(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM users WHERE douban_user_id <> '' AND restriction <> 'ban' ORDER BY id`)
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
		return s.Sync(ctx, userID, options.Full, func(progress Progress) { s.Queue.SetProgress(ctx, job.ID, progress) })
	})
	worker.Handle(KindDaily, func(ctx context.Context, _ jobs.Job) error { return s.StartAll(ctx) })
	worker.Every(KindDaily, 24*time.Hour)
}

// Sync 同步一个用户的豆瓣标记。本站已有的标记不会被覆盖，只有"想看"会升级成"看过"。
//
// full 为 false 时是增量同步：豆瓣的标记按时间倒序排列，读到一整页都已经导入过，后面的就不用再读了。
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
	for _, pass := range []struct{ doubanStatus, status string }{{"done", library.StatusWatched}, {"mark", library.StatusWish}} {
		for start := 0; start < 20000; start += pageSize {
			interests, total, err := s.Douban.Interests(ctx, user.DoubanUserID, pass.doubanStatus, start, pageSize)
			if err != nil {
				return err
			}
			if start == 0 {
				progress.Total += total
			}
			changed := 0
			for _, interest := range interests {
				progress.Processed++
				mediaID, err := s.Catalog.EnsureDouban(ctx, interest.Item, "", importPriority)
				if err != nil {
					changed++ // 没能处理的也算"这一页还没读完整"，不能据此提前结束
					continue
				}
				imported, err := s.importOne(ctx, userID, mediaID, pass.status, interest)
				if err != nil {
					return err
				}
				if imported {
					progress.Imported++
					changed++
				}
			}
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

// Package feedback 管理用户反馈和版权投诉。
package feedback

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"

	"moovie/internal/db"
)

// Feedback 是一条反馈。
type Feedback struct {
	ID        int64
	UserID    *int64
	Username  string
	Kind      string
	Content   string
	PageURL   string
	Status    string
	Reply     string
	RepliedAt *time.Time
	CreatedAt time.Time
}

// Kinds 是反馈的类型和中文名。dmca 是版权投诉，对应"通知—删除"流程。
var Kinds = []struct{ Key, Label string }{
	{"bug", "问题反馈"}, {"request", "求片"}, {"suggestion", "功能建议"}, {"dmca", "版权投诉"},
}

// KindLabel 返回类型的中文名。
func KindLabel(kind string) string {
	for _, item := range Kinds {
		if item.Key == kind {
			return item.Label
		}
	}
	return ""
}

// KindLabel 返回这条反馈类型的中文名。
func (f Feedback) KindLabel() string { return KindLabel(f.Kind) }

// ErrInvalid 表示反馈内容不合法。
var ErrInvalid = errors.New("反馈内容不合法")

// ErrNotFound 表示反馈不存在。
var ErrNotFound = errors.New("反馈不存在")

// Store 读写 feedbacks 表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

// Create 提交一条反馈。
func (s *Store) Create(ctx context.Context, userID int64, kind, content, pageURL string) error {
	content = strings.TrimSpace(content)
	if KindLabel(kind) == "" || content == "" || len([]rune(content)) > 5000 || len(pageURL) > 500 {
		return ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO feedbacks (user_id, kind, content, page_url) VALUES (NULLIF($1, 0), $2, $3, $4)`,
		userID, kind, content, pageURL)
	return err
}

// List 列出反馈。userID 非 0 时只看这个用户的；status 非空时按状态筛选。
func (s *Store) List(ctx context.Context, userID int64, status string, limit, offset int) ([]Feedback, error) {
	rows, err := s.pool.Query(ctx, `SELECT f.id, f.user_id, COALESCE(u.username, ''), f.kind, f.content, f.page_url, f.status, f.reply, f.replied_at, f.created_at
FROM feedbacks f LEFT JOIN users u ON u.id = f.user_id
WHERE ($1 = 0 OR f.user_id = $1) AND ($2 = '' OR f.status = $2)
ORDER BY f.id DESC LIMIT $3 OFFSET $4`, userID, status, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Feedback{}
	for rows.Next() {
		var f Feedback
		if err := rows.Scan(&f.ID, &f.UserID, &f.Username, &f.Kind, &f.Content, &f.PageURL, &f.Status, &f.Reply, &f.RepliedAt, &f.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, f)
	}
	return list, rows.Err()
}

// Respond 由管理员设置状态和回复。返回提交人（匿名为 0），以及回复内容是否有新变化（用来决定要不要通知提交人）。
func (s *Store) Respond(ctx context.Context, id int64, status, reply string) (userID int64, replied bool, err error) {
	if status != "open" && status != "resolved" && status != "rejected" {
		return 0, false, ErrInvalid
	}
	reply = strings.TrimSpace(reply)
	var previous string
	err = s.pool.QueryRow(ctx, `WITH old AS (SELECT id, reply FROM feedbacks WHERE id = $1 FOR UPDATE)
UPDATE feedbacks f SET status = $2, reply = $3, replied_at = CASE WHEN $3 <> '' THEN now() ELSE f.replied_at END
FROM old WHERE f.id = old.id RETURNING COALESCE(f.user_id, 0), old.reply`, id, status, reply).Scan(&userID, &previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	return userID, err == nil && reply != "" && reply != previous, err
}

// OpenCount 返回待处理的反馈数量。
func (s *Store) OpenCount(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM feedbacks WHERE status = 'open'`).Scan(&count)
	return count, err
}

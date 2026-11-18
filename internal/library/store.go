// Package library 管理用户的想看、看过、评分和短评。
package library

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"moovie/internal/db"
	"moovie/internal/media"
)

// 标记状态。
const (
	StatusWish    = "wish"
	StatusWatched = "watched"
)

// Entry 是用户对一部作品的标记。
type Entry struct {
	ID        int64
	UserID    int64
	MediaID   int64
	Status    string
	Rating    int
	Comment   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Item 是列表里的一项：标记加上作品卡片。
type Item struct {
	Entry Entry
	Card  media.Card
}

// Store 读写 user_movies 表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

// ErrInvalid 表示标记内容不合法。
var ErrInvalid = errors.New("标记内容不合法")

const entryColumns = `um.id, um.user_id, um.media_id, um.status, um.rating, um.comment, um.created_at, um.updated_at`

// Get 返回用户对某部作品的标记，没有标记时第二个返回值为 false。
func (s *Store) Get(ctx context.Context, userID, mediaID int64) (Entry, bool, error) {
	var e Entry
	err := s.pool.QueryRow(ctx, `SELECT `+entryColumns+` FROM user_movies um WHERE um.user_id = $1 AND um.media_id = $2`, userID, mediaID).
		Scan(&e.ID, &e.UserID, &e.MediaID, &e.Status, &e.Rating, &e.Comment, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, false, nil
	}
	return e, err == nil, err
}

// Save 新建或更新标记。标成想看时会清掉评分和短评。
func (s *Store) Save(ctx context.Context, userID, mediaID int64, status string, rating int, comment string) (Entry, error) {
	comment = strings.TrimSpace(comment)
	if (status != StatusWish && status != StatusWatched) || rating < 0 || rating > 5 || len([]rune(comment)) > 2000 {
		return Entry{}, ErrInvalid
	}
	if status == StatusWish {
		rating, comment = 0, ""
	}
	var e Entry
	err := s.pool.QueryRow(ctx, `INSERT INTO user_movies AS um (user_id, media_id, status, rating, comment)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, media_id) DO UPDATE SET status = EXCLUDED.status, rating = EXCLUDED.rating,
comment = EXCLUDED.comment, updated_at = now()
RETURNING `+entryColumns, userID, mediaID, status, rating, comment).
		Scan(&e.ID, &e.UserID, &e.MediaID, &e.Status, &e.Rating, &e.Comment, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

// Remove 删除标记。
func (s *Store) Remove(ctx context.Context, userID, mediaID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_movies WHERE user_id = $1 AND media_id = $2`, userID, mediaID)
	return err
}

// List 分页列出用户某种状态的标记，最近更新的在前。
func (s *Store) List(ctx context.Context, userID int64, status string, limit, offset int) ([]Item, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+entryColumns+`, m.id, m.media_type, m.title, m.year, m.poster,
CASE WHEN m.rating_douban > 0 THEN m.rating_douban ELSE m.rating_tmdb END, m.season, m.episode_count
FROM user_movies um JOIN media m ON m.id = um.media_id
WHERE um.user_id = $1 AND um.status = $2 AND NOT m.hidden
ORDER BY um.updated_at DESC, um.id DESC LIMIT $3 OFFSET $4`, userID, status, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var item Item
		e, c := &item.Entry, &item.Card
		if err := rows.Scan(&e.ID, &e.UserID, &e.MediaID, &e.Status, &e.Rating, &e.Comment, &e.CreatedAt, &e.UpdatedAt,
			&c.ID, &c.Type, &c.Title, &c.Year, &c.Poster, &c.Rating, &c.Season, &c.EpisodeCount); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// Counts 返回用户想看和看过的数量。
func (s *Store) Counts(ctx context.Context, userID int64) (wish, watched int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'wish'), count(*) FILTER (WHERE status = 'watched')
FROM user_movies WHERE user_id = $1`, userID).Scan(&wish, &watched)
	return
}

// StatusMap 返回用户对一批作品的标记状态，用来在卡片上显示"已想看 / 已看过"。
func (s *Store) StatusMap(ctx context.Context, userID int64, mediaIDs []int64) (map[int64]string, error) {
	result := map[int64]string{}
	if userID == 0 || len(mediaIDs) == 0 {
		return result, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT media_id, status FROM user_movies WHERE user_id = $1 AND media_id = ANY($2)`, userID, mediaIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		result[id] = status
	}
	return result, rows.Err()
}

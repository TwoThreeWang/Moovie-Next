// Package social 管理短评的点赞和回复、通知、关注，以及短评动态。
package social

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"moovie/internal/db"
)

// Comment 是一条短评，带作者、作品和互动数据。
type Comment struct {
	ID          int64 // user_movies.id
	UserID      int64
	Username    string
	Avatar      string
	UserPublic  bool
	MediaID     int64
	MediaTitle  string
	MediaPoster string
	MediaYear   int
	Rating      int
	Text        string
	UpdatedAt   time.Time
	Likes       int
	Replies     int
	Liked       bool // 当前访客是否点过赞
}

// Reply 是短评下的一条回复。
type Reply struct {
	ID        int64
	CommentID int64
	UserID    int64
	Username  string
	Avatar    string
	Content   string
	CreatedAt time.Time
}

// Notification 是一条通知。
type Notification struct {
	ID           int64
	Kind         string
	ActorID      int64
	ActorName    string
	ActorAvatar  string
	MediaID      int64
	MediaTitle   string
	Excerpt      string // 回复内容、被点赞的短评、管理员的回复，或被删掉的内容
	Reason       string // 内容被删除的原因
	FeedbackID   int64
	CollectionID int64
	CreatedAt    time.Time
	Read         bool
}

// Notice 是要发出的一条通知（反馈被回复、内容被删除这类由管理员触发的通知）。
type Notice struct {
	UserID     int64
	ActorID    int64
	Kind       string
	MediaID    int64
	FeedbackID int64
	Body       string
	Reason     string
}

// DefaultRemoveReason 是管理员没写原因时的删除原因。
const DefaultRemoveReason = "违反社区规范"

func excerpt(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return string(runes)
}

func notify(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, n Notice) error {
	if n.UserID == n.ActorID || n.UserID == 0 {
		return nil
	}
	_, err := q.Exec(ctx, `INSERT INTO notifications (user_id, actor_id, kind, media_id, feedback_id, body, reason)
VALUES ($1, $2, $3, NULLIF($4, 0), NULLIF($5, 0), $6, $7)`, n.UserID, n.ActorID, n.Kind, n.MediaID, n.FeedbackID, excerpt(n.Body, 300), excerpt(n.Reason, 100))
	return err
}

// Notify 发一条通知。发给自己的不发。
func (s *Store) Notify(ctx context.Context, n Notice) error { return notify(ctx, s.pool, n) }

// RemoveComment 由管理员删除一条短评：清空短评文字（评分和看过记录保留），删掉它的回复、点赞和相关通知，再通知作者。
func (s *Store) RemoveComment(ctx context.Context, adminID, commentID int64, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = DefaultRemoveReason
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var ownerID, mediaID int64
		var text string
		err := tx.QueryRow(ctx, `SELECT user_id, media_id, comment FROM user_movies WHERE id = $1 AND comment <> '' FOR UPDATE`, commentID).
			Scan(&ownerID, &mediaID, &text)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		for _, statement := range []string{
			`UPDATE user_movies SET comment = '' WHERE id = $1`,
			`DELETE FROM comment_replies WHERE user_movie_id = $1`,
			`DELETE FROM comment_likes WHERE user_movie_id = $1`,
			`DELETE FROM notifications WHERE user_movie_id = $1`,
		} {
			if _, err := tx.Exec(ctx, statement, commentID); err != nil {
				return err
			}
		}
		return notify(ctx, tx, Notice{UserID: ownerID, ActorID: adminID, Kind: "removed", MediaID: mediaID, Body: text, Reason: reason})
	})
}

// Profile 是用户的公开资料。
type Profile struct {
	ID        int64
	Username  string
	Avatar    string
	IsPublic  bool
	CreatedAt time.Time
	Followers int
	Following int
	Followed  bool // 当前访客是否关注了这个用户
}

// ErrNotFound 表示目标不存在。
var ErrNotFound = errors.New("内容不存在")

// Store 读写社交相关的表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

const commentSelect = `SELECT um.id, u.id, u.username, u.avatar, u.is_public, m.id, m.title, m.poster, m.year,
um.rating, um.comment, um.updated_at,
(SELECT count(*) FROM comment_likes l WHERE l.user_movie_id = um.id),
(SELECT count(*) FROM comment_replies r WHERE r.user_movie_id = um.id),
EXISTS (SELECT 1 FROM comment_likes l WHERE l.user_movie_id = um.id AND l.user_id = $1)
FROM user_movies um JOIN users u ON u.id = um.user_id JOIN media m ON m.id = um.media_id
WHERE um.comment <> '' AND NOT m.hidden`

func scanComments(rows pgx.Rows, err error) ([]Comment, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	comments := []Comment{}
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.UserID, &c.Username, &c.Avatar, &c.UserPublic, &c.MediaID, &c.MediaTitle, &c.MediaPoster,
			&c.MediaYear, &c.Rating, &c.Text, &c.UpdatedAt, &c.Likes, &c.Replies, &c.Liked); err != nil {
			return nil, err
		}
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// MediaComments 返回一部作品下的短评，点赞多的在前。
func (s *Store) MediaComments(ctx context.Context, viewerID, mediaID int64, limit, offset int) ([]Comment, error) {
	return scanComments(s.pool.Query(ctx, commentSelect+` AND um.media_id = $2
ORDER BY 13 DESC, um.updated_at DESC LIMIT $3 OFFSET $4`, viewerID, mediaID, limit, offset))
}

// Feed 返回全站最新的短评。onlyFollowing 为 true 时只看关注的人。
func (s *Store) Feed(ctx context.Context, viewerID int64, onlyFollowing bool, limit, offset int) ([]Comment, error) {
	return scanComments(s.pool.Query(ctx, commentSelect+` AND (NOT $2 OR um.user_id IN (SELECT followee_id FROM user_follows WHERE follower_id = $1))
ORDER BY um.updated_at DESC, um.id DESC LIMIT $3 OFFSET $4`, viewerID, onlyFollowing, limit, offset))
}

// UserComments 返回某个用户写过的短评。
func (s *Store) UserComments(ctx context.Context, viewerID, userID int64, limit, offset int) ([]Comment, error) {
	return scanComments(s.pool.Query(ctx, commentSelect+` AND um.user_id = $2
ORDER BY um.updated_at DESC, um.id DESC LIMIT $3 OFFSET $4`, viewerID, userID, limit, offset))
}

// Comment 返回一条短评。
func (s *Store) Comment(ctx context.Context, viewerID, commentID int64) (Comment, error) {
	comments, err := scanComments(s.pool.Query(ctx, commentSelect+` AND um.id = $2`, viewerID, commentID))
	if err != nil {
		return Comment{}, err
	}
	if len(comments) == 0 {
		return Comment{}, ErrNotFound
	}
	return comments[0], nil
}

// ToggleLike 点赞或取消点赞，返回操作后的状态。点赞时给短评作者发通知（自己赞自己不发）。
func (s *Store) ToggleLike(ctx context.Context, userID, commentID int64) (liked bool, err error) {
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var ownerID int64
		if err := tx.QueryRow(ctx, `SELECT user_id FROM user_movies WHERE id = $1 AND comment <> ''`, commentID).Scan(&ownerID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM comment_likes WHERE user_movie_id = $1 AND user_id = $2`, commentID, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			_, err = tx.Exec(ctx, `DELETE FROM notifications WHERE kind = 'like' AND user_movie_id = $1 AND actor_id = $2`, commentID, userID)
			return err
		}
		liked = true
		if _, err := tx.Exec(ctx, `INSERT INTO comment_likes (user_movie_id, user_id) VALUES ($1, $2)`, commentID, userID); err != nil {
			return err
		}
		if ownerID == userID {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO notifications (user_id, actor_id, kind, user_movie_id) VALUES ($1, $2, 'like', $3)
ON CONFLICT DO NOTHING`, ownerID, userID, commentID)
		return err
	})
	return liked, err
}

// Replies 返回一条短评下的回复，按时间顺序。
func (s *Store) Replies(ctx context.Context, commentID int64) ([]Reply, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.id, r.user_movie_id, u.id, u.username, u.avatar, r.content, r.created_at
FROM comment_replies r JOIN users u ON u.id = r.user_id WHERE r.user_movie_id = $1 ORDER BY r.id LIMIT 200`, commentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	replies := []Reply{}
	for rows.Next() {
		var r Reply
		if err := rows.Scan(&r.ID, &r.CommentID, &r.UserID, &r.Username, &r.Avatar, &r.Content, &r.CreatedAt); err != nil {
			return nil, err
		}
		replies = append(replies, r)
	}
	return replies, rows.Err()
}

// AddReply 回复一条短评，并通知短评作者。
func (s *Store) AddReply(ctx context.Context, userID, commentID int64, content string) error {
	content = strings.TrimSpace(content)
	if length := len([]rune(content)); length < 1 || length > 300 {
		return errors.New("回复需要 1–300 个字")
	}
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var ownerID int64
		if err := tx.QueryRow(ctx, `SELECT user_id FROM user_movies WHERE id = $1 AND comment <> ''`, commentID).Scan(&ownerID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var replyID int64
		if err := tx.QueryRow(ctx, `INSERT INTO comment_replies (user_movie_id, user_id, content) VALUES ($1, $2, $3) RETURNING id`,
			commentID, userID, content).Scan(&replyID); err != nil {
			return err
		}
		if ownerID == userID {
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO notifications (user_id, actor_id, kind, user_movie_id, reply_id) VALUES ($1, $2, 'reply', $3, $4)`,
			ownerID, userID, commentID, replyID)
		return err
	})
}

// DeleteReply 删除回复。只有回复的作者或管理员可以删；管理员删别人的回复时通知作者。返回所属短评的 ID。
func (s *Store) DeleteReply(ctx context.Context, userID int64, admin bool, replyID int64, reason string) (int64, error) {
	if strings.TrimSpace(reason) == "" {
		reason = DefaultRemoveReason
	}
	var commentID int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var authorID, mediaID int64
		var content string
		err := tx.QueryRow(ctx, `DELETE FROM comment_replies r USING user_movies um
WHERE r.id = $1 AND um.id = r.user_movie_id AND (r.user_id = $2 OR $3)
RETURNING r.user_movie_id, r.user_id, r.content, um.media_id`, replyID, userID, admin).Scan(&commentID, &authorID, &content, &mediaID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return notify(ctx, tx, Notice{UserID: authorID, ActorID: userID, Kind: "removed", MediaID: mediaID, Body: content, Reason: reason})
	})
	return commentID, err
}

// Notifications 返回用户的通知，新的在前。
func (s *Store) Notifications(ctx context.Context, userID int64, limit, offset int) ([]Notification, error) {
	return s.notifications(ctx, `n.user_id = $1 ORDER BY n.id DESC LIMIT $2 OFFSET $3`, userID, limit, offset)
}

// Notification 返回一条通知（只能是自己的）。
func (s *Store) Notification(ctx context.Context, userID, id int64) (Notification, error) {
	list, err := s.notifications(ctx, `n.user_id = $1 AND n.id = $2`, userID, id)
	if err != nil {
		return Notification{}, err
	}
	if len(list) == 0 {
		return Notification{}, ErrNotFound
	}
	return list[0], nil
}

func (s *Store) notifications(ctx context.Context, where string, args ...any) ([]Notification, error) {
	rows, err := s.pool.Query(ctx, `SELECT n.id, n.kind, a.id, a.username, a.avatar, COALESCE(m.id, 0), COALESCE(m.title, ''),
CASE WHEN n.body <> '' THEN n.body ELSE COALESCE(r.content, um.comment, '') END, n.reason, COALESCE(n.feedback_id, 0), COALESCE(n.collection_id, 0),
n.created_at, n.read_at IS NOT NULL
FROM notifications n JOIN users a ON a.id = n.actor_id
LEFT JOIN user_movies um ON um.id = n.user_movie_id LEFT JOIN media m ON m.id = COALESCE(n.media_id, um.media_id)
LEFT JOIN comment_replies r ON r.id = n.reply_id
WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Notification{}
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.ActorID, &n.ActorName, &n.ActorAvatar, &n.MediaID, &n.MediaTitle, &n.Excerpt, &n.Reason, &n.FeedbackID, &n.CollectionID, &n.CreatedAt, &n.Read); err != nil {
			return nil, err
		}
		list = append(list, n)
	}
	return list, rows.Err()
}

// Target 是点开这条通知要去的页面。
func (n Notification) Target() string {
	id := func(v int64) string { return strconv.FormatInt(v, 10) }
	switch {
	case n.Kind == "follow":
		return "/user/" + id(n.ActorID)
	case n.Kind == "feedback":
		return "/feedback#f-" + id(n.FeedbackID)
	case n.CollectionID > 0:
		return "/me/collections/" + id(n.CollectionID)
	case n.MediaID > 0 && n.Kind == "removed":
		return "/m/" + id(n.MediaID)
	case n.MediaID > 0:
		return "/m/" + id(n.MediaID) + "#comments"
	}
	return "/notifications"
}

// MarkRead 把一条通知标为已读。
func (s *Store) MarkRead(ctx context.Context, userID, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE notifications SET read_at = now() WHERE user_id = $1 AND id = $2 AND read_at IS NULL`, userID, id)
	return err
}

// DeleteNotification 删除一条通知。
func (s *Store) DeleteNotification(ctx context.Context, userID, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM notifications WHERE user_id = $1 AND id = $2`, userID, id)
	return err
}

// ClearNotifications 删除所有通知。
func (s *Store) ClearNotifications(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM notifications WHERE user_id = $1`, userID)
	return err
}

// Unread 返回未读通知数。
func (s *Store) Unread(ctx context.Context, userID int64) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, userID).Scan(&count)
	return count, err
}

// MarkAllRead 把所有通知标为已读。
func (s *Store) MarkAllRead(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE notifications SET read_at = now() WHERE user_id = $1 AND read_at IS NULL`, userID)
	return err
}

// ToggleFollow 关注或取消关注，返回操作后的状态。
func (s *Store) ToggleFollow(ctx context.Context, followerID, followeeID int64) (following bool, err error) {
	if followerID == followeeID {
		return false, errors.New("不能关注自己")
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM user_follows WHERE follower_id = $1 AND followee_id = $2`, followerID, followeeID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			_, err = tx.Exec(ctx, `DELETE FROM notifications WHERE kind = 'follow' AND user_id = $2 AND actor_id = $1`, followerID, followeeID)
			return err
		}
		following = true
		if _, err := tx.Exec(ctx, `INSERT INTO user_follows (follower_id, followee_id) VALUES ($1, $2)`, followerID, followeeID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO notifications (user_id, actor_id, kind) VALUES ($1, $2, 'follow') ON CONFLICT DO NOTHING`, followeeID, followerID)
		return err
	})
	return following, err
}

// Profile 返回用户的公开资料和关注数据。
func (s *Store) Profile(ctx context.Context, viewerID, userID int64) (Profile, error) {
	var p Profile
	err := s.pool.QueryRow(ctx, `SELECT u.id, u.username, u.avatar, u.is_public, u.created_at,
(SELECT count(*) FROM user_follows WHERE followee_id = u.id),
(SELECT count(*) FROM user_follows WHERE follower_id = u.id),
EXISTS (SELECT 1 FROM user_follows WHERE follower_id = $1 AND followee_id = u.id)
FROM users u WHERE u.id = $2`, viewerID, userID).Scan(&p.ID, &p.Username, &p.Avatar, &p.IsPublic, &p.CreatedAt, &p.Followers, &p.Following, &p.Followed)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

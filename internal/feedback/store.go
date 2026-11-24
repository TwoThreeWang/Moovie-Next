// Package feedback 管理用户反馈和版权投诉。
//
// 每条反馈是一段对话：用户提交的内容是第一条消息，之后用户和管理员都可以继续回复。
// 处理状态不手动设置，而是看最后一条消息是谁发的（见 State）。
package feedback

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"moovie/internal/db"
)

// 对话的状态。
const (
	StateStaff  = "staff"  // 最后一条是用户发的，等管理员回复
	StateUser   = "user"   // 最后一条是管理员发的，等用户回复
	StateClosed = "closed" // 已关闭：有人主动关闭，或者等用户回复超过 AutoCloseDays 天
)

// AutoCloseDays 是管理员回复后用户多少天没有再回复，就自动算作关闭。用户之后再回复会重新打开。
const AutoCloseDays = 14

// MaxLength 是一条消息的最大字数。
const MaxLength = 5000

// Feedback 是一条反馈（一段对话）。
type Feedback struct {
	ID        int64
	UserID    *int64
	Username  string
	Kind      string
	PageURL   string
	State     string
	Outcome   string     // 关闭时标的结果：done、declined，或空
	ClosedAt  *time.Time // 主动关闭的时间；自动关闭的为 nil
	CreatedAt time.Time
	LastAt    time.Time // 最后一条消息的时间
	Preview   string    // 第一条消息，也就是用户最初提交的内容
	Count     int       // 消息条数
	Messages  []Message // 只有 Get 和 WithMessages 会填
}

// Message 是对话里的一条消息。
type Message struct {
	ID        int64
	Staff     bool
	Content   string
	CreatedAt time.Time
}

// Kinds 是反馈的类型和中文名。dmca 是版权投诉，对应"通知—删除"流程。
var Kinds = []struct{ Key, Label string }{
	{"bug", "问题反馈"}, {"request", "求片"}, {"suggestion", "功能建议"}, {"dmca", "版权投诉"},
}

// outcomeLabels 是各类型关闭时两种结果的说法：[done, declined]。
var outcomeLabels = map[string][2]string{
	"bug": {"已修复", "不处理"}, "request": {"已上线", "找不到"}, "suggestion": {"已采纳", "未采纳"}, "dmca": {"已处理", "已驳回"},
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

// OutcomeLabel 返回某个类型的结果的中文说法，outcome 为空或不认识时返回空。
func OutcomeLabel(kind, outcome string) string {
	labels, ok := outcomeLabels[kind]
	switch {
	case !ok:
		return ""
	case outcome == "done":
		return labels[0]
	case outcome == "declined":
		return labels[1]
	}
	return ""
}

// KindLabel 返回这条反馈类型的中文名。
func (f Feedback) KindLabel() string { return KindLabel(f.Kind) }

// OutcomeLabel 返回这条反馈关闭时标的结果，没标时返回空。
func (f Feedback) OutcomeLabel() string { return OutcomeLabel(f.Kind, f.Outcome) }

// Outcomes 返回这条反馈可以标的两种结果，给后台的下拉框用。
func (f Feedback) Outcomes() []struct{ Key, Label string } {
	return []struct{ Key, Label string }{{"done", OutcomeLabel(f.Kind, "done")}, {"declined", OutcomeLabel(f.Kind, "declined")}}
}

// UserStateLabel 是给提交人看的状态。
func (f Feedback) UserStateLabel() string {
	switch f.State {
	case StateStaff:
		return "处理中"
	case StateUser:
		return "已回复"
	}
	if label := f.OutcomeLabel(); label != "" {
		return label
	}
	return "已关闭"
}

// AdminStateLabel 是后台看到的状态。
func (f Feedback) AdminStateLabel() string {
	switch f.State {
	case StateStaff:
		return "待回复"
	case StateUser:
		return "等用户回复"
	}
	if label := f.OutcomeLabel(); label != "" {
		return "已关闭 · " + label
	}
	return "已关闭"
}

// ErrInvalid 表示反馈内容不合法。
var ErrInvalid = errors.New("反馈内容不合法")

// ErrNotFound 表示反馈不存在，或者不是这个用户的。
var ErrNotFound = errors.New("反馈不存在")

// Store 读写 feedbacks 和 feedback_messages 表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

func cleanContent(content string) (string, bool) {
	content = strings.TrimSpace(content)
	return content, content != "" && len([]rune(content)) <= MaxLength
}

// Create 提交一条反馈，返回它的编号。
func (s *Store) Create(ctx context.Context, userID int64, kind, content, pageURL string) (int64, error) {
	content, ok := cleanContent(content)
	if KindLabel(kind) == "" || !ok || len(pageURL) > 500 {
		return 0, ErrInvalid
	}
	var id int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO feedbacks (user_id, kind, page_url) VALUES (NULLIF($1, 0), $2, $3) RETURNING id`,
			userID, kind, pageURL).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO feedback_messages (feedback_id, author_id, staff, content) VALUES ($1, NULLIF($2, 0), false, $3)`,
			id, userID, content)
		return err
	})
	return id, err
}

// listSQL 列出反馈并算出状态。$1 是提交人（0 表示所有人），$2 是反馈编号（0 表示不限）。
var listSQL = `SELECT f.id, f.user_id, COALESCE(u.username, '') AS username, f.kind, f.page_url, f.outcome, f.closed_at, f.created_at,
  last.created_at AS last_at, first.content AS preview, total.n AS count,
  CASE WHEN f.closed_at IS NOT NULL OR (last.staff AND last.created_at < now() - interval '` + autoClose + `') THEN 'closed'
       WHEN last.staff THEN 'user' ELSE 'staff' END AS state
FROM feedbacks f LEFT JOIN users u ON u.id = f.user_id
CROSS JOIN LATERAL (SELECT content FROM feedback_messages WHERE feedback_id = f.id ORDER BY id LIMIT 1) first
CROSS JOIN LATERAL (SELECT staff, created_at FROM feedback_messages WHERE feedback_id = f.id ORDER BY id DESC LIMIT 1) last
CROSS JOIN LATERAL (SELECT count(*)::int AS n FROM feedback_messages WHERE feedback_id = f.id) total
WHERE ($1 = 0 OR f.user_id = $1) AND ($2 = 0 OR f.id = $2)`

var autoClose = strconv.Itoa(AutoCloseDays) + " days"

func scanFeedbacks(rows pgx.Rows) ([]Feedback, error) {
	defer rows.Close()
	list := []Feedback{}
	for rows.Next() {
		var f Feedback
		if err := rows.Scan(&f.ID, &f.UserID, &f.Username, &f.Kind, &f.PageURL, &f.Outcome, &f.ClosedAt, &f.CreatedAt,
			&f.LastAt, &f.Preview, &f.Count, &f.State); err != nil {
			return nil, err
		}
		list = append(list, f)
	}
	return list, rows.Err()
}

// List 列出反馈，最近有动静的在前。userID 非 0 时只看这个用户的；state 非空时按状态筛选。
func (s *Store) List(ctx context.Context, userID int64, state string, limit, offset int) ([]Feedback, error) {
	rows, err := s.pool.Query(ctx, `SELECT * FROM (`+listSQL+`) t WHERE ($3 = '' OR state = $3)
ORDER BY last_at DESC, id DESC LIMIT $4 OFFSET $5`, userID, 0, state, limit, offset)
	if err != nil {
		return nil, err
	}
	return scanFeedbacks(rows)
}

// Get 读出一条反馈和它的全部消息。userID 非 0 时只能读这个用户自己的。
func (s *Store) Get(ctx context.Context, id, userID int64) (Feedback, error) {
	if id <= 0 {
		return Feedback{}, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, listSQL, userID, id)
	if err != nil {
		return Feedback{}, err
	}
	list, err := scanFeedbacks(rows)
	if err != nil {
		return Feedback{}, err
	}
	if len(list) == 0 {
		return Feedback{}, ErrNotFound
	}
	if err := s.WithMessages(ctx, list); err != nil {
		return Feedback{}, err
	}
	return list[0], nil
}

// WithMessages 给一批反馈填上全部消息。
func (s *Store) WithMessages(ctx context.Context, list []Feedback) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]int64, len(list))
	index := map[int64]int{}
	for i, f := range list {
		ids[i], index[f.ID] = f.ID, i
	}
	rows, err := s.pool.Query(ctx, `SELECT feedback_id, id, staff, content, created_at FROM feedback_messages
WHERE feedback_id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var feedbackID int64
		var m Message
		if err := rows.Scan(&feedbackID, &m.ID, &m.Staff, &m.Content, &m.CreatedAt); err != nil {
			return err
		}
		i := index[feedbackID]
		list[i].Messages = append(list[i].Messages, m)
	}
	return rows.Err()
}

// Reply 在对话里加一条消息，已关闭的对话会重新打开。
// owner 非 0 时只能回复这个用户自己的反馈（用户回复），为 0 时不检查（管理员回复）。
// 返回提交人（已注销为 0），用来通知。
func (s *Store) Reply(ctx context.Context, id, owner, authorID int64, staff bool, content string) (userID int64, err error) {
	content, ok := cleanContent(content)
	if !ok {
		return 0, ErrInvalid
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE feedbacks SET closed_at = NULL, outcome = '' WHERE id = $1 AND ($2 = 0 OR user_id = $2)
RETURNING COALESCE(user_id, 0)`, id, owner).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO feedback_messages (feedback_id, author_id, staff, content) VALUES ($1, NULLIF($2, 0), $3, $4)`,
			id, authorID, staff, content)
		return err
	})
	return userID, err
}

// Close 关闭一段对话，outcome 是结果（done、declined 或空）。owner 的含义同 Reply。已经关闭的只更新结果。
func (s *Store) Close(ctx context.Context, id, owner int64, outcome string) (userID int64, err error) {
	if outcome != "" && outcome != "done" && outcome != "declined" {
		return 0, ErrInvalid
	}
	err = s.pool.QueryRow(ctx, `UPDATE feedbacks SET closed_at = COALESCE(closed_at, now()), outcome = $3
WHERE id = $1 AND ($2 = 0 OR user_id = $2) RETURNING COALESCE(user_id, 0)`, id, owner, outcome).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return userID, err
}

// OpenCount 返回等管理员回复的反馈数量。
func (s *Store) OpenCount(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM (`+listSQL+`) t WHERE state = 'staff'`, 0, 0).Scan(&count)
	return count, err
}

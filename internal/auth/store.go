// Package auth 负责账号、登录状态和访问控制。
package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"moovie/internal/db"
)

// User 是一个账号。
type User struct {
	ID            int64
	Email         string
	Username      string
	PasswordHash  string
	Role          string
	Avatar        string
	IsPublic      bool
	DoubanUserID  string
	AdSkipEnabled bool
	SessionEpoch  int
	CreatedAt     time.Time
	// 账号限制：Restriction 为 mute（禁言）或 ban（封禁）；RestrictedUntil 为空表示永久。
	Restriction       string
	RestrictedUntil   *time.Time
	RestrictionReason string
}

// restricted 判断某种限制当前是否有效（没到期）。封禁包含禁言。
func (u *User) restricted(kind string) bool {
	if u == nil || u.Restriction == "" || (u.RestrictedUntil != nil && time.Now().After(*u.RestrictedUntil)) {
		return false
	}
	return u.Restriction == kind || (kind == "mute" && u.Restriction == "ban")
}

// Banned 表示账号被封禁（不能登录）。
func (u *User) Banned() bool { return u.restricted("ban") }

// Muted 表示账号被禁言（不能发短评、回复和弹幕）。
func (u *User) Muted() bool { return u.restricted("mute") }

// RestrictionText 是给用户看的限制说明，例如"已被禁言到 2026-11-23 10:00，原因：广告"。
func (u *User) RestrictionText() string {
	if u == nil || u.Restriction == "" {
		return ""
	}
	label := map[string]string{"mute": "禁言", "ban": "封禁"}[u.Restriction]
	text := "已被" + label
	if u.RestrictedUntil != nil {
		text = "已被" + label + "到 " + u.RestrictedUntil.Format("2006-01-02 15:04")
	} else {
		text += "（永久）"
	}
	if u.RestrictionReason != "" {
		text += "，原因：" + u.RestrictionReason
	}
	return text
}

// IsAdmin 表示是不是管理员。
func (u *User) IsAdmin() bool { return u != nil && u.Role == "admin" }

var (
	// ErrNotFound 表示账号不存在。
	ErrNotFound = errors.New("账号不存在")
	// ErrTaken 表示邮箱或用户名已被占用。
	ErrTaken = errors.New("邮箱或用户名已被占用")
)

// Store 读写 users 表。
type Store struct{ db db.Querier }

// NewStore 创建 Store。
func NewStore(q db.Querier) *Store { return &Store{db: q} }

const userColumns = `id, email, username, password_hash, role, avatar, is_public, douban_user_id, ad_skip_enabled, session_epoch, created_at,
restriction, restricted_until, restriction_reason`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Username, &u.PasswordHash, &u.Role, &u.Avatar, &u.IsPublic,
		&u.DoubanUserID, &u.AdSkipEnabled, &u.SessionEpoch, &u.CreatedAt, &u.Restriction, &u.RestrictedUntil, &u.RestrictionReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// Create 新建账号。库里还没有任何账号时，第一个注册的人成为管理员。
func (s *Store) Create(ctx context.Context, email, username, passwordHash string) (*User, error) {
	user, err := scanUser(s.db.QueryRow(ctx, `INSERT INTO users (email, username, password_hash, role)
VALUES ($1, $2, $3, CASE WHEN EXISTS (SELECT 1 FROM users) THEN 'user' ELSE 'admin' END)
RETURNING `+userColumns, normalizeEmail(email), strings.TrimSpace(username), passwordHash))
	if db.IsUniqueViolation(err) {
		return nil, ErrTaken
	}
	return user, err
}

// ByID 按 ID 查账号。
func (s *Store) ByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

// ByEmail 按邮箱查账号。
func (s *Store) ByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, normalizeEmail(email)))
}

// editable 是允许通过 Update 修改的列。
var editable = map[string]bool{
	"email": true, "username": true, "avatar": true, "is_public": true,
	"douban_user_id": true, "ad_skip_enabled": true, "role": true,
}

// Update 修改一个字段。
func (s *Store) Update(ctx context.Context, id int64, column string, value any) error {
	if !editable[column] {
		return errors.New("不允许修改的字段: " + column)
	}
	if column == "email" {
		value = normalizeEmail(value.(string))
	}
	tag, err := s.db.Exec(ctx, `UPDATE users SET `+column+` = $2 WHERE id = $1`, id, value)
	if db.IsUniqueViolation(err) {
		return ErrTaken
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// SetPassword 修改密码，并让这个账号在其他设备上的登录全部失效。
func (s *Store) SetPassword(ctx context.Context, id int64, passwordHash string) (int, error) {
	var epoch int
	err := s.db.QueryRow(ctx, `UPDATE users SET password_hash = $2, session_epoch = session_epoch + 1
WHERE id = $1 RETURNING session_epoch`, id, passwordHash).Scan(&epoch)
	return epoch, err
}

// SetRestriction 设置或解除账号限制（kind 为空表示解除）。封禁时让所有设备上的登录失效。
func (s *Store) SetRestriction(ctx context.Context, id int64, kind string, until *time.Time, reason string) error {
	if kind != "" && kind != "mute" && kind != "ban" {
		return errors.New("限制类型不正确")
	}
	if kind == "" {
		until, reason = nil, ""
	}
	tag, err := s.db.Exec(ctx, `UPDATE users SET restriction = $2, restricted_until = $3, restriction_reason = $4,
session_epoch = session_epoch + CASE WHEN $2 = 'ban' THEN 1 ELSE 0 END WHERE id = $1`, id, kind, until, strings.TrimSpace(reason))
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// Delete 删除账号，关联数据随外键级联删除。
func (s *Store) Delete(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	return err
}

// List 分页列出账号，可按邮箱或用户名搜索。
func (s *Store) List(ctx context.Context, query string, limit, offset int) ([]*User, int, error) {
	pattern := "%" + strings.ToLower(strings.TrimSpace(query)) + "%"
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(email) LIKE $1 OR lower(username) LIKE $1`, pattern).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(ctx, `SELECT `+userColumns+` FROM users
WHERE lower(email) LIKE $1 OR lower(username) LIKE $1 ORDER BY id DESC LIMIT $2 OFFSET $3`, pattern, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var users []*User
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		users = append(users, user)
	}
	return users, total, rows.Err()
}

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

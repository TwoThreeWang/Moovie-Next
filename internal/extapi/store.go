// Package extapi 存取浏览器插件（仓库里的 extension/ 目录）要用的数据：连接凭证、贡献记录、写回豆瓣的待办，
// 以及插件要的几样查询。接口本身在 internal/app/extension.go。设计见 docs/extension.md。
package extapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"moovie/internal/db"
)

// ErrNoToken 表示凭证不存在或已经失效。
var ErrNoToken = errors.New("插件凭证无效")

// outboxKeep 是写回豆瓣的待办保留多久。插件一个月都没开过，再写回去也没多大意义。
const outboxKeep = 30 * 24 * time.Hour

// Store 读写插件相关的表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Issue 给用户签发一个新凭证，返回凭证明文（只在这时出现一次）。epoch 是账号当前的密码版本。
func (s *Store) Issue(ctx context.Context, userID int64, epoch int, userAgent string) (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	token := "mvx_" + hex.EncodeToString(buffer)
	if runes := []rune(userAgent); len(runes) > 300 {
		userAgent = string(runes[:300])
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO extension_tokens (user_id, token_hash, session_epoch, user_agent) VALUES ($1, $2, $3, $4)`,
		userID, hashToken(token), epoch, userAgent)
	return token, err
}

// Authenticate 校验凭证，返回用户 ID 和签发时的密码版本，顺便记下最近使用时间（一小时内只记一次）。
func (s *Store) Authenticate(ctx context.Context, token string) (userID int64, epoch int, err error) {
	if len(token) < 20 || len(token) > 200 {
		return 0, 0, ErrNoToken
	}
	err = s.pool.QueryRow(ctx, `UPDATE extension_tokens SET last_used_at = CASE WHEN last_used_at < now() - interval '1 hour' THEN now() ELSE last_used_at END
WHERE token_hash = $1 RETURNING user_id, session_epoch`, hashToken(token)).Scan(&userID, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrNoToken
	}
	return userID, epoch, err
}

// Revoke 删掉一个凭证（插件里点"断开"）。
func (s *Store) Revoke(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM extension_tokens WHERE token_hash = $1`, hashToken(token))
	return err
}

// RevokeAll 删掉用户的全部凭证（设置页点"断开全部插件"）。
func (s *Store) RevokeAll(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM extension_tokens WHERE user_id = $1`, userID)
	return err
}

// Connected 返回用户连着几个插件、最近一次用是什么时候。
func (s *Store) Connected(ctx context.Context, userID int64) (count int, last *time.Time, err error) {
	err = s.pool.QueryRow(ctx, `SELECT count(*), max(last_used_at) FROM extension_tokens WHERE user_id = $1`, userID).Scan(&count, &last)
	return
}

// ---------- 贡献 ----------

// RecordContribution 记下用户用插件补全了一部作品的资料。
func (s *Store) RecordContribution(ctx context.Context, userID, mediaID int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO media_contributions (user_id, media_id) VALUES ($1, $2)
ON CONFLICT (user_id, media_id) DO UPDATE SET created_at = now()`, userID, mediaID)
	return err
}

// Contributions 返回用户补全过几部作品。
func (s *Store) Contributions(ctx context.Context, userID int64) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM media_contributions WHERE user_id = $1`, userID).Scan(&count)
	return count, err
}

// ---------- 写回豆瓣 ----------

// OutboxItem 是一条等插件写回豆瓣的标记。
type OutboxItem struct {
	ID       int64     `json:"id"`
	DoubanID string    `json:"douban_id"`
	Status   string    `json:"status"` // wish 或 watched
	Rating   int       `json:"rating"`
	At       time.Time `json:"at"`
}

// QueueOutbox 记下用户在本站做的一次标记，等插件写回豆瓣。没连接过插件的用户不记。
func (s *Store) QueueOutbox(ctx context.Context, userID int64, doubanID, status string, rating int) error {
	if doubanID == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO douban_outbox (user_id, douban_id, status, rating)
SELECT $1, $2, $3, $4 WHERE EXISTS (SELECT 1 FROM extension_tokens WHERE user_id = $1)`, userID, doubanID, status, rating)
	return err
}

// Outbox 返回编号大于 after 的待办，最早的在前。after 为 0 时只返回最新的一条的编号（Cursor），不返回内容：
// 插件第一次打开双向同步时，不该把以前在本站做的标记全都写回豆瓣。
func (s *Store) Outbox(ctx context.Context, userID, after int64, limit int) (items []OutboxItem, cursor int64, err error) {
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM douban_outbox WHERE user_id = $1`, userID).Scan(&cursor); err != nil {
		return nil, 0, err
	}
	items = []OutboxItem{}
	if after <= 0 {
		return items, cursor, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, douban_id, status, rating, created_at FROM douban_outbox
WHERE user_id = $1 AND id > $2 ORDER BY id LIMIT $3`, userID, after, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var item OutboxItem
		if err := rows.Scan(&item.ID, &item.DoubanID, &item.Status, &item.Rating, &item.At); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, cursor, rows.Err()
}

// tokenIdle 是凭证多久没用就删掉：插件每天都会调接口，这么久没动静多半是卸载了。
const tokenIdle = 90 * 24 * time.Hour

// Prune 删掉过期的写回待办和长期没用的凭证。
func (s *Store) Prune(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM douban_outbox WHERE created_at < now() - make_interval(secs => $1)`, outboxKeep.Seconds()); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM extension_tokens WHERE last_used_at < now() - make_interval(secs => $1)`, tokenIdle.Seconds())
	return err
}

// ---------- 插件要的查询 ----------

// Task 是一部等着补全豆瓣资料的作品。
type Task struct {
	MediaID  int64  `json:"-"`
	DoubanID string `json:"douban_id"`
	Type     string `json:"type"` // 作品类型，插件据此先试电影接口还是剧集接口
}

// OwnPending 返回这个用户自己标记过、或者放进自己片单里的、还没有豆瓣资料的作品，最近加的在前。
func (s *Store) OwnPending(ctx context.Context, userID int64, limit int) ([]Task, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.id, m.douban_id, m.media_type FROM media m
WHERE m.metadata_status = 'partial' AND m.douban_id <> '' AND m.hongguo_id = '' AND NOT m.hidden
  AND (m.douban_restricted_until IS NULL OR m.douban_restricted_until <= now()) AND m.id IN (
  SELECT media_id FROM user_movies WHERE user_id = $1
  UNION SELECT ci.media_id FROM collection_items ci JOIN collections c ON c.id = ci.collection_id WHERE c.owner_user_id = $1)
ORDER BY m.id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		var task Task
		if err := rows.Scan(&task.MediaID, &task.DoubanID, &task.Type); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// Mark 是用户对一部作品的标记，给豆瓣页面上的海报角标用。
type Mark struct {
	MediaID  int64  `json:"media_id"`
	Status   string `json:"status,omitempty"`  // wish、watched，没有标记为空
	Episode  int    `json:"episode,omitempty"` // 有播放进度时，最近看到第几集（电影为 0）
	Watching bool   `json:"watching,omitempty"`
}

// Marks 返回用户对这些豆瓣条目的标记和播放进度，只包括有标记或有进度的。
func (s *Store) Marks(ctx context.Context, userID int64, doubanIDs []string) (map[string]Mark, error) {
	result := map[string]Mark{}
	if len(doubanIDs) == 0 {
		return result, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT m.douban_id, m.id, coalesce(um.status, ''),
coalesce((SELECT p.episode FROM playback_positions p WHERE p.user_id = $1 AND p.media_id = m.id ORDER BY p.activity_at DESC LIMIT 1), -1)
FROM media m LEFT JOIN user_movies um ON um.media_id = m.id AND um.user_id = $1
WHERE m.douban_id = ANY($2) AND NOT m.hidden`, userID, doubanIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var doubanID string
		var mark Mark
		var episode int
		if err := rows.Scan(&doubanID, &mark.MediaID, &mark.Status, &episode); err != nil {
			return nil, err
		}
		if episode >= 0 {
			mark.Watching, mark.Episode = true, episode
		}
		if mark.Status != "" || mark.Watching {
			result[doubanID] = mark
		}
	}
	return result, rows.Err()
}

// Airing 是用户在追的一部剧集到今天为止播出到了第几集。
type Airing struct {
	MediaID int64  `json:"media_id"`
	Title   string `json:"title"`
	Type    string `json:"type"`
	Season  int    `json:"season"`
	Latest  int    `json:"latest"`
}

// Airing 返回用户在追的（想看的，或者有播放进度的）剧集、动漫、综艺最新播出到第几集，
// 只看最近 60 天里有更新的。插件和上次看到的比，变大了就提醒。
func (s *Store) Airing(ctx context.Context, userID int64) ([]Airing, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.id, m.title, m.media_type, m.season, max(u.episode) FROM media m
JOIN media_units u ON u.media_id = m.id AND u.air_date <= current_date
WHERE NOT m.hidden AND m.media_type <> 'movie' AND (
  EXISTS (SELECT 1 FROM user_movies um WHERE um.user_id = $1 AND um.media_id = m.id AND um.status = 'wish')
  OR EXISTS (SELECT 1 FROM playback_positions p WHERE p.user_id = $1 AND p.media_id = m.id))
GROUP BY m.id HAVING max(u.air_date) >= current_date - 60 ORDER BY m.id LIMIT 300`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Airing{}
	for rows.Next() {
		var item Airing
		if err := rows.Scan(&item.MediaID, &item.Title, &item.Type, &item.Season, &item.Latest); err != nil {
			return nil, err
		}
		list = append(list, item)
	}
	return list, rows.Err()
}

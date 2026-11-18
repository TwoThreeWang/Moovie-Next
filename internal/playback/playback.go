// Package playback 保存播放进度。进度只按作品和集数记录，不含任何源信息。
// 浏览器只上报，页面由服务器直接读库渲染，所以不需要"增量同步"那一套（版本号、软删除）。
package playback

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
	"moovie/internal/db"
	"moovie/internal/httpx"
	"moovie/internal/media"
)

// Position 是某一集的播放进度。电影固定用 season=1, episode=1。
type Position struct {
	MediaID   int64   `json:"media_id"`
	Season    int     `json:"season"`
	Episode   int     `json:"episode"`
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	Completed bool    `json:"completed"`
	At        int64   `json:"at"` // 播放发生的时间，毫秒时间戳
}

// Recent 是"继续观看"里的一项。
type Recent struct {
	Card     media.Card
	Season   int
	Episode  int
	Position float64
	Duration float64
	At       time.Time
}

// Store 读写 playback_positions 表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

func valid(p Position) bool {
	return p.MediaID > 0 && p.Season >= 0 && p.Season <= 200 && p.Episode >= 0 && p.Episode <= 5000 &&
		p.Position >= 0 && p.Duration >= 0 && p.Duration <= 86400 && p.Position <= 86400
}

// Upsert 保存一批进度。同一集以播放时间较新的为准（不是进度较大的：用户可能往回拖）。
// 不存在的作品和不合法的条目会被跳过。返回实际写入的条数。
func (s *Store) Upsert(ctx context.Context, userID int64, positions []Position) (int, error) {
	written := 0
	now := time.Now()
	for _, p := range positions {
		if !valid(p) {
			continue
		}
		at := time.UnixMilli(p.At)
		// 客户端时钟不准时不能让一条记录永远排在最前面。
		if p.At <= 0 || at.After(now.Add(5*time.Minute)) {
			at = now
		}
		tag, err := s.pool.Exec(ctx, `INSERT INTO playback_positions
(user_id, media_id, season, episode, position_seconds, duration_seconds, completed, activity_at)
SELECT $1, m.id, $3, $4, $5, $6, $7, $8 FROM media m WHERE m.id = $2
ON CONFLICT (user_id, media_id, season, episode) DO UPDATE SET
  position_seconds = EXCLUDED.position_seconds, duration_seconds = EXCLUDED.duration_seconds,
  completed = EXCLUDED.completed, activity_at = EXCLUDED.activity_at
WHERE playback_positions.activity_at <= EXCLUDED.activity_at`,
			userID, p.MediaID, p.Season, p.Episode, p.Position, p.Duration, p.Completed, at)
		if err != nil {
			return written, err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		written++
		if _, err := s.pool.Exec(ctx, `INSERT INTO watch_days (user_id, day) VALUES ($1, $2::date) ON CONFLICT DO NOTHING`,
			userID, at.Format("2006-01-02")); err != nil {
			return written, err
		}
	}
	return written, nil
}

// Delete 删除某部作品的全部进度。
func (s *Store) Delete(ctx context.Context, userID, mediaID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM playback_positions WHERE user_id = $1 AND media_id = $2`, userID, mediaID)
	return err
}

// Recent 返回"继续观看"：每部作品只取最近看的那一集。
func (s *Store) Recent(ctx context.Context, userID int64, limit, offset int) ([]Recent, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.id, m.media_type, m.title, m.year, m.poster, COALESCE(m.backdrops[1], ''),
CASE WHEN m.rating_douban > 0 THEN m.rating_douban ELSE m.rating_tmdb END, m.season, m.episode_count,
latest.season, latest.episode, latest.position_seconds, latest.duration_seconds, latest.activity_at
FROM (
  SELECT DISTINCT ON (media_id) * FROM playback_positions
  WHERE user_id = $1 ORDER BY media_id, activity_at DESC
) latest JOIN media m ON m.id = latest.media_id
WHERE NOT m.hidden ORDER BY latest.activity_at DESC LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Recent{}
	for rows.Next() {
		var r Recent
		c := &r.Card
		if err := rows.Scan(&c.ID, &c.Type, &c.Title, &c.Year, &c.Poster, &c.Backdrop, &c.Rating, &c.Season, &c.EpisodeCount,
			&r.Season, &r.Episode, &r.Position, &r.Duration, &r.At); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// ForMedia 返回用户在某部作品各集上的进度。
func (s *Store) ForMedia(ctx context.Context, userID, mediaID int64) ([]Position, error) {
	rows, err := s.pool.Query(ctx, `SELECT media_id, season, episode, position_seconds, duration_seconds, completed, activity_at
FROM playback_positions WHERE user_id = $1 AND media_id = $2 ORDER BY activity_at DESC`, userID, mediaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	positions := []Position{}
	for rows.Next() {
		var p Position
		var at time.Time
		if err := rows.Scan(&p.MediaID, &p.Season, &p.Episode, &p.Position, &p.Duration, &p.Completed, &at); err != nil {
			return nil, err
		}
		p.At = at.UnixMilli()
		positions = append(positions, p)
	}
	return positions, rows.Err()
}

// ---------- HTTP ----------

// Handler 提供上报和删除进度的接口。
type Handler struct {
	store *Store
	limit *httpx.Limiter
}

// NewHandler 创建 Handler。
func NewHandler(store *Store) *Handler {
	return &Handler{store: store, limit: httpx.NewLimiter(120, time.Minute)}
}

// Register 挂载路由。
func (h *Handler) Register(router gin.IRouter) {
	api := router.Group("/api/positions", auth.Require())
	api.POST("", h.upsert)
	api.DELETE("/:media_id", h.remove)
}

func (h *Handler) upsert(c *gin.Context) {
	user := auth.Current(c)
	if !h.limit.Allow(strconv.FormatInt(user.ID, 10)) {
		httpx.Error(c, http.StatusTooManyRequests, "操作太频繁")
		return
	}
	var body struct {
		Items []Position `json:"items"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Items) == 0 || len(body.Items) > 200 {
		httpx.Error(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	written, err := h.store.Upsert(c.Request.Context(), user.ID, body.Items)
	if err != nil {
		c.Error(err)
		httpx.Error(c, http.StatusInternalServerError, "保存进度失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"written": written})
}

func (h *Handler) remove(c *gin.Context) {
	mediaID, ok := httpx.ParamID(c, "media_id")
	if !ok {
		return
	}
	if err := h.store.Delete(c.Request.Context(), auth.Current(c).ID, mediaID); err != nil {
		c.Error(err)
		httpx.Error(c, http.StatusInternalServerError, "删除失败")
		return
	}
	if httpx.IsHTMX(c) {
		c.Status(http.StatusOK)
		return
	}
	c.Status(http.StatusNoContent)
}

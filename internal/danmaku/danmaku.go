// Package danmaku 提供弹幕：用户自己发的，加上第三方弹幕库里的。
//
// 有元数据的作品按"作品 ID + 季 + 集"归档；没有的按"片名|S01|E003"归档。两种方式都和播放源无关。
package danmaku

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
	"moovie/internal/db"
	"moovie/internal/httpx"
	"moovie/internal/media"
	"moovie/internal/providers"
)

const (
	localMaximum    = 3000
	externalMaximum = 6000
	externalTTL     = 6 * time.Hour
	// 第三方弹幕在内存里最多缓存多少集。一集最多 6000 条、约 0.6 MB，200 集封顶在 120 MB 左右；
	// 超过就整个清空重来（丢了只是下次打开时重新取一次）。
	externalCacheMax = 200
)

// Key 标识一集的弹幕。MediaID 非 0 时按作品归档，否则按片名归档。
type Key struct {
	MediaID int64
	Title   string
	Season  int
	Episode int
}

// TitleKey 返回按片名归档时用的键，形如"片名|S01|E003"。
func (k Key) TitleKey() string {
	return fmt.Sprintf("%s|S%02d|E%03d", media.Normalize(k.Title), k.Season, k.Episode)
}

// Store 读写 danmakus 表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

// List 返回一集里用户发的弹幕。
func (s *Store) List(ctx context.Context, key Key, limit int) ([]providers.DanmuComment, error) {
	query := `SELECT time_seconds, body, mode, color FROM danmakus WHERE NOT deleted AND `
	args := []any{}
	if key.MediaID > 0 {
		query += `media_id = $1 AND season = $2 AND episode = $3`
		args = append(args, key.MediaID, key.Season, key.Episode)
	} else {
		query += `title_key = $1`
		args = append(args, key.TitleKey())
	}
	rows, err := s.pool.Query(ctx, query+` ORDER BY time_seconds LIMIT `+strconv.Itoa(limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []providers.DanmuComment{}
	for rows.Next() {
		var d providers.DanmuComment
		if err := rows.Scan(&d.Time, &d.Text, &d.Mode, &d.Color); err != nil {
			return nil, err
		}
		list = append(list, d)
	}
	return list, rows.Err()
}

// Add 保存一条用户弹幕。10 秒内同一用户在同一集发的重复内容会被拒绝。
func (s *Store) Add(ctx context.Context, userID int64, key Key, d providers.DanmuComment) (bool, error) {
	var mediaID any
	titleKey := ""
	if key.MediaID > 0 {
		mediaID = key.MediaID
	} else {
		titleKey = key.TitleKey()
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO danmakus (media_id, season, episode, title_key, time_seconds, body, mode, color, user_id)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9 WHERE NOT EXISTS (
  SELECT 1 FROM danmakus WHERE user_id = $9 AND body = $6 AND created_at > now() - interval '10 seconds'
  AND media_id IS NOT DISTINCT FROM $1 AND season = $2 AND episode = $3 AND title_key = $4)`,
		mediaID, key.Season, key.Episode, titleKey, d.Time, d.Text, d.Mode, d.Color, userID)
	return tag.RowsAffected() == 1, err
}

// ---------- 服务：合并本地和第三方弹幕 ----------

// Upstream 是第三方弹幕库。
type Upstream interface {
	Enabled() bool
	Fetch(ctx context.Context, title string, season, episode int) ([]providers.DanmuComment, error)
}

type cached struct {
	items []providers.DanmuComment
	at    time.Time
}

// Service 合并用户弹幕和第三方弹幕。
type Service struct {
	store    *Store
	media    *media.Store
	upstream Upstream

	mu    sync.Mutex
	cache map[string]cached
}

// NewService 创建 Service。upstream 可以为 nil。
func NewService(store *Store, mediaStore *media.Store, upstream Upstream) *Service {
	return &Service{store: store, media: mediaStore, upstream: upstream, cache: map[string]cached{}}
}

// resolve 返回站内归档键和第三方查询键。
func (s *Service) resolve(ctx context.Context, key Key) (Key, Key, error) {
	if key.MediaID == 0 {
		return key, key, nil
	}
	m, err := s.media.Get(ctx, key.MediaID)
	if err != nil {
		return key, Key{}, err
	}
	if key.Season == 0 {
		key.Season = m.Season
	}
	if m.Type == media.TypeMovie {
		// 电影的站内集号用于归档，第三方按片名匹配。
		return key, Key{Title: m.Title}, nil
	}
	// 第三方弹幕库按"剧名 S02E03"匹配，所以用去掉季数标记的片名。
	base, _ := media.SplitSeason(m.Title)
	return key, Key{Title: base, Season: key.Season, Episode: key.Episode}, nil
}

// List 返回一集的全部弹幕，按时间排序。第三方弹幕库出错时只返回用户弹幕。
func (s *Service) List(ctx context.Context, key Key) ([]providers.DanmuComment, error) {
	key, externalKey, err := s.resolve(ctx, key)
	if err != nil {
		return nil, err
	}
	local, err := s.store.List(ctx, key, localMaximum)
	if err != nil {
		return nil, err
	}
	merged := append(local, s.external(ctx, externalKey.Title, externalKey.Season, externalKey.Episode)...)
	sort.SliceStable(merged, func(a, b int) bool { return merged[a].Time < merged[b].Time })
	return merged, nil
}

func (s *Service) external(ctx context.Context, title string, season, episode int) []providers.DanmuComment {
	if s.upstream == nil || !s.upstream.Enabled() || strings.TrimSpace(title) == "" {
		return nil
	}
	cacheKey := fmt.Sprintf("%s|%d|%d", media.Normalize(title), season, episode)
	s.mu.Lock()
	entry, ok := s.cache[cacheKey]
	if len(s.cache) > externalCacheMax {
		s.cache = map[string]cached{}
	}
	s.mu.Unlock()
	if ok && time.Since(entry.at) < externalTTL {
		return entry.items
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	items, err := s.upstream.Fetch(fetchCtx, title, max(1, season), episode)
	if err != nil {
		// 失败时用旧缓存兜底，没有就不返回第三方弹幕。
		return entry.items
	}
	items = sample(items, externalMaximum)
	s.mu.Lock()
	s.cache[cacheKey] = cached{items: items, at: time.Now()}
	s.mu.Unlock()
	return items
}

// sample 弹幕过多时按固定间隔抽样，保证时间分布均匀。
func sample(items []providers.DanmuComment, maximum int) []providers.DanmuComment {
	if len(items) <= maximum {
		return items
	}
	result := make([]providers.DanmuComment, 0, maximum)
	step := float64(len(items)) / float64(maximum)
	for index := 0; index < maximum; index++ {
		result = append(result, items[int(float64(index)*step)])
	}
	return result
}

var colorPattern = regexp.MustCompile(`^#[0-9A-F]{6}$`)

// Send 发一条弹幕。
func (s *Service) Send(ctx context.Context, userID int64, key Key, d providers.DanmuComment) error {
	key, _, err := s.resolve(ctx, key)
	if err != nil {
		return err
	}
	d.Text = strings.TrimSpace(d.Text)
	d.Color = strings.ToUpper(d.Color)
	if d.Color == "" {
		d.Color = "#FFFFFF"
	}
	length := len([]rune(d.Text))
	if length < 1 || length > 50 || d.Time < 0 || d.Mode < 0 || d.Mode > 2 || !colorPattern.MatchString(d.Color) ||
		(key.MediaID == 0 && media.Normalize(key.Title) == "") {
		return ErrInvalid
	}
	added, err := s.store.Add(ctx, userID, key, d)
	if err == nil && !added {
		return ErrDuplicate
	}
	return err
}

// ErrInvalid 表示弹幕内容不合法。
var ErrInvalid = fmt.Errorf("弹幕内容不合法")

// ErrDuplicate 表示短时间内重复发送。
var ErrDuplicate = fmt.Errorf("请不要重复发送")

// ---------- HTTP ----------

// Handler 提供弹幕接口。
type Handler struct {
	service *Service
	send    *httpx.Limiter
}

// NewHandler 创建 Handler。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service, send: httpx.NewLimiter(20, time.Minute)}
}

// Register 挂载路由。
func (h *Handler) Register(router gin.IRouter) {
	api := router.Group("/api/danmaku", auth.Require())
	api.GET("", h.list)
	api.POST("", httpx.RateLimit(h.send), auth.RequireVoice(), h.post)
}

func keyFrom(mediaID int64, title string, season, episode int) Key {
	return Key{MediaID: mediaID, Title: title, Season: season, Episode: episode}
}

func (h *Handler) list(c *gin.Context) {
	mediaID, _ := strconv.ParseInt(c.Query("media_id"), 10, 64)
	key := keyFrom(mediaID, c.Query("title"), httpx.QueryInt(c, "season", 1), httpx.QueryInt(c, "episode", 1))
	items, err := h.service.List(c.Request.Context(), key)
	if err != nil {
		httpx.Error(c, http.StatusNotFound, "作品不存在")
		return
	}
	// 不让浏览器缓存：刚发的弹幕刷新后就要看得到。第三方弹幕在服务器上已经缓存了，库里的查一次很快。
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) post(c *gin.Context) {
	var body struct {
		MediaID int64   `json:"media_id"`
		Title   string  `json:"title"`
		Season  int     `json:"season"`
		Episode int     `json:"episode"`
		Time    float64 `json:"time"`
		Text    string  `json:"text"`
		Mode    int     `json:"mode"`
		Color   string  `json:"color"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	err := h.service.Send(c.Request.Context(), auth.Current(c).ID, keyFrom(body.MediaID, body.Title, body.Season, body.Episode),
		providers.DanmuComment{Time: body.Time, Text: body.Text, Mode: body.Mode, Color: body.Color})
	switch err {
	case nil:
		c.Status(http.StatusNoContent)
	case ErrInvalid, ErrDuplicate:
		httpx.Error(c, http.StatusBadRequest, err.Error())
	default:
		c.Error(err)
		httpx.Error(c, http.StatusInternalServerError, "发送失败")
	}
}

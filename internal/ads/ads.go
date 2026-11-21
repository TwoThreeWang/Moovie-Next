// Package ads 管理站内的自托管广告，内容在管理后台设置。
//
// 每条广告是一张图片（填图片链接，不上传）或者一行文字，可以带一个点击链接。同一个广告位可以有多条，
// 按排序号竖着往下排。不加载任何第三方脚本，不追踪访客。页面渲染时读的是内存里的缓存（最多 30 秒前的），
// 不会每次打开页面都查库；有多个网站进程时，后台改完最多 30 秒各处都会生效。
package ads

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"moovie/internal/db"
)

// Sidebar 是播放页弹幕下方、详情页信息下方的广告位，每条高 60px。
const Sidebar = "sidebar"

const (
	maxText     = 60
	maxURL      = 2000
	maxPerSlot  = 10
	maxPosition = 999
	cacheFor    = 30 * time.Second
)

// ErrNotFound 表示要改或删的广告不存在。
var ErrNotFound = errors.New("广告不存在")

// InputError 是填写的内容不对，可以直接显示给管理员。
type InputError struct{ message string }

func (e *InputError) Error() string { return e.message }

func invalid(message string) error { return &InputError{message} }

// Ad 是一条广告。
type Ad struct {
	ID       int64
	Slot     string
	Position int // 从小到大往下排
	Enabled  bool
	Kind     string // image 或 text
	ImageURL string
	Text     string // 文字广告的内容；图片广告的说明文字（图片加载不出来、读屏软件读的就是它）
	Link     string // 点击后去的地址，可以为空
}

// Visible 表示这条广告开着，而且有可以显示的内容。
func (a Ad) Visible() bool {
	if !a.Enabled {
		return false
	}
	if a.Kind == "image" {
		return a.ImageURL != ""
	}
	return a.Text != ""
}

// Store 读写 ads 表。
type Store struct {
	pool  *db.Pool
	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	ads []Ad
	at  time.Time
}

func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool, cache: map[string]cached{}}
}

// List 返回一个广告位的所有广告（包括关着的），按显示顺序。
func (s *Store) List(ctx context.Context, slot string) ([]Ad, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, slot, position, enabled, kind, image_url, text, link FROM ads
WHERE slot = $1 ORDER BY position, id`, slot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Ad{}
	for rows.Next() {
		var ad Ad
		if err := rows.Scan(&ad.ID, &ad.Slot, &ad.Position, &ad.Enabled, &ad.Kind, &ad.ImageURL, &ad.Text, &ad.Link); err != nil {
			return nil, err
		}
		list = append(list, ad)
	}
	return list, rows.Err()
}

// Current 返回页面上要显示的广告（开着、有内容的），按显示顺序；没有或读不出来时返回 nil。
// 广告读不出来不影响页面，只是不显示（之前读到过的话继续用上一次的）。
func (s *Store) Current(ctx context.Context, slot string) []Ad {
	s.mu.Lock()
	entry, ok := s.cache[slot]
	s.mu.Unlock()
	if ok && time.Since(entry.at) < cacheFor {
		return entry.ads
	}
	list, err := s.List(ctx, slot)
	var shown []Ad
	if err != nil {
		shown = entry.ads
	} else {
		for _, ad := range list {
			if ad.Visible() {
				shown = append(shown, ad)
			}
		}
	}
	s.mu.Lock()
	s.cache[slot] = cached{ads: shown, at: time.Now()}
	s.mu.Unlock()
	return shown
}

// Create 检查并添加一条广告，返回它的 ID。返回的检查错误可以直接给管理员看。
func (s *Store) Create(ctx context.Context, ad Ad) (int64, error) {
	if err := Normalize(&ad); err != nil {
		return 0, err
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ads WHERE slot = $1`, ad.Slot).Scan(&count); err != nil {
		return 0, err
	}
	if count >= maxPerSlot {
		return 0, invalid("一个广告位最多 10 条")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO ads (slot, position, enabled, kind, image_url, text, link)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`, ad.Slot, ad.Position, ad.Enabled, ad.Kind, ad.ImageURL, ad.Text, ad.Link).Scan(&id)
	if err == nil {
		s.forget(ad.Slot)
	}
	return id, err
}

// Update 检查并保存一条广告的修改。
func (s *Store) Update(ctx context.Context, ad Ad) error {
	if err := Normalize(&ad); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE ads SET position = $3, enabled = $4, kind = $5, image_url = $6, text = $7, link = $8,
updated_at = now() WHERE id = $1 AND slot = $2`, ad.ID, ad.Slot, ad.Position, ad.Enabled, ad.Kind, ad.ImageURL, ad.Text, ad.Link)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.forget(ad.Slot)
	return nil
}

// Delete 删除一条广告。
func (s *Store) Delete(ctx context.Context, slot string, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM ads WHERE id = $1 AND slot = $2`, id, slot)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.forget(slot)
	return nil
}

// forget 让本进程的缓存立刻失效，后台改完马上能在页面上看到。
func (s *Store) forget(slot string) {
	s.mu.Lock()
	delete(s.cache, slot)
	s.mu.Unlock()
}

// Normalize 去掉首尾空白并检查各项。开着的广告必须有内容；关着的也检查格式，免得以后打开时才发现不对。
func Normalize(ad *Ad) error {
	ad.ImageURL, ad.Text, ad.Link = strings.TrimSpace(ad.ImageURL), strings.Join(strings.Fields(ad.Text), " "), strings.TrimSpace(ad.Link)
	if ad.Slot != Sidebar {
		return invalid("未知的广告位")
	}
	if ad.Kind != "image" && ad.Kind != "text" {
		return invalid("请选择图片或文字")
	}
	if ad.Position < 0 || ad.Position > maxPosition {
		return invalid("排序要在 0 到 999 之间")
	}
	if utf8.RuneCountInString(ad.Text) > maxText {
		return invalid("文字最多 60 个字")
	}
	if ad.ImageURL != "" && !siteOrHTTPS(ad.ImageURL, false) {
		return invalid("图片链接要以 https:// 开头，或者是本站的路径（以 / 开头）")
	}
	if ad.Link != "" && !siteOrHTTPS(ad.Link, true) {
		return invalid("点击链接要以 https:// 或 http:// 开头，或者是本站的路径（以 / 开头）")
	}
	if ad.Enabled && ad.Kind == "image" && ad.ImageURL == "" {
		return invalid("图片广告需要填图片链接")
	}
	if ad.Enabled && ad.Kind == "text" && ad.Text == "" {
		return invalid("文字广告需要填文字")
	}
	return nil
}

// siteOrHTTPS 只接受 https 地址（允许 http 时也接受 http）或本站路径。
// 图片只能是 https：页面的安全策略只允许加载 https 图片，http 图片会被浏览器拦下。
func siteOrHTTPS(raw string, allowHTTP bool) bool {
	if len(raw) > maxURL || strings.ContainsAny(raw, " \t\r\n\\") {
		return false
	}
	if strings.HasPrefix(raw, "/") {
		return !strings.HasPrefix(raw, "//")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	return parsed.Scheme == "https" || (allowHTTP && parsed.Scheme == "http")
}

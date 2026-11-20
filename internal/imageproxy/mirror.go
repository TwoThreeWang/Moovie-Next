package imageproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"moovie/internal/db"
	"moovie/internal/telegram"
)

// Mirror 把代理过的图片备份到 Telegram，之后优先从 Telegram 取。
// 原图地址失效（豆瓣改了地址、防盗链变严）时图片也还在。
//
// 备份在网站进程里做，不走任务队列：代理从原图取图的时候已经拿到了图片，顺手交给备份器，
// 不用再下载一次；备份器在后台一张一张地上传，遵守 Telegram 的频率限制，不占后台任务的执行位。
// 排着队的图片只在内存里（最多 32 MB），进程重启丢了也没关系：下次有人看到这张图会重新排。
type Mirror struct {
	db  db.Querier
	tg  *telegram.Client
	log *slog.Logger
	gap time.Duration // 两次上传的间隔：私聊每秒一条，频道和群每分钟约 20 条

	mu           sync.Mutex
	pending      []upload
	pendingBytes int
	// cooling 记着排过队、或者上传失败过的图片，到期之前不再接收，免得同一张图反复排、反复失败。
	cooling map[string]time.Time
	wake    chan struct{}
	next    time.Time // 下一次允许上传的时间，只有上传的那个协程读写
}

type upload struct {
	raw, contentType string
	name             string
	data             []byte
}

const (
	maxPendingBytes = 32 << 20
	coolingFor      = time.Hour
	uploadTries     = 3 // 被 Telegram 限流时按它说的时间等，最多试这么多次
)

// Record 是一张图片的备份记录：原图地址对应 Telegram 里的哪个文件。
type Record struct {
	OriginalURL string
	FileID      string
	ContentType string
	Bytes       int
	CreatedAt   time.Time
}

// 两次上传之间的最小间隔。
var (
	uploadGap        = 3200 * time.Millisecond // 频道、群
	privateUploadGap = 1100 * time.Millisecond // 和机器人的私聊
)

// NewMirror 创建备份器，并在后台开始上传。
func NewMirror(q db.Querier, tg *telegram.Client, log *slog.Logger) *Mirror {
	m := newMirror(q, tg, log)
	go m.loop()
	return m
}

func newMirror(q db.Querier, tg *telegram.Client, log *slog.Logger) *Mirror {
	gap := uploadGap
	if tg.Private() {
		gap = privateUploadGap
	}
	return &Mirror{db: q, tg: tg, log: log, gap: gap, cooling: map[string]time.Time{}, wake: make(chan struct{}, 1)}
}

func hashURL(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// Lookup 查一张图片的备份。
func (m *Mirror) Lookup(ctx context.Context, raw string) (Record, bool, error) {
	var r Record
	err := m.db.QueryRow(ctx, `SELECT original_url, tg_file_id, content_type, bytes, created_at FROM image_mirrors WHERE url_hash = $1`,
		hashURL(raw)).Scan(&r.OriginalURL, &r.FileID, &r.ContentType, &r.Bytes, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, nil
	}
	return r, err == nil, err
}

// Offer 把刚从原图取到、还没备份的图片交给备份器。不会等待：排着的太多就算了。
func (m *Mirror) Offer(raw, contentType string, data []byte, name string) {
	key := hex.EncodeToString(hashURL(raw))
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if until, ok := m.cooling[key]; ok && now.Before(until) {
		return
	}
	if m.pendingBytes+len(data) > maxPendingBytes {
		return
	}
	if len(m.cooling) > 50000 {
		for existing, until := range m.cooling {
			if now.After(until) {
				delete(m.cooling, existing)
			}
		}
	}
	m.cooling[key] = now.Add(coolingFor)
	m.pending = append(m.pending, upload{raw: raw, contentType: contentType, data: data, name: name})
	m.pendingBytes += len(data)
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Mirror) take() (upload, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 0 {
		return upload{}, false
	}
	item := m.pending[0]
	m.pending[0] = upload{}
	m.pending = m.pending[1:]
	m.pendingBytes -= len(item.data)
	return item, true
}

func (m *Mirror) loop() {
	for {
		if !m.drain() {
			<-m.wake
		}
	}
}

// drain 把排着的图片全部上传。返回 false 表示本来就没有要传的。
func (m *Mirror) drain() bool {
	worked := false
	for {
		item, ok := m.take()
		if !ok {
			return worked
		}
		worked = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		if err := m.upload(ctx, item); err != nil {
			m.log.Warn("图片备份失败，一小时内不再重试", "error", err)
		}
		cancel()
	}
}

// upload 上传一张图片并记下对应关系。被 Telegram 限流时按它说的时间等一会儿再试。
func (m *Mirror) upload(ctx context.Context, item upload) error {
	if _, found, err := m.Lookup(ctx, item.raw); err != nil || found {
		return err
	}
	name := item.name
	for try := 1; ; try++ {
		if wait := time.Until(m.next); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		m.next = time.Now().Add(m.gap)
		uploaded, err := m.tg.Upload(ctx, name, item.contentType, item.data)
		var limited *telegram.RetryAfter
		if errors.As(err, &limited) && try < uploadTries {
			m.next = time.Now().Add(limited.Wait)
			continue
		}
		if err != nil {
			return err
		}
		_, err = m.db.Exec(ctx, `INSERT INTO image_mirrors (url_hash, original_url, tg_file_id, content_type, bytes)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT (url_hash) DO NOTHING`, hashURL(item.raw), item.raw, uploaded.FileID, item.contentType, len(item.data))
		return err
	}
}

// Open 从 Telegram 读取备份。
func (m *Mirror) Open(ctx context.Context, r Record) (io.ReadCloser, error) {
	return m.tg.Open(ctx, r.FileID)
}

// Counts 返回已经备份的图片数量和总大小。
func (m *Mirror) Counts(ctx context.Context) (count int, bytes int64, err error) {
	err = m.db.QueryRow(ctx, `SELECT count(*), COALESCE(sum(bytes), 0) FROM image_mirrors`).Scan(&count, &bytes)
	return count, bytes, err
}

func extension(contentType string) string {
	switch contentType {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/avif":
		return ".avif"
	}
	return ".jpg"
}

func mediaType(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.TrimSpace(contentType)
}

// openOrigin 请求原图；不是图片或者状态码不对时返回错误。
func openOrigin(ctx context.Context, client *http.Client, raw string) (*http.Response, error) {
	// 不让允许域名的重定向把请求带到任意地址或内网。
	if !Allowed(raw) {
		return nil, fmt.Errorf("图片来源不允许")
	}
	checked := *client
	checked.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !Allowed(request.URL.String()) {
			return fmt.Errorf("图片重定向来源不允许")
		}
		if len(via) >= 10 {
			return fmt.Errorf("图片重定向次数过多")
		}
		if client.CheckRedirect != nil {
			return client.CheckRedirect(request, via)
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(strings.ToLower(request.URL.Hostname()), ".doubanio.com") {
		request.Header.Set("Referer", "https://movie.douban.com/")
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	response, err := checked.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "image/") {
		response.Body.Close()
		return nil, fmt.Errorf("原图请求失败：HTTP %d %s", response.StatusCode, response.Header.Get("Content-Type"))
	}
	return response, nil
}

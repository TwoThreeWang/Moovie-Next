// Package imageproxy 代理作品图片（豆瓣、TMDB、红果短剧）。豆瓣图片有防盗链，浏览器不能直接引用；
// 配置了 Telegram 时，代理过的图片会备份到 Telegram，之后优先从那里取。
// 只代理允许的来源域名，不接受任意地址，也不会访问资源站。图片内容由 CDN 缓存。
package imageproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/semaphore"

	"moovie/internal/httpx"
)

const maxImageBytes = 8 << 20

// memoryBudget 限制同时缓冲在内存里的图片字节和缩图时解码出的位图，避免回源高峰撑爆容器内存。
const memoryBudget = 96 << 20

// Handler 是图片代理。
type Handler struct {
	client      *http.Client
	slots       chan struct{}
	resizeSlots chan struct{}
	memory      *semaphore.Weighted
	key         []byte
	requests    *httpx.Limiter
	originals   *httpx.Limiter

	mu     sync.Mutex
	failed map[string]time.Time // 最近从原图取不到的地址：10 分钟内不再去取，免得每次打开页面都重复请求对方
	// Mirror 不为空时，图片先从 Telegram 备份里取，没有备份的交给它去备份。
	Mirror *Mirror
}

// New 创建图片代理，最多同时转发 32 个请求。
func New(client *http.Client, secret string) *Handler {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	var key []byte
	if secret != "" {
		sum := sha256.Sum256([]byte("moovie-image-key-v1:" + secret))
		key = sum[:]
	}
	return &Handler{client: client, slots: make(chan struct{}, 32), resizeSlots: make(chan struct{}, 2), memory: semaphore.NewWeighted(memoryBudget), key: key,
		requests: httpx.NewLimiter(600, time.Minute), originals: httpx.NewLimiter(60, time.Minute), failed: map[string]time.Time{}}
}

// Register 挂载路由。
func (h *Handler) Register(router gin.IRouter) {
	router.GET("/img/:encoded", h.serve)
	router.HEAD("/img/:encoded", h.serve)
}

// Allowed 判断一个地址是不是允许代理的图片：豆瓣的图片域名、TMDB 的图片服务器，和红果短剧的封面（p3-novel.byteimg.com 这样的域名）。
func Allowed(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return strings.HasSuffix(host, ".doubanio.com") || host == "image.tmdb.org" || hongguoImageHost.MatchString(host)
}

var hongguoImageHost = regexp.MustCompile(`^p[0-9]+-novel\.byteimg\.com$`)

func (h *Handler) serve(c *gin.Context) {
	// 默认所有失败都禁止缓存，只有完整获取图片后才设置成功响应的缓存头。
	c.Header("Cache-Control", "no-store")
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		c.Status(http.StatusBadRequest)
		return
	}
	ref, size, err := h.verify(c.Param("encoded"), query)
	if err != nil {
		c.Status(http.StatusForbidden)
		return
	}
	if !h.requests.Allow(c.ClientIP()) || (size == "original" && !h.originals.Allow(c.ClientIP())) {
		c.Header("Retry-After", "60")
		c.Status(http.StatusTooManyRequests)
		return
	}
	target, resize := ref.source(size)
	key := backupKey(target, size, resize)
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	case <-c.Request.Context().Done():
		return
	}
	ctx := c.Request.Context()
	budget := &reservation{memory: h.memory, ctx: ctx}
	defer budget.release()
	wait := waitsFor(size)
	if data, contentType, ok := h.readBackup(budget, key, wait.backup); ok {
		h.write(c, contentType, "telegram", data)
		return
	}
	data, contentType, from, err := h.readSource(budget, target, resize > 0, wait)
	if err != nil && size != "original" && target != ref.URL {
		// 单张图片缺少上游的小尺寸版本时，用来源版本（或它的 TG 备份）在本机缩图。
		// 结果按本机缩图的 key 备份：上游尺寸地址可能被别的规格共用（豆瓣的 display 和 original 都是 l 图），
		// 不能让缩小的兜底图占用它。
		resize = resizeEdge(size)
		key = backupKey(ref.URL, size, resize)
		if data, contentType, ok := h.readBackup(budget, key, wait.backup); ok {
			h.write(c, contentType, "telegram", data)
			return
		}
		data, contentType, from, err = h.readSource(budget, ref.URL, true, wait)
	}
	if err != nil {
		h.fail(c)
		return
	}
	if resize > 0 {
		select {
		case h.resizeSlots <- struct{}{}:
			defer func() { <-h.resizeSlots }()
		case <-ctx.Done():
			return
		}
		data, contentType, err = resizeImageQuality(data, contentType, resize, jpegQuality(size), budget.take)
		if err != nil {
			h.fail(c)
			return
		}
	}
	if err := h.write(c, contentType, from, data); err == nil && h.Mirror != nil {
		h.Mirror.Offer(key, contentType, data, ref.filename(size, contentType, key))
	}
}

// backupKey 是 Telegram 备份的查找键：上游提供的尺寸直接用它的地址，本机缩图的结果另加规格后缀。
func backupKey(source, size string, resize int) string {
	if resize > 0 {
		return source + "#moovie-" + size + "-v1"
	}
	return source
}

// waits 是一次请求里读 Telegram 备份、回源各自最多等多久。外层 HTTP 客户端的超时是一分钟，
// 图片等不了那么久：卡住的请求还占着全站共用的执行位（slots），会拖慢其他人的图片。
// 备份读不回来就改从来源取；来源也超时就返回失败，失败不会被 CDN 缓存。原图大，给得宽一些。
type waits struct{ backup, source time.Duration }

var waitsFor = func(size string) waits { // 变量：测试里换成很短的时间
	if size == "original" {
		return waits{backup: 20 * time.Second, source: 40 * time.Second}
	}
	return waits{backup: 6 * time.Second, source: 15 * time.Second}
}

// reservation 记录一次请求占用的内存额度，请求结束时一并归还。
type reservation struct {
	memory *semaphore.Weighted
	ctx    context.Context
	held   int64
}

func (r *reservation) take(n int64) error {
	n = min(max(n, 1), memoryBudget)
	if err := r.memory.Acquire(r.ctx, n); err != nil {
		return err
	}
	r.held += n
	return nil
}

func (r *reservation) release() {
	if r.held > 0 {
		r.memory.Release(r.held)
		r.held = 0
	}
}

func (h *Handler) readBackup(budget *reservation, key string, wait time.Duration) ([]byte, string, bool) {
	if h.Mirror == nil {
		return nil, "", false
	}
	ctx, cancel := context.WithTimeout(budget.ctx, wait)
	defer cancel()
	record, found, err := h.Mirror.Lookup(ctx, key)
	if err != nil || !found || record.Bytes <= 0 || record.Bytes > maxImageBytes || budget.take(int64(record.Bytes)) != nil {
		return nil, "", false
	}
	body, err := h.Mirror.Open(ctx, record)
	if err != nil {
		return nil, "", false
	}
	defer body.Close()
	data, err := readImage(body, int64(record.Bytes))
	if err != nil || len(data) != record.Bytes {
		return nil, "", false
	}
	return data, record.ContentType, true
}

func (h *Handler) readSource(budget *reservation, target string, backup bool, wait waits) ([]byte, string, string, error) {
	if backup {
		if data, typ, ok := h.readBackup(budget, target, wait.backup); ok {
			return data, typ, "telegram", nil
		}
	}
	if h.recentlyFailed(target) {
		return nil, "", "", fmt.Errorf("图片来源冷却中")
	}
	ctx, cancel := context.WithTimeout(budget.ctx, wait.source)
	defer cancel()
	response, err := openOrigin(ctx, h.client, target)
	if err != nil {
		h.markFailed(target)
		return nil, "", "", err
	}
	defer response.Body.Close()
	// 来源给出长度时只预留实际大小，否则按上限预留。
	limit := int64(maxImageBytes)
	if response.ContentLength > 0 && response.ContentLength <= maxImageBytes {
		limit = response.ContentLength
	}
	if err := budget.take(limit); err != nil {
		return nil, "", "", err
	}
	data, err := readImage(response.Body, limit)
	if err != nil {
		h.markFailed(target)
		return nil, "", "", err
	}
	return data, mediaType(response.Header.Get("Content-Type")), "origin", nil
}

const failedFor = 10 * time.Minute

// 失败冷却只保存在应用里，不能让 CDN 把失败缓存成所有访客都打不开的图片。
func (h *Handler) fail(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusBadGateway)
}

func (h *Handler) recentlyFailed(target string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	at, ok := h.failed[target]
	return ok && time.Since(at) < failedFor
}

func (h *Handler) markFailed(target string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.failed) > 10000 {
		for key, at := range h.failed {
			if time.Since(at) >= failedFor {
				delete(h.failed, key)
			}
		}
		if len(h.failed) > 10000 {
			h.failed = map[string]time.Time{}
		}
	}
	h.failed[target] = time.Now()
}

func (h *Handler) write(c *gin.Context, contentType, from string, data []byte) error {
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "public, max-age=2592000, immutable")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Image-From", from)
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	c.Header("ETag", etag)
	for _, tag := range strings.Split(c.GetHeader("If-None-Match"), ",") {
		if tag = strings.TrimSpace(tag); tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
			c.Status(http.StatusNotModified)
			return nil
		}
	}
	c.Header("Content-Length", fmt.Sprint(len(data)))
	c.Status(http.StatusOK)
	if c.Request.Method == http.MethodHead {
		return nil
	}
	_, err := c.Writer.Write(data)
	return err
}

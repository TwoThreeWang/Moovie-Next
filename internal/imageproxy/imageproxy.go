// Package imageproxy 代理作品图片（豆瓣、TMDB、红果短剧）。豆瓣图片有防盗链，浏览器不能直接引用；
// 配置了 Telegram 时，代理过的图片会备份到 Telegram，之后优先从那里取。
// 只代理这两个图片域名，不接受任意地址，也不会访问资源站。
package imageproxy

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const maxImageBytes = 8 << 20

// Handler 是图片代理。
type Handler struct {
	client *http.Client
	slots  chan struct{}

	mu     sync.Mutex
	failed map[string]time.Time // 最近从原图取不到的地址：10 分钟内不再去取，免得每次打开页面都重复请求对方
	// Mirror 不为空时，图片先从 Telegram 备份里取，没有备份的交给它去备份。
	Mirror *Mirror
}

// New 创建图片代理，最多同时转发 32 个请求。
func New(client *http.Client) *Handler {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Handler{client: client, slots: make(chan struct{}, 32), failed: map[string]time.Time{}}
}

// Register 挂载路由。
func (h *Handler) Register(router gin.IRouter) { router.GET("/img/:encoded", h.serve) }

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
	decoded, err := base64.RawURLEncoding.DecodeString(c.Param("encoded"))
	target := string(decoded)
	if err != nil || !Allowed(target) {
		c.Status(http.StatusBadRequest)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	case <-c.Request.Context().Done():
		return
	}
	ctx := c.Request.Context()
	if h.Mirror != nil {
		if record, found, err := h.Mirror.Lookup(ctx, target); err == nil && found {
			if body, err := h.Mirror.Open(ctx, record); err == nil {
				defer body.Close()
				h.write(c, record.ContentType, "telegram", body)
				return
			}
			// Telegram 取不到：退回原图
		}
	}
	if h.recentlyFailed(target) {
		h.fail(c)
		return
	}
	response, err := openOrigin(ctx, h.client, target)
	if err != nil {
		h.markFailed(target)
		h.fail(c)
		return
	}
	defer response.Body.Close()
	contentType := mediaType(response.Header.Get("Content-Type"))
	if h.Mirror == nil {
		h.write(c, contentType, "origin", response.Body)
		return
	}
	// 一边发给浏览器一边留一份，发完了交给备份器，省得备份时再下载一次。
	// 只有完整发出去、而且没超过大小上限的图片才备份。
	var copied bytes.Buffer
	if err := h.write(c, contentType, "origin", io.TeeReader(response.Body, &copied)); err == nil && copied.Len() < maxImageBytes {
		h.Mirror.Offer(target, contentType, copied.Bytes())
	}
}

const failedFor = 10 * time.Minute

// fail 告诉浏览器这张图取不到，并让它 10 分钟内别再来要（浏览器会显示默认图）。
func (h *Handler) fail(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=600")
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

func (h *Handler) write(c *gin.Context, contentType, from string, body io.Reader) error {
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "public, max-age=2592000, immutable")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Image-From", from)
	c.Status(http.StatusOK)
	_, err := io.Copy(c.Writer, io.LimitReader(body, maxImageBytes))
	return err
}

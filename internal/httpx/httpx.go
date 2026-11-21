// Package httpx 放和业务无关的 HTTP 小工具：CSRF、限流、安全响应头、错误输出。
package httpx

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// IsHTMX 判断请求是不是 HTMX 发的局部请求。
func IsHTMX(c *gin.Context) bool { return c.GetHeader("HX-Request") == "true" }

// WantsJSON 判断请求是不是接口调用（/api/ 下且不是 HTMX 片段）。
func WantsJSON(c *gin.Context) bool {
	path := c.Request.URL.Path
	return c.GetHeader("Accept") == "application/json" ||
		strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/api/htmx/")
}

// ErrorPages 为完整页面提供统一的错误页，接口和局部请求继续使用原有格式。
func ErrorPages(render func(*gin.Context, int, string)) gin.HandlerFunc {
	return func(c *gin.Context) { c.Set("httpx.errorPage", render) }
}

// Error 用对应请求的格式返回错误。
func Error(c *gin.Context, status int, message string) {
	c.Header("Cache-Control", "no-store")
	if WantsJSON(c) {
		c.AbortWithStatusJSON(status, gin.H{"error": message})
		return
	}
	if !IsHTMX(c) && !strings.HasPrefix(c.Request.URL.Path, "/api/") {
		if render, ok := c.Get("httpx.errorPage"); ok {
			render.(func(*gin.Context, int, string))(c, status, message)
			c.Abort()
			return
		}
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(status, message)
	c.Abort()
}

// ---------- CSRF：双重提交 Cookie ----------

const csrfCookie = "mv_csrf"

// CSRFToken 返回当前访客的 CSRF 令牌，没有就新建并写入 Cookie。
func CSRFToken(c *gin.Context) string {
	if value, ok := c.Get("csrf"); ok {
		return value.(string)
	}
	token, err := c.Cookie(csrfCookie)
	if err != nil || len(token) != 32 {
		buffer := make([]byte, 16)
		rand.Read(buffer)
		token = hex.EncodeToString(buffer)
		http.SetCookie(c.Writer, &http.Cookie{
			Name: csrfCookie, Value: token, Path: "/", MaxAge: 86400 * 365,
			SameSite: http.SameSiteLaxMode, Secure: c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https",
		})
	}
	c.Set("csrf", token)
	return token
}

// CSRF 要求所有会修改数据的请求带上和 Cookie 一致的令牌（请求头 X-CSRF-Token 或表单字段 _csrf）。
func CSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := CSRFToken(c)
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		sent := c.GetHeader("X-CSRF-Token")
		if sent == "" {
			sent = c.PostForm("_csrf")
		}
		if sent == "" || subtle.ConstantTimeCompare([]byte(sent), []byte(token)) != 1 {
			Error(c, http.StatusForbidden, "页面已过期，请刷新后重试")
			return
		}
		c.Next()
	}
}

// ---------- 安全响应头 ----------

// SecurityHeaders 设置 CSP 等响应头。
//
// connect-src / media-src / img-src 放开到任意 HTTPS 地址，因为用户的播放源由浏览器直连，
// 服务器事先不知道是哪些域名。正因为这样，资源站返回的内容在前端一律当作不可信数据处理。
//
// scriptHashes 是允许执行的内联脚本的哈希（形如 'sha256-…'），用于 import map。
//
// 每个响应还带一个随机 nonce：站点自己不用它，是给 Cloudflare 的 JavaScript Detections 用的。
// Cloudflare 会在页面末尾插一段内联脚本，内容每次不同、没法算哈希；它会从这个响应头里读出 nonce，
// 加到自己插入的脚本上。没有 nonce 时那段脚本被 CSP 拦下，控制台报错。
func SecurityHeaders(dev bool, scriptHashes ...string) gin.HandlerFunc {
	local := ""
	if dev {
		local = " http://localhost:* http://127.0.0.1:*"
	}
	before, after, _ := strings.Cut(strings.Join([]string{
		"default-src 'self'",
		strings.TrimSpace("script-src 'self' 'nonce-\x00' 'inline-speculation-rules' " + strings.Join(scriptHashes, " ")),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob: https:" + local,
		"media-src 'self' blob: https:" + local,
		"connect-src 'self' https:" + local,
		"worker-src 'self' blob:",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"object-src 'none'",
	}, "; "), "\x00")
	return func(c *gin.Context) {
		header := c.Writer.Header()
		nonce := make([]byte, 16)
		_, _ = rand.Read(nonce)
		header.Set("Content-Security-Policy", before+base64.StdEncoding.EncodeToString(nonce)+after)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")
		header.Set("X-Frame-Options", "DENY")
		c.Next()
	}
}

// ---------- 限流：按 key 的令牌桶 ----------

// Limiter 是内存里的令牌桶限流器。
type Limiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     float64 // 每秒恢复多少个令牌
	capacity float64
	now      func() time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// NewLimiter 创建限流器：每个 key 在 per 时间内最多 count 次，允许短时间内用完。
func NewLimiter(count int, per time.Duration) *Limiter {
	return &Limiter{buckets: map[string]*bucket{}, rate: float64(count) / per.Seconds(), capacity: float64(count), now: time.Now}
}

// Allow 消耗一个令牌；没有令牌时返回 false。
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	current, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) > 50000 {
			l.sweep(now)
		}
		current = &bucket{tokens: l.capacity, seen: now}
		l.buckets[key] = current
	}
	current.tokens = min(l.capacity, current.tokens+now.Sub(current.seen).Seconds()*l.rate)
	current.seen = now
	if current.tokens < 1 {
		return false
	}
	current.tokens--
	return true
}

// sweep 清掉已经恢复满的桶，防止 map 无限增长。
func (l *Limiter) sweep(now time.Time) {
	for key, current := range l.buckets {
		if current.tokens+now.Sub(current.seen).Seconds()*l.rate >= l.capacity {
			delete(l.buckets, key)
		}
	}
}

// RateLimit 按客户端 IP 限流。
func RateLimit(limiter *Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !limiter.Allow(c.ClientIP()) {
			seconds := strconv.Itoa(int(math.Ceil(1 / limiter.rate)))
			c.Header("Retry-After", seconds)
			Error(c, http.StatusTooManyRequests, "操作太频繁，请至少等待 "+seconds+" 秒后再试。")
			return
		}
		c.Next()
	}
}

// ParamID 读取路径里的数字 ID。不是正整数时返回 400 并终止请求。
func ParamID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		Error(c, http.StatusBadRequest, "参数不正确")
		return 0, false
	}
	return id, true
}

// QueryInt 读取查询参数里的整数，缺失或不合法时返回 fallback。
func QueryInt(c *gin.Context, name string, fallback int) int {
	value, err := strconv.Atoi(c.Query(name))
	if err != nil {
		return fallback
	}
	return value
}

// ParsePage 校验页码（从 1 开始，空值是第 1 页）并计算偏移。不是正整数或大到偏移会溢出时 ok 为 false。
func ParsePage(raw string, size int) (page, offset int, ok bool) {
	if raw == "" {
		return 1, 0, true
	}
	page, err := strconv.Atoi(raw)
	if err != nil || page < 1 || page > math.MaxInt/size {
		return 0, 0, false
	}
	return page, (page - 1) * size, true
}

// Page 读取 page 参数，不合法时按第 1 页处理。
func Page(c *gin.Context, size int) (page, offset int) {
	if page, offset, ok := ParsePage(c.Query("page"), size); ok {
		return page, offset
	}
	return 1, 0
}

// HeadAsGet 让 HEAD 请求按 GET 处理。路由只注册了 GET，不这样的话 HEAD 全都落到 404，
// 在线监控、链接检查和部分爬虫会以为页面不存在。响应体由 net/http 丢弃：
// 它按连接上原始请求的方法判断，这里只改交给路由的那份副本。
func HeadAsGet(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			r = r.Clone(r.Context())
			r.Method = http.MethodGet
		}
		next.ServeHTTP(w, r)
	})
}

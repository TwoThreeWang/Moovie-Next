// Package relay 替浏览器转发播放源的查询请求。
//
// 浏览器直接查资源站时，大多数资源站没有给跨域许可，请求会被浏览器拦下；HTTP 的接口在 HTTPS 页面里也访问不了。
// 这时浏览器改走 GET /api/relay?url=…，由服务器去取，原样把文本交回。只转发文本（JSON、XML），
// 不转发视频、播放清单和图片；只有登录用户能用，按用户限流；服务器自己的内网地址一律不让访问。
package relay

import (
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
	"moovie/internal/httpx"
)

const (
	maxBody    = 2 << 20 // 资源站一页的查询结果一般几十 KB
	timeout    = 8 * time.Second
	concurrent = 64 // 同时最多转发这么多个请求，再多的排队
)

// ErrBlocked 表示目标地址是内网、本机这类不允许访问的地址。
var ErrBlocked = errors.New("不允许访问这个地址")

// blockedPrefixes 是 netip 自带判断之外，还不能访问的网段。
// 198.18.0.0/15 不拦：Clash、mihomo 这类代理软件的 fake-ip 模式把所有域名都解析到这个网段，
// 拦了的话在这种网络环境里所有转发都会失败；这个网段本身不会有内网服务。
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // 运营商级 NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64，可能绕到内网
}

// allowed 判断一个 IP 能不能访问：只允许公网单播地址。
func allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

// Relay 是转发器。
type Relay struct {
	client  *http.Client
	slots   chan struct{}
	limiter *httpx.Limiter
}

// New 创建转发器。检查放在建立连接的那一刻（拿到真正要连的 IP 之后），
// 域名解析出内网地址、或者跳转到内网地址，都会被拦下。
func New() *Relay { return newRelay(false) }

func newRelay(allowPrivate bool) *Relay {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return ErrBlocked
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !allowed(ip) {
			return ErrBlocked
		}
		return nil
	}}
	transport := &http.Transport{
		DialContext: dialer.DialContext, Proxy: nil, ForceAttemptHTTP2: true,
		MaxIdleConns: 100, MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("跳转次数太多")
		}
		if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
			return ErrBlocked
		}
		return nil
	}}
	return &Relay{client: client, slots: make(chan struct{}, concurrent), limiter: httpx.NewLimiter(300, time.Minute)}
}

// Register 挂载路由。要登录：游客用不到播放源，也免得被人当成公共代理。
func (r *Relay) Register(router gin.IRouter) {
	router.GET("/api/relay", auth.Require(), r.serve)
}

func (r *Relay) serve(c *gin.Context) {
	user := auth.Current(c)
	if !r.limiter.Allow(strconv.FormatInt(user.ID, 10)) {
		httpx.Error(c, http.StatusTooManyRequests, "查询太频繁，请稍后再试")
		return
	}
	target, err := url.Parse(c.Query("url"))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil {
		httpx.Error(c, http.StatusBadRequest, "地址不正确")
		return
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-c.Request.Context().Done():
		return
	}
	status, contentType, body, err := r.fetch(c.Request.Context(), target.String())
	switch {
	case errors.Is(err, ErrBlocked):
		httpx.Error(c, http.StatusForbidden, ErrBlocked.Error())
		return
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		httpx.Error(c, http.StatusGatewayTimeout, "对方响应太慢")
		return
	case errors.Is(err, errTooLarge):
		httpx.Error(c, http.StatusRequestEntityTooLarge, err.Error())
		return
	case errors.Is(err, errNotText):
		httpx.Error(c, http.StatusUnsupportedMediaType, err.Error())
		return
	case err != nil:
		httpx.Error(c, http.StatusBadGateway, "连不上对方")
		return
	}
	// 一律当纯文本交回：内容是别人网站的，不能让它以本站的身份被当成网页打开。
	charset := "utf-8"
	if _, params, err := mime.ParseMediaType(contentType); err == nil && params["charset"] != "" {
		charset = params["charset"]
	}
	c.Header("Content-Type", "text/plain; charset="+charset)
	c.Header("Content-Security-Policy", "sandbox; default-src 'none'")
	c.Header("Cache-Control", "no-store")
	c.Data(status, "text/plain; charset="+charset, body)
}

var (
	errTooLarge = errors.New("内容太大")
	errNotText  = errors.New("只转发文本，不转发视频和图片")
)

func (r *Relay) fetch(ctx context.Context, target string) (int, string, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, "", nil, err
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	request.Header.Set("Accept", "application/json, text/xml, */*;q=0.5")
	response, err := r.client.Do(request) // 拨号时被拦下的，错误链里带着 ErrBlocked
	if err != nil {
		return 0, "", nil, err
	}
	defer response.Body.Close()
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	for _, kind := range []string{"video/", "audio/", "image/", "mpegurl", "octet-stream"} {
		if strings.Contains(contentType, kind) {
			return 0, "", nil, errNotText
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return 0, "", nil, err
	}
	if len(body) > maxBody {
		return 0, "", nil, errTooLarge
	}
	if strings.HasPrefix(strings.TrimSpace(string(body[:min(len(body), 64)])), "#EXTM3U") {
		return 0, "", nil, errNotText
	}
	return response.StatusCode, contentType, body, nil
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Package app 把各个模块装配成一个 HTTP 应用，并包含所有页面的处理函数。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	netmail "net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/ads"
	"moovie/internal/adskip"
	"moovie/internal/auth"
	"moovie/internal/collections"
	"moovie/internal/config"
	"moovie/internal/danmaku"
	"moovie/internal/db"
	"moovie/internal/doubansync"
	"moovie/internal/feedback"
	"moovie/internal/httpx"
	"moovie/internal/imageproxy"
	"moovie/internal/ingest"
	"moovie/internal/jobs"
	"moovie/internal/library"
	"moovie/internal/mail"
	"moovie/internal/media"
	"moovie/internal/playback"
	"moovie/internal/pow"
	"moovie/internal/providers"
	"moovie/internal/recommend"
	"moovie/internal/relay"
	"moovie/internal/report"
	"moovie/internal/social"
	"moovie/internal/telegram"
	"moovie/internal/trends"
	"moovie/internal/ui"
	"moovie/internal/vault"
	"moovie/web"
)

// App 持有所有模块。
type App struct {
	Config config.Config
	Pool   *db.Pool
	Log    *slog.Logger

	Users       *auth.Store
	Sessions    *auth.Sessions
	Media       *media.Store
	Ingest      *ingest.Service
	Library     *library.Store
	Social      *social.Store
	Collections *collections.Store
	Feedback    *feedback.Store
	Positions   *playback.Store
	Vault       *vault.Store
	AdSkip      *adskip.Store
	Ads         *ads.Store
	Danmaku     *danmaku.Service
	Recommend   *recommend.Service
	Reports     *report.Service
	Trends      *trends.Store
	Mail        *mail.Mailer
	DoubanSync  *doubansync.Service
	Queue       *jobs.Queue

	render          *ui.Renderer
	assetV          string
	modules         moduleMap
	resolve         *httpx.Limiter
	discover        *httpx.Limiter
	searched        *recentSet
	searchLog       *httpx.Limiter
	collectionViews *httpx.Limiter
	images          *imageproxy.Handler
	exampleSub      []byte // 临时：示例订阅，见 example_subscription.go
	topics          shortTopics
	pow             *pow.Issuer
	accounts        *auth.Handler
}

// Options 让测试可以替换外部依赖。
type Options struct {
	// HTTP 是访问外部来源用的客户端，默认带 60 秒超时。
	HTTP *http.Client
	// Douban、TMDB、Wikidata、AI、Embedder、Danmu、Hongguo 为 nil 时按配置创建。
	Douban   *providers.Douban
	TMDB     *providers.TMDB
	Wikidata *providers.Wikidata
	AI       *providers.OpenAI
	Embedder *providers.OpenAI
	Danmu    *providers.Danmu
	Hongguo  *providers.Hongguo
	// TemplateFS 为 nil 时：开发环境读磁盘上的 web/ 目录（改模板不用重启），否则用打包进可执行文件的模板。
	TemplateFS fs.FS
	// PoW 是登录、注册的人机校验。为 nil 时按配置创建；DisablePoW 为 true 时不校验（测试用）。
	PoW        *pow.Issuer
	DisablePoW bool
	// MailTransport 为 nil 时按配置创建：配了 SMTP 用 SMTP，开发环境写日志，生产环境不发信。
	MailTransport mail.Transport
	// MXResolver 为 nil 时用系统 DNS 查收件域名的 MX 记录。
	MXResolver mail.Resolver
	// Telegram 为 nil 时按配置创建（没配置就不备份图片）。
	Telegram *telegram.Client
}

// New 创建应用。
func New(cfg config.Config, pool *db.Pool, log *slog.Logger, options Options) (*App, error) {
	client := options.HTTP
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	app := &App{
		Config: cfg, Pool: pool, Log: log,
		Users: auth.NewStore(pool), Media: media.NewStore(pool), Library: library.NewStore(pool),
		Social: social.NewStore(pool), Collections: collections.NewStore(pool), Feedback: feedback.NewStore(pool),
		Positions: playback.NewStore(pool), Vault: vault.NewStore(pool), AdSkip: adskip.NewStore(pool),
		Reports: report.NewService(pool), Queue: jobs.NewQueue(pool), Trends: trends.NewStore(pool), Ads: ads.NewStore(pool),
		assetV:   strconv.FormatInt(time.Now().Unix(), 36),
		resolve:  httpx.NewLimiter(120, time.Minute),
		discover: httpx.NewLimiter(20, time.Minute),
		searched: newRecentSet(10 * time.Minute), searchLog: httpx.NewLimiter(30, time.Minute),
		collectionViews: httpx.NewLimiter(120, time.Minute),
		images:          imageproxy.New(client, cfg.AppSecret),
		pow:             options.PoW,
	}
	if app.pow == nil && !options.DisablePoW {
		app.pow = pow.New(cfg.PoWBits, pow.MinSolveTime)
	}
	app.Sessions = auth.NewSessions(cfg.AppSecret, cfg.SessionTTL, app.Users)
	app.Mail = newMailer(cfg, pool, log, options)
	tg := options.Telegram
	if tg == nil && cfg.TelegramToken != "" && cfg.TelegramChatID != "" {
		tg = telegram.New(cfg.TelegramToken, cfg.TelegramChatID, cfg.TelegramAPI, nil)
	}
	if tg != nil {
		app.images.Mirror = imageproxy.NewMirror(pool, tg, log)
	}
	app.Recommend = recommend.NewService(pool, app.Media)

	douban := options.Douban
	if douban == nil {
		doubanClient := client
		if cfg.DoubanProxy != "" {
			proxy, err := url.Parse(cfg.DoubanProxy)
			if err != nil || proxy.Host == "" {
				return nil, fmt.Errorf("DOUBAN_PROXY 格式不对，应该像 socks5://127.0.0.1:1080")
			}
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = http.ProxyURL(proxy)
			doubanClient = &http.Client{Timeout: client.Timeout, Transport: transport}
		}
		douban = providers.NewDouban(doubanClient)
	}
	tmdb := options.TMDB
	if tmdb == nil {
		tmdb = providers.NewTMDB(client, cfg.TMDBToken)
	}
	wikidata := options.Wikidata
	if wikidata == nil && cfg.WikidataEndpoint != "" {
		wikidata = &providers.Wikidata{HTTP: client, Endpoint: cfg.WikidataEndpoint, UserAgent: cfg.WikidataUserAgent}
	}
	ai := options.AI
	if ai == nil {
		// 非流式输出需要等模型生成完，给慢模型和上游排队留足时间；任务租约需长于此超时。
		ai = &providers.OpenAI{HTTP: &http.Client{Timeout: 10 * time.Minute}, BaseURL: cfg.AIBaseURL, Key: cfg.AIKey, Model: cfg.AIModel}
	}
	embedder := options.Embedder
	if embedder == nil {
		embedder = &providers.OpenAI{HTTP: &http.Client{Timeout: 3 * time.Minute}, BaseURL: cfg.EmbeddingBaseURL, Key: cfg.EmbeddingKey, Model: cfg.EmbeddingModel}
	}
	danmu := options.Danmu
	if danmu == nil {
		danmu = &providers.Danmu{HTTP: client, Base: cfg.DanmuBase}
	}
	hongguo := options.Hongguo
	if hongguo == nil {
		hongguo = providers.NewHongguo(client)
	}
	app.Ingest = &ingest.Service{Media: app.Media, Queue: app.Queue, Douban: douban, Wikidata: wikidata, TMDB: tmdb, AI: ai, Embedder: embedder,
		Hongguo: hongguo, ShortSync: cfg.HongguoSync, Log: log}
	app.Danmaku = danmaku.NewService(danmaku.NewStore(pool), app.Media, danmu)
	app.DoubanSync = &doubansync.Service{Pool: pool, Users: app.Users, Douban: douban, Catalog: app.Ingest, Queue: app.Queue}

	templates := options.TemplateFS
	reload := false
	if templates == nil {
		templates = web.Files
		if cfg.Dev() {
			if _, err := os.Stat("web/templates/layout.html"); err == nil {
				templates, reload = os.DirFS("web"), true
			}
		}
	}
	modules, err := buildModuleMap(app.assetV)
	if err != nil {
		return nil, fmt.Errorf("整理前端模块: %w", err)
	}
	app.modules = modules
	app.exampleSub = app.loadExampleSubscription()
	renderer, err := ui.New(templates, reload, app.common, app.images.URL)
	if err != nil {
		return nil, fmt.Errorf("加载模板: %w", err)
	}
	renderer.Prepare = app.prepareHead
	app.render = renderer
	return app, nil
}

// common 返回每个页面都需要的数据。
func (a *App) common(c *gin.Context) gin.H {
	user := auth.Current(c)
	// 导航栏上的未读提醒：只在整页渲染时查（HTMX 片段用不到）。
	unread := 0
	if user != nil && !httpx.IsHTMX(c) && !c.GetBool("errorPage") {
		unread, _ = a.Social.Unread(c.Request.Context(), user.ID)
	}
	return gin.H{
		"NavUnread": unread,
		"Site":      a.Config.SiteName,
		"User":      user,
		"CSRF":      httpx.CSRFToken(c),
		"Path":      c.Request.URL.Path,
		"AssetV":    a.assetV,
		"Back":      backTarget(c.Request.URL.Path),
		"Head":      a.modules.head,
	}
}

// doubanKinds 是要向豆瓣发请求的任务。它们共用豆瓣那点请求额度，单独走一条通道：
// 等豆瓣的时候不占别的任务的位置，豆瓣让我们暂停的时候也只停这一条。
var doubanKinds = []string{ingest.KindRefresh, ingest.KindCharts, doubansync.KindSync}

// doubanPause 返回豆瓣还要暂停多久，没有暂停时为 0。
func (a *App) doubanPause() time.Duration {
	if a.Ingest.Douban == nil || a.Ingest.Douban.Pace == nil {
		return 0
	}
	if _, until := a.Ingest.Douban.Pace.Status(); !until.IsZero() {
		return time.Until(until)
	}
	return 0
}

// hongguoPause 返回红果还要暂停多久，没有暂停时为 0。
func (a *App) hongguoPause() time.Duration {
	if a.Ingest.Hongguo == nil || a.Ingest.Hongguo.Pace == nil {
		return 0
	}
	if _, until := a.Ingest.Hongguo.Pace.Status(); !until.IsZero() {
		return time.Until(until)
	}
	return 0
}

// RegisterJobs 把所有任务处理函数注册到 Worker。
func (a *App) RegisterJobs(worker *jobs.Worker) {
	a.Ingest.Register(worker)
	a.DoubanSync.Register(worker)
	worker.Lane(jobs.Lane{
		// 三个执行位：请求的快慢由豆瓣的限速统一控制，多几个位置只是让某个用户的标记很多、
		// 要读很久的时候，别的任务还能接着用豆瓣。
		Kinds: doubanKinds, Loops: 3,
		Hold:     a.doubanPause,
		Postpone: func(err error) bool { return errors.Is(err, providers.ErrThrottled) },
	})
	// 红果单独一条通道：它限流、暂停时只停短剧，不影响豆瓣和别的任务。请求快慢由红果的限速统一控制。
	worker.Lane(jobs.Lane{
		Kinds: ingest.ShortKinds, Loops: 2,
		Hold:     a.hongguoPause,
		Postpone: func(err error) bool { return errors.Is(err, providers.ErrThrottled) },
	})
	// AI 写一段描述要十几秒到一分钟，单独两个执行位，不让它把补资料、算向量这些快任务堵在后面。
	worker.Lane(jobs.Lane{Kinds: []string{ingest.KindSemantic}, Loops: 2})
	worker.Handle("trends.prune", func(ctx context.Context, _ jobs.Job) error { return a.Trends.Prune(ctx) })
	worker.Every("trends.prune", 24*time.Hour)
	worker.Handle("collections.prune-views", func(ctx context.Context, _ jobs.Job) error { return a.Collections.PruneViews(ctx) })
	worker.Every("collections.prune-views", 24*time.Hour)
	worker.Handle("mail.prune", func(ctx context.Context, _ jobs.Job) error { return a.Mail.Prune(ctx) })
	worker.Every("mail.prune", 24*time.Hour)
}

// Router 创建路由。HEAD 请求按 GET 处理，见 httpx.HeadAsGet。
func (a *App) Router() http.Handler {
	if !a.Config.Dev() {
		gin.SetMode(gin.ReleaseMode)
	}
	router := gin.New()
	if err := router.SetTrustedProxies(a.Config.TrustedProxies); err != nil {
		a.Log.Warn("TRUSTED_PROXIES 格式不对，不信任任何代理", "error", err)
		router.SetTrustedProxies(nil)
	}
	router.Use(a.accessLog(), httpx.Compress("/img/"))
	router.Use(httpx.SecurityHeaders(a.Config.Dev(), a.modules.hash))
	router.Use(httpx.ErrorPages(func(c *gin.Context, status int, message string) { a.errorPage(c, status, "", message) }))
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		if !c.Writer.Written() {
			httpx.Error(c, http.StatusInternalServerError, "请稍后重试。")
		}
		c.Abort()
	}))
	router.NoRoute(func(c *gin.Context) {
		httpx.Error(c, http.StatusNotFound, "这个链接对应的页面不存在，或者已经被移除。")
	})

	a.static(router)
	a.images.Register(router)
	router.GET("/healthz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := a.Pool.Ping(ctx); err != nil {
			c.String(http.StatusServiceUnavailable, "database unavailable")
			return
		}
		c.String(http.StatusOK, "ok")
	})
	a.installRoutes(router)
	router.GET("/robots.txt", a.robots)
	router.GET("/sitemap.xml", a.sitemapIndex)
	router.GET("/sitemap-:file", a.sitemapFile)
	router.GET(exampleSubscriptionPath, a.exampleSubscription)

	site := router.Group("", a.Sessions.Load(), httpx.CSRF())
	accounts := auth.NewHandler(a.Users, a.Sessions, a.render)
	accounts.PoW, accounts.Mail = a.pow, a.Mail
	accounts.Secret, accounts.SiteURL, accounts.SiteName = []byte(a.Config.AppSecret), a.Config.SiteURL, a.Config.SiteName
	accounts.Register(site)
	a.accounts = accounts
	a.publicRoutes(site)

	// 登录墙：下面的所有页面和接口都需要登录。
	member := site.Group("", auth.Require())
	a.memberRoutes(member)
	playback.NewHandler(a.Positions).Register(site)
	vaultHandler := vault.NewHandler(a.Vault)
	vaultHandler.Register(site)
	vaultHandler.RegisterKeyring(site)
	site.GET("/api/example-subscription", auth.Require(), a.pendingExampleSubscription) // 临时，见 example_subscription.go
	adskip.NewHandler(a.AdSkip).Register(site)
	relay.New().Register(site)
	danmaku.NewHandler(a.Danmaku).Register(site)

	admin := member.Group("/admin", auth.RequireAdmin())
	a.adminRoutes(admin)
	return httpx.HeadAsGet(router)
}

// accessLog 记录访问日志。只记方法、路径、状态码和耗时；不记查询参数和请求体，
// 这样搜索词、广告指纹投票等内容不会进日志。
func (a *App) accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		status := c.Writer.Status()
		if status < 400 && !a.Config.Dev() {
			return
		}
		attrs := []any{"method", c.Request.Method, "path", c.Request.URL.Path, "status", status, "ms", time.Since(start).Milliseconds()}
		if len(c.Errors) > 0 {
			attrs = append(attrs, "error", c.Errors.String())
		}
		if status >= 500 {
			a.Log.Error("请求出错", attrs...)
		} else {
			a.Log.Info("请求", attrs...)
		}
	}
}

func (a *App) static(router *gin.Engine) {
	var files fs.FS
	if a.Config.Dev() {
		if _, err := os.Stat("web/static"); err == nil {
			files = os.DirFS("web/static")
		}
	}
	if files == nil {
		files, _ = fs.Sub(web.Files, "static")
	}
	server := http.StripPrefix("/static/", http.FileServer(http.FS(files)))
	var mu sync.Mutex
	etags := map[string]string{}
	etag := func(name string) string {
		mu.Lock()
		defer mu.Unlock()
		if value, ok := etags[name]; ok && !a.Config.Dev() {
			return value
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return ""
		}
		sum := sha256.Sum256(body)
		etags[name] = `"` + hex.EncodeToString(sum[:8]) + `"`
		return etags[name]
	}
	serve := func(c *gin.Context, name string) {
		// 带版本号的地址内容不会变，可以长期缓存；其余的每次用 ETag 确认有没有变化。
		if c.Query("v") != "" && !a.Config.Dev() {
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			c.Header("Cache-Control", "no-cache")
		}
		if value := etag(name); value != "" {
			c.Header("ETag", value)
			if c.GetHeader("If-None-Match") == value {
				c.Status(http.StatusNotModified)
				return
			}
		}
		c.Request.URL.Path = "/static/" + name
		server.ServeHTTP(c.Writer, c.Request)
	}
	router.GET("/static/*path", func(c *gin.Context) { serve(c, strings.TrimPrefix(c.Param("path"), "/")) })
	// 不少手机浏览器添加到桌面时不看页面里的 <link>，直接到网站根路径下取这几个固定文件名的图标。
	for path, name := range map[string]string{
		"/apple-touch-icon.png":             "img/apple-touch-icon.png",
		"/apple-touch-icon-precomposed.png": "img/apple-touch-icon.png",
		"/favicon.ico":                      "img/favicon-64.png",
	} {
		router.GET(path, func(c *gin.Context) { serve(c, name) })
	}
}

// fail 记录错误并返回 500。
func (a *App) fail(c *gin.Context, err error) {
	c.Error(err)
	httpx.Error(c, http.StatusInternalServerError, "请稍后再试。如果问题持续发生，可以提交反馈。")
}

// newMailer 按配置创建发信器。
func newMailer(cfg config.Config, pool *db.Pool, log *slog.Logger, options Options) *mail.Mailer {
	transport := options.MailTransport
	if transport == nil {
		switch {
		case cfg.SMTPHost != "":
			transport = &mail.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword}
		case cfg.Dev():
			transport = mail.LogTransport{Log: log}
		}
	}
	from := cfg.MailFrom
	if from == "" {
		host := "localhost"
		if parsed, err := url.Parse(cfg.SiteURL); err == nil && parsed.Hostname() != "" {
			host = parsed.Hostname()
		}
		from = (&netmail.Address{Name: cfg.SiteName, Address: "noreply@" + host}).String()
	}
	limits := mail.DefaultLimits
	if cfg.MailHourlyLimit > 0 {
		limits.PerHour = cfg.MailHourlyLimit
	}
	return mail.New(transport, mail.NewChecker(options.MXResolver), pool, from, limits, log)
}

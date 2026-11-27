package app

// 浏览器插件（仓库里的 extension/ 目录）：下载和说明页、连接账号的页面、插件调用的 /api/ext/ 接口。设计见 docs/extension.md。
//
// 插件不用网站的登录 Cookie，也不过 CSRF 校验：它在"连接插件"页面拿到一个凭证，每次请求放在 Authorization 头里。
// 插件交来的豆瓣数据（标记列表、详情接口的回答、详情网页的片段）都是原样的豆瓣内容，由服务器用和自己抓取时相同的代码解析，
// 再做一遍合理性检查才写进作品库。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"moovie/extension"
	"moovie/internal/auth"
	"moovie/internal/collections"
	"moovie/internal/doubansync"
	"moovie/internal/extapi"
	"moovie/internal/httpx"
	"moovie/internal/ingest"
	"moovie/internal/library"
	"moovie/internal/media"
	"moovie/internal/playback"
	"moovie/internal/providers"
)

const (
	// extLendHold 是借给插件的豆瓣任务推迟多久：插件在这段时间里没交回来，服务器照常自己做。
	extLendHold = 10 * time.Minute
	// extTaskLimit 是插件一次领多少个任务。插件大约 8 秒取一个，20 个不到 3 分钟，在 extLendHold 之内。
	extTaskLimit = 20
	// extNeedsRefresh 是资料多久以前同步的，用户在豆瓣上打开这部作品时，插件顺手把页面上的资料交上来。
	extNeedsRefresh = 7 * 24 * time.Hour
	extMaxBody      = 2 << 20
)

// extensionState 是打包好的插件压缩包，按站点地址缓存。
type extensionState struct {
	mu   sync.Mutex
	zip  []byte
	err  error
	done bool
}

func (a *App) extensionZip() ([]byte, error) {
	a.extZip.mu.Lock()
	defer a.extZip.mu.Unlock()
	if !a.extZip.done || a.Config.Dev() {
		a.extZip.zip, a.extZip.err = extension.Zip(siteOrigin(a.Config.SiteURL))
		a.extZip.done = true
	}
	return a.extZip.zip, a.extZip.err
}

// siteOrigin 取站点地址的协议和域名部分。
func siteOrigin(siteURL string) string {
	parsed, err := url.Parse(siteURL)
	if err != nil || parsed.Host == "" {
		return strings.TrimRight(siteURL, "/")
	}
	return parsed.Scheme + "://" + parsed.Host
}

// extensionRoutes 注册插件相关的路由。site 是带登录状态和 CSRF 的页面组，member 要求登录。
// /api/ext/ 回应里的链接都是站内路径（/m/12），插件用自己连着的网站地址拼成完整链接：
// 不用 SITE_URL，否则 SITE_URL 和插件连的不是同一个站时（比如本机开发），作品编号对不上别的站。
func (a *App) extensionRoutes(router *gin.Engine, site, member gin.IRouter) {
	site.GET("/extension", a.extensionPage)
	site.GET("/extension/download", a.extensionDownload)
	site.GET("/contributors", a.contributorsPage)
	member.GET("/extension/connect", a.extensionConnectPage)
	member.POST("/extension/connect", a.extensionConnect)
	member.POST("/settings/extension/revoke", a.extensionRevokeAll)

	api := router.Group("/api/ext")
	api.GET("/version", a.extVersion)
	authed := api.Group("", a.extAuth, httpx.RateLimit(a.extCalls))
	authed.GET("/me", a.extMe)
	authed.POST("/disconnect", a.extDisconnect)
	authed.POST("/douban/bind", a.extBind)
	authed.POST("/douban/interests", a.extInterests)
	authed.POST("/douban/synced", a.extSynced)
	authed.GET("/tasks", a.extTasks)
	authed.POST("/douban/subject", a.extSubject)
	authed.POST("/douban/page", a.extPage)
	authed.GET("/media", a.extMedia)
	authed.POST("/marks", a.extMarks)
	authed.POST("/mark", a.extMark)
	authed.GET("/outbox", a.extOutbox)
	authed.GET("/airing", a.extAiring)
	authed.POST("/collections", a.extImportCollection)
}

// ---------- 页面 ----------

// contributorsPage 是贡献榜：用插件补全资料最多的人，点名字进主页。只列公开了主页的人。
func (a *App) contributorsPage(c *gin.Context) {
	ctx := c.Request.Context()
	list, err := a.Ext.Leaderboard(ctx, 50)
	if err != nil {
		a.fail(c, err)
		return
	}
	data := gin.H{"Title": "贡献榜", "List": list, "SEO": a.pageSEO("/contributors", a.Config.SiteName+"贡献榜：用浏览器插件帮影牛补全作品资料最多的人。")}
	if user := auth.Current(c); user != nil {
		// 自己补全过、但主页没公开时提醒一句：公开了才能上榜
		count, err := a.Ext.Contributions(ctx, user.ID)
		if err != nil {
			a.fail(c, err)
			return
		}
		data["Mine"], data["MyTier"], data["MyPublic"] = count, extapi.TierFor(count), user.IsPublic
	}
	a.render.Page(c, http.StatusOK, "contributors.html", data)
}

func (a *App) extensionPage(c *gin.Context) {
	seo := a.pageSEO("/extension", a.Config.SiteName+"浏览器插件：用你自己的豆瓣登录状态同步想看和看过，在豆瓣电影页显示影牛卡片、海报角标，豆列导入片单，备份豆瓣标记。支持 Chrome 和 Edge。")
	seo.Title, seo.Keywords = "浏览器插件 - 豆瓣同步、豆瓣页面增强", []string{"豆瓣同步", "豆瓣插件", "豆瓣备份", "豆列导入", "Chrome 插件"}
	releases := extension.Releases()
	size := 0
	if body, err := a.extensionZip(); err == nil {
		size = len(body)
	}
	data := gin.H{
		"Title": "浏览器插件", "Nav": "me", "Bare": auth.Current(c) == nil, "Scripts": []string{"pages/extension.js"}, "SEO": seo,
		"Version": extension.Version(), "Releases": releases, "SizeKB": (size + 1023) / 1024, "Folder": extension.Folder,
	}
	if len(releases) > 0 {
		data["Released"] = releases[0].Date
	}
	if user := auth.Current(c); user != nil {
		count, last, err := a.Ext.Connected(c.Request.Context(), user.ID)
		if err != nil {
			a.fail(c, err)
			return
		}
		data["Connected"], data["LastUsed"] = count, last
	}
	a.render.Page(c, http.StatusOK, "extension.html", data)
}

func (a *App) extensionDownload(c *gin.Context) {
	body, err := a.extensionZip()
	if err != nil {
		a.fail(c, err)
		return
	}
	c.Header("Cache-Control", "no-cache")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="moovie-extension-%s.zip"`, extension.Version()))
	c.Data(http.StatusOK, "application/zip", body)
}

func (a *App) extensionConnectPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	a.render.Page(c, http.StatusOK, "extension_connect.html", gin.H{"Title": "连接浏览器插件", "Nav": "me", "Scripts": []string{"pages/extension.js"}, "SEO": SEO{NoIndex: true}})
}

// extensionConnect 签发一个凭证，放在页面里交给插件（插件在本站页面上的脚本读走它）。凭证只在这个回应里出现一次。
func (a *App) extensionConnect(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	user := auth.Current(c)
	token, err := a.Ext.Issue(c.Request.Context(), user.ID, user.SessionEpoch, c.Request.UserAgent())
	if err != nil {
		a.fail(c, err)
		return
	}
	a.render.Page(c, http.StatusOK, "extension_connect.html", gin.H{"Title": "连接浏览器插件", "Nav": "me", "Scripts": []string{"pages/extension.js"}, "SEO": SEO{NoIndex: true},
		"Token": token})
}

func (a *App) extensionRevokeAll(c *gin.Context) {
	if err := a.Ext.RevokeAll(c.Request.Context(), auth.Current(c).ID); err != nil {
		c.Error(err)
		settingsDone(c, "extension", "failed")
		return
	}
	settingsDone(c, "extension", "")
}

// ---------- 接口：凭证 ----------

// extAuth 校验插件凭证，把用户放进请求上下文（auth.Current 照常可用）。
func (a *App) extAuth(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	token, _ := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
	userID, epoch, err := a.Ext.Authenticate(c.Request.Context(), token)
	if errors.Is(err, extapi.ErrNoToken) {
		httpx.Error(c, http.StatusUnauthorized, "插件还没有连接影牛账号，或者连接已经失效")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	user, err := a.Users.ByID(c.Request.Context(), userID)
	if errors.Is(err, auth.ErrNotFound) || (err == nil && (user.SessionEpoch != epoch || user.Banned())) {
		httpx.Error(c, http.StatusUnauthorized, "连接已经失效（改过密码，或者账号被限制了），请重新连接")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	c.Set("user", user)
	c.Set("extToken", token)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, extMaxBody)
	c.Next()
}

func (a *App) extVersion(c *gin.Context) {
	c.Header("Cache-Control", "no-cache")
	data := gin.H{"version": extension.Version(), "page": "/extension", "download": "/extension/download"}
	if releases := extension.Releases(); len(releases) > 0 {
		data["date"], data["notes"] = releases[0].Date, releases[0].Notes
	}
	c.JSON(http.StatusOK, data)
}

func (a *App) extMe(c *gin.Context) {
	ctx, user := c.Request.Context(), auth.Current(c)
	contributions, err := a.Ext.Contributions(ctx, user.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	synced, err := a.DoubanSync.ExtensionSyncedAt(ctx, user.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"username": user.Username, "avatar": user.Avatar, "douban_user_id": user.DoubanUserID, "contributions": contributions,
		"title": extapi.TierFor(contributions).Title, "synced_at": synced, "profile": "/user/" + strconv.FormatInt(user.ID, 10), "site": a.Config.SiteName,
	})
}

func (a *App) extDisconnect(c *gin.Context) {
	if err := a.Ext.Revoke(c.Request.Context(), c.GetString("extToken")); err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// bindJSON 读请求体，读不出来时回 400。
func bindJSON(c *gin.Context, destination any) bool {
	if err := json.NewDecoder(c.Request.Body).Decode(destination); err != nil {
		httpx.Error(c, http.StatusBadRequest, "请求格式不对")
		return false
	}
	return true
}

// ---------- 接口：同步豆瓣标记 ----------

func (a *App) extBind(c *gin.Context) {
	var body struct {
		DoubanUserID string `json:"douban_user_id"`
	}
	if !bindJSON(c, &body) {
		return
	}
	err := a.DoubanSync.BindFromExtension(c.Request.Context(), auth.Current(c).ID, strings.TrimSpace(body.DoubanUserID))
	if errors.Is(err, doubansync.ErrInvalidUser) {
		httpx.Error(c, http.StatusBadRequest, "豆瓣 ID 不正确")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// extInterests 导入插件读到的一页标记。response 是豆瓣标记列表接口的原始回答。
func (a *App) extInterests(c *gin.Context) {
	var body struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	if !bindJSON(c, &body) {
		return
	}
	user := auth.Current(c)
	if user.DoubanUserID == "" {
		httpx.Error(c, http.StatusConflict, "还没有绑定豆瓣账号")
		return
	}
	interests, total, err := providers.DecodeInterests(body.Response)
	if err != nil || len(interests) > 100 {
		httpx.Error(c, http.StatusBadRequest, "豆瓣标记的格式不对")
		return
	}
	for i := range interests {
		cleanItem(&interests[i].Item)
	}
	progress, changed, err := a.DoubanSync.ImportPage(c.Request.Context(), user.ID, body.Status, interests)
	if errors.Is(err, doubansync.ErrInvalidUser) {
		httpx.Error(c, http.StatusBadRequest, "status 只能是 done 或 mark")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "processed": progress.Processed, "imported": progress.Imported, "failed": progress.Failed, "changed": changed})
}

// cleanItem 整理插件交来的列表资料（片名、年份、海报只用来给新作品占位，之后会被详情覆盖）：去掉不像样的值。
func cleanItem(item *providers.DoubanItem) {
	item.Title = clipRunes(strings.TrimSpace(item.Title), 200)
	if !doubanImage(item.Poster) {
		item.Poster = ""
	}
	if item.Year < 1870 || item.Year > time.Now().Year()+10 {
		item.Year = 0
	}
	if item.Rating < 0 || item.Rating > 10 {
		item.Rating = 0
	}
	if item.Kind != "movie" && item.Kind != "tv" {
		item.Kind = ""
	}
}

func (a *App) extSynced(c *gin.Context) {
	var body struct {
		Failed int `json:"failed"`
	}
	if !bindJSON(c, &body) {
		return
	}
	if body.Failed == 0 {
		if err := a.DoubanSync.ExtensionSynced(c.Request.Context(), auth.Current(c).ID); err != nil {
			a.fail(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------- 接口：补全作品资料 ----------

// extTasks 给插件一批要去豆瓣取详情的条目：先是这个用户自己标记的、片单里的，help=1 时再从服务器排着的豆瓣任务里借一些。
// 有人正等着的任务不借（见 jobs.Queue.Lend）。领走的任务服务器推迟 extLendHold 再做，插件交回来就删掉。
func (a *App) extTasks(c *gin.Context) {
	ctx, user := c.Request.Context(), auth.Current(c)
	limit := min(max(httpx.QueryInt(c, "limit", extTaskLimit), 1), extTaskLimit)
	tasks, err := a.Ext.OwnPending(ctx, user.ID, limit)
	if err != nil {
		a.fail(c, err)
		return
	}
	subjects := make([]string, 0, len(tasks))
	for _, task := range tasks {
		subjects = append(subjects, strconv.FormatInt(task.MediaID, 10))
	}
	if err := a.Queue.Hold(ctx, ingest.KindRefresh, subjects, extLendHold); err != nil {
		a.fail(c, err)
		return
	}
	if c.Query("help") == "1" && len(tasks) < limit {
		lent, err := a.Queue.Lend(ctx, ingest.KindRefresh, ingest.PriorityManual, limit-len(tasks), extLendHold)
		if err != nil {
			a.fail(c, err)
			return
		}
		for _, job := range lent {
			id, err := strconv.ParseInt(job.Subject, 10, 64)
			if err != nil {
				continue
			}
			m, err := a.Media.Get(ctx, id)
			if err != nil || !media.ValidDoubanID(m.DoubanID) || m.HongguoID != "" || m.Hidden {
				continue
			}
			tasks = append(tasks, extapi.Task{MediaID: m.ID, DoubanID: m.DoubanID, Type: m.Type})
		}
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks, "hold_seconds": int(extLendHold.Seconds())})
}

// extSubject 收下插件取回的详情接口回答。kind 是插件请求的是电影还是剧集接口。
// urgent 为 true 表示有人正开着这部作品的页面在等，后面补充资料的步骤也优先。
func (a *App) extSubject(c *gin.Context) {
	var body struct {
		DoubanID string          `json:"douban_id"`
		Kind     string          `json:"kind"`
		Response json.RawMessage `json:"response"`
		Urgent   bool            `json:"urgent"`
	}
	if !bindJSON(c, &body) {
		return
	}
	subject, err := providers.DecodeSubject(body.Response, body.Kind)
	if errors.Is(err, providers.ErrNotFound) {
		// 不据此把作品标成"豆瓣上没有"：那样任何人都能让一部作品从站里消失。交给服务器自己去确认。
		c.JSON(http.StatusOK, gin.H{"applied": false, "reason": "not_found"})
		return
	}
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, "豆瓣详情的格式不对")
		return
	}
	a.applyContribution(c, body.DoubanID, subject, body.Urgent)
}

// extPage 收下插件从用户打开的详情网页上摘出的几块 HTML（片名、海报、信息栏、简介、评分）。
func (a *App) extPage(c *gin.Context) {
	var body struct {
		DoubanID string `json:"douban_id"`
		HTML     string `json:"html"`
		Urgent   bool   `json:"urgent"`
	}
	if !bindJSON(c, &body) {
		return
	}
	if !media.ValidDoubanID(body.DoubanID) || len(body.HTML) > 500_000 {
		httpx.Error(c, http.StatusBadRequest, "请求格式不对")
		return
	}
	subject, err := providers.ParseSubjectPage(body.DoubanID, []byte(body.HTML))
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, "没能从页面里读出资料")
		return
	}
	a.applyContribution(c, body.DoubanID, subject, body.Urgent)
}

// applyContribution 检查插件交来的详情，合理就写进作品库。
func (a *App) applyContribution(c *gin.Context, doubanID string, subject *providers.DoubanSubject, urgent bool) {
	ctx, user := c.Request.Context(), auth.Current(c)
	if !media.ValidDoubanID(doubanID) || subject.ID != doubanID {
		httpx.Error(c, http.StatusBadRequest, "条目编号对不上")
		return
	}
	if problem := checkSubject(subject); problem != "" {
		httpx.Error(c, http.StatusBadRequest, "资料不合理："+problem)
		return
	}
	if !a.extUploads.Allow(strconv.FormatInt(user.ID, 10)) {
		httpx.Error(c, http.StatusTooManyRequests, "今天交的资料已经够多了，明天再继续")
		return
	}
	m, err := a.Media.ByDoubanID(ctx, doubanID)
	if errors.Is(err, media.ErrNotFound) {
		item := providers.DoubanItem{ID: doubanID, Title: subject.Title, Year: subject.Year, Poster: subject.Poster, Kind: subject.Kind}
		var id int64
		if id, err = a.Ingest.EnsureDouban(ctx, item, "", ingest.PriorityImport); err == nil {
			m, err = a.Media.Get(ctx, id)
		}
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	if m.Hidden || m.HongguoID != "" || m.MetadataStatus == "not_found" {
		c.JSON(http.StatusOK, gin.H{"applied": false, "reason": "hidden"})
		return
	}
	// 已经有片名的作品（豆瓣列表、Wikidata、NeoDB、TMDB 给的，或者豆瓣同步过）要对得上才采用；
	// 只有豆瓣 ID、还没有片名的，交来的就是第一份资料，照收。
	if m.Title != "" {
		names, err := a.Media.Aliases(ctx, m.ID, 200)
		if err != nil {
			a.fail(c, err)
			return
		}
		if !consistentSubject(m, names, subject) {
			httpx.Error(c, http.StatusConflict, "和本站已有的资料对不上，没有采用")
			return
		}
	}
	priority := ingest.PriorityImport
	if urgent {
		priority = ingest.PriorityViewing
	}
	if err := a.Ingest.ApplyContributed(ctx, m.ID, subject, priority); err != nil {
		a.fail(c, err)
		return
	}
	if err := a.Ext.RecordContribution(ctx, user.ID, m.ID); err != nil {
		c.Error(err)
	}
	c.JSON(http.StatusOK, gin.H{"applied": true, "media_id": m.ID, "url": mediaHref(m.ID)})
}

// checkSubject 检查详情是不是像豆瓣给的：长度、数量、年份、海报地址。返回问题，没问题返回空字符串。
func checkSubject(s *providers.DoubanSubject) string {
	tooLong := func(values []string, count, runes int) bool {
		if len(values) > count {
			return true
		}
		for _, value := range values {
			if utf8.RuneCountInString(value) > runes {
				return true
			}
		}
		return false
	}
	switch {
	case strings.TrimSpace(s.Title) == "" || utf8.RuneCountInString(s.Title) > 200 || utf8.RuneCountInString(s.OriginalTitle) > 200:
		return "片名"
	case utf8.RuneCountInString(s.Intro) > 8000:
		return "简介太长"
	case tooLong(s.AKA, 60, 200), tooLong(s.Genres, 30, 30), tooLong(s.Countries, 30, 50), tooLong(s.Directors, 60, 100), tooLong(s.Actors, 300, 100):
		return "名单"
	case s.Year != 0 && (s.Year < 1870 || s.Year > time.Now().Year()+10):
		return "年份"
	case s.Rating < 0 || s.Rating > 10, s.EpisodesCount < 0 || s.EpisodesCount > 20000, s.DurationMinutes < 0 || s.DurationMinutes > 3000:
		return "数字"
	case s.Poster != "" && !doubanImage(s.Poster):
		return "海报地址"
	}
	return ""
}

// doubanImage 判断是不是豆瓣的图片地址。
func doubanImage(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && strings.HasSuffix(parsed.Hostname(), ".doubanio.com")
}

// consistentSubject 判断交来的详情和已有资料的作品是不是同一部：片名或原名是这部作品已知的名字之一
// （片名、原名和各来源给的别名，names），年份最多差一年。
// 对不上的不采用：多半是假资料；真是豆瓣改了片名，服务器自己的同步会更新。
func consistentSubject(m media.Media, names []string, s *providers.DoubanSubject) bool {
	known := map[string]bool{}
	for _, name := range append([]string{m.Title, m.OriginalTitle}, names...) {
		if normalized := media.Normalize(name); normalized != "" {
			known[normalized] = true
		}
	}
	sameTitle := known[media.Normalize(s.Title)] || known[media.Normalize(s.OriginalTitle)]
	year := m.Year == 0 || s.Year == 0 || (m.Year-s.Year <= 1 && s.Year-m.Year <= 1)
	return sameTitle && year
}

func clipRunes(value string, limit int) string {
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

// ---------- 接口：豆瓣页面上的影牛卡片和角标 ----------

type extCard struct {
	Title string `json:"title"`
	Year  int    `json:"year,omitempty"`
	URL   string `json:"url"`
}

// extMedia 给豆瓣详情页上的影牛卡片：本站有没有这部、你的标记和进度、收录它的片单、相似推荐。
func (a *App) extMedia(c *gin.Context) {
	ctx, user, doubanID := c.Request.Context(), auth.Current(c), c.Query("douban_id")
	if !media.ValidDoubanID(doubanID) {
		httpx.Error(c, http.StatusBadRequest, "豆瓣 ID 不对")
		return
	}
	m, err := a.Media.ByDoubanID(ctx, doubanID)
	if errors.Is(err, media.ErrNotFound) || (err == nil && (m.Hidden || m.MetadataStatus == "not_found")) {
		c.JSON(http.StatusOK, gin.H{"found": false, "needs_data": err != nil})
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	entry, marked, err := a.Library.Get(ctx, user.ID, m.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	data := gin.H{
		"found": true, "id": m.ID, "title": m.Title, "year": m.Year, "type": m.Type, "url": mediaHref(m.ID),
		"play_url": "/play/" + strconv.FormatInt(m.ID, 10), "rating_tmdb": m.RatingTMDB,
		"needs_data": m.MetadataStatus != "complete" || m.MetadataSyncedAt == nil || time.Since(*m.MetadataSyncedAt) > extNeedsRefresh,
		"complete":   m.MetadataStatus == "complete",
	}
	if marked {
		data["status"], data["rating"] = entry.Status, entry.Rating
	}
	positions, err := a.Positions.ForMedia(ctx, user.ID, m.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	if progress := progressLabel(m, positions); progress != "" {
		data["progress"] = progress
	}
	lists, err := a.Collections.ContainingMedia(ctx, m.ID, 3)
	if err != nil {
		a.fail(c, err)
		return
	}
	cards := []extCard{}
	for _, list := range lists {
		cards = append(cards, extCard{Title: list.Title, URL: "/list/" + list.Slug})
	}
	data["collections"] = cards
	similar := []extCard{}
	if m.MetadataStatus == "complete" {
		found, err := a.Media.Similar(ctx, m, 6)
		if err != nil {
			c.Error(err) // 相似推荐只是锦上添花，查不出来不影响卡片
		}
		for _, card := range found {
			similar = append(similar, extCard{Title: card.Title, Year: card.Year, URL: mediaHref(card.ID)})
		}
	}
	data["similar"] = similar
	c.JSON(http.StatusOK, data)
}

// progressLabel 写成"看到第 3 集"或"看到 42 分钟"，没看过返回空字符串。
func progressLabel(m media.Media, positions []playback.Position) string {
	var latest *playback.Position
	for i := range positions {
		if latest == nil || positions[i].At > latest.At {
			latest = &positions[i]
		}
	}
	if latest == nil {
		return ""
	}
	if m.Type == media.TypeMovie {
		if latest.Completed {
			return "已看完"
		}
		return fmt.Sprintf("看到 %d 分钟", int(latest.Position/60))
	}
	return fmt.Sprintf("看到第 %d %s", latest.Episode, media.EpisodeUnit(m.Type))
}

// extMarks 给海报角标：一批豆瓣条目里，你在本站标记过或看过的。
func (a *App) extMarks(c *gin.Context) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if !bindJSON(c, &body) {
		return
	}
	ids := make([]string, 0, len(body.IDs))
	for _, id := range body.IDs {
		if media.ValidDoubanID(id) && len(ids) < 300 {
			ids = append(ids, id)
		}
	}
	marks, err := a.Ext.Marks(c.Request.Context(), auth.Current(c).ID, ids)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"marks": marks})
}

// extMark 在豆瓣页面的影牛卡片上标记想看、看过，或者取消。本站还没有这部作品时先收录。
func (a *App) extMark(c *gin.Context) {
	var body struct {
		DoubanID string `json:"douban_id"`
		Status   string `json:"status"`
		Rating   int    `json:"rating"`
		Title    string `json:"title"`
		Year     int    `json:"year"`
		Poster   string `json:"poster"`
	}
	if !bindJSON(c, &body) {
		return
	}
	if !media.ValidDoubanID(body.DoubanID) || (body.Status != "" && body.Status != library.StatusWish && body.Status != library.StatusWatched) {
		httpx.Error(c, http.StatusBadRequest, "请求格式不对")
		return
	}
	ctx, user := c.Request.Context(), auth.Current(c)
	item := providers.DoubanItem{ID: body.DoubanID, Title: body.Title, Year: body.Year, Poster: body.Poster}
	cleanItem(&item)
	id, err := a.Ingest.EnsureDouban(ctx, item, "", ingest.PriorityDiscover)
	if err != nil {
		a.fail(c, err)
		return
	}
	switch body.Status {
	case "":
		err = a.Library.Remove(ctx, user.ID, id)
	case library.StatusWish:
		_, err = a.Library.Save(ctx, user.ID, id, library.StatusWish, 0, "")
	default:
		// 已经写过短评的，保留原来的短评
		entry, _, getErr := a.Library.Get(ctx, user.ID, id)
		if getErr != nil {
			a.fail(c, getErr)
			return
		}
		rating := min(max(body.Rating, 0), 5)
		if rating == 0 {
			rating = entry.Rating // 卡片上点"看过"不带评分，保留原来的评分
		}
		_, err = a.Library.Save(ctx, user.ID, id, library.StatusWatched, rating, entry.Comment)
	}
	if errors.Is(err, library.ErrInvalid) {
		httpx.Error(c, http.StatusBadRequest, "评分不合法")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	if err := a.Recommend.Invalidate(ctx, user.ID); err != nil {
		c.Error(err)
	}
	c.JSON(http.StatusOK, gin.H{"status": body.Status, "media_id": id, "url": mediaHref(id)})
}

// ---------- 接口：写回豆瓣、更新提醒、豆列导入 ----------

func (a *App) extOutbox(c *gin.Context) {
	after, _ := strconv.ParseInt(c.Query("after"), 10, 64)
	items, cursor, err := a.Ext.Outbox(c.Request.Context(), auth.Current(c).ID, after, 50)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "cursor": cursor})
}

// queueOutbox 记下用户在本站的一次标记，等插件写回豆瓣。出错只记日志：写回豆瓣是附带的，不能让标记本身失败。
func (a *App) queueOutbox(c *gin.Context, m media.Media, status string, rating int) {
	if err := a.Ext.QueueOutbox(c.Request.Context(), auth.Current(c).ID, m.DoubanID, status, rating); err != nil {
		c.Error(err)
	}
}

func (a *App) extAiring(c *gin.Context) {
	list, err := a.Ext.Airing(c.Request.Context(), auth.Current(c).ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	type item struct {
		extapi.Airing
		URL  string `json:"url"`
		Unit string `json:"unit"`
	}
	items := make([]item, 0, len(list))
	for _, airing := range list {
		items = append(items, item{Airing: airing, URL: mediaHref(airing.MediaID), Unit: media.EpisodeUnit(airing.Type)})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// extImportCollection 把豆列导入成一个私密片单（草稿），返回编辑页地址，用户在那里检查、补推荐语、再决定要不要投稿。
func (a *App) extImportCollection(c *gin.Context) {
	var body struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Items       []struct {
			DoubanID string `json:"douban_id"`
			Title    string `json:"title"`
			Year     int    `json:"year"`
			Poster   string `json:"poster"`
		} `json:"items"`
	}
	if !bindJSON(c, &body) {
		return
	}
	title := clipRunes(strings.TrimSpace(body.Title), 80)
	if title == "" {
		title = "从豆瓣导入的片单"
	}
	ctx, user := c.Request.Context(), auth.Current(c)
	draft := collections.Draft{OwnerID: user.ID, Title: title, Description: clipRunes(strings.TrimSpace(body.Description), 1000)}
	seen := map[int64]bool{}
	for _, entry := range body.Items {
		if len(draft.MediaIDs) >= 200 || !media.ValidDoubanID(entry.DoubanID) {
			continue
		}
		item := providers.DoubanItem{ID: entry.DoubanID, Title: entry.Title, Year: entry.Year, Poster: entry.Poster}
		cleanItem(&item)
		id, err := a.Ingest.EnsureDouban(ctx, item, "", ingest.PriorityImport)
		if err != nil {
			a.fail(c, err)
			return
		}
		if !seen[id] {
			seen[id] = true
			draft.MediaIDs = append(draft.MediaIDs, id)
		}
	}
	if len(draft.MediaIDs) == 0 {
		httpx.Error(c, http.StatusBadRequest, "豆列里没有找到影视条目")
		return
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		a.fail(c, err)
		return
	}
	draft.Slug = "list-" + hex.EncodeToString(suffix[:])
	id, err := a.Collections.Save(ctx, draft)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "count": len(draft.MediaIDs), "edit_url": "/me/collections/" + strconv.FormatInt(id, 10)})
}

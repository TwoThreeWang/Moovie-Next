package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
	"moovie/internal/feedback"
	"moovie/internal/httpx"
	"moovie/internal/ingest"
	"moovie/internal/jobs"
	"moovie/internal/media"
	"moovie/internal/providers"
	"moovie/internal/social"
)

const adminPageSize = 30

func (a *App) adminRoutes(r gin.IRouter) {
	r.GET("", a.adminDashboard)
	r.POST("/tasks", a.adminTask)
	r.GET("/users", a.adminUsers)
	r.POST("/users/:id/role", a.adminUserRole)
	r.POST("/users/:id/delete", a.adminUserDelete)
	r.POST("/users/:id/reset-link", a.adminUserResetLink)
	r.POST("/users/:id/restrict", a.adminUserRestrict)
	r.GET("/content", a.adminContent)
	r.POST("/content/delete", a.adminContentDelete)
	r.GET("/fingerprints", a.adminFingerprints)
	r.GET("/media", a.adminMedia)
	r.POST("/media/add", a.adminMediaAdd)
	r.POST("/media/:id/refresh", a.adminMediaRefresh)
	r.POST("/media/:id/hidden", a.adminMediaHidden)
	r.GET("/jobs", a.adminJobs)
	r.POST("/jobs/retry", a.adminJobsRetry)
	r.GET("/filters", a.adminFilters)
	r.POST("/filters", a.adminFilterAdd)
	r.POST("/filters/:id/delete", a.adminFilterDelete)
	r.GET("/feedback", a.adminFeedback)
	r.POST("/feedback/:id", a.adminFeedbackRespond)
	r.GET("/collections", a.adminCollections)
	r.GET("/collections/:id", a.adminCollectionReview)
	r.POST("/collections/:id", a.adminCollectionDecision)
}

// adminBack 回到来源页面，并带上一条提示。
func adminBack(c *gin.Context, fallback, notice string) {
	target := fallback
	if referer, err := url.Parse(c.GetHeader("Referer")); err == nil && strings.HasPrefix(referer.Path, "/admin") {
		query := referer.Query()
		query.Del("notice")
		referer.RawQuery = query.Encode()
		target = referer.RequestURI()
	}
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	c.Redirect(http.StatusSeeOther, target+separator+"notice="+url.QueryEscape(notice))
}

func (a *App) adminPage(c *gin.Context, template, title, section string, data gin.H) {
	data["Title"], data["Nav"], data["Section"], data["Notice"] = title+" · 管理后台", "admin", section, c.Query("notice")
	data["CSS"] = []string{"admin"}
	a.render.Page(c, http.StatusOK, template, data)
}

func (a *App) adminDashboard(c *gin.Context) {
	ctx := c.Request.Context()
	_, userCount, err := a.Users.List(ctx, "", 1, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	total, complete, hidden, embedded, err := a.Media.Counts(ctx)
	if err != nil {
		a.fail(c, err)
		return
	}
	jobCounts, err := a.Queue.Counts(ctx)
	if err != nil {
		a.fail(c, err)
		return
	}
	openFeedback, err := a.Feedback.OpenCount(ctx)
	if err != nil {
		a.fail(c, err)
		return
	}
	pendingCollections, err := a.Collections.PendingCount(ctx)
	if err != nil {
		a.fail(c, err)
		return
	}
	fingerprints, confirmed, rejected, err := a.AdSkip.Counts(ctx)
	if err != nil {
		a.fail(c, err)
		return
	}
	mirrored, mirroredBytes := 0, int64(0)
	if a.images.Mirror != nil {
		mirrored, mirroredBytes, _ = a.images.Mirror.Counts(ctx)
	}
	trends, err := a.trends30(ctx)
	if err != nil {
		a.fail(c, err)
		return
	}
	shorts, shortsComplete, shortsWaiting, err := a.Media.ShortCounts(ctx)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.adminPage(c, "admin.html", "概览", "dashboard", gin.H{
		"Shorts": shorts, "ShortsComplete": shortsComplete, "ShortsWaiting": shortsWaiting, "ShortSync": a.Ingest.ShortSync,
		"HongguoPace": pacerStatus(a.Ingest.Hongguo.Pace), "ShortBacklog": a.shortBacklogText(ctx),
		"Trends":   trends,
		"Telegram": a.images.Mirror != nil, "Mirrored": mirrored, "MirroredMB": float64(mirroredBytes) / 1e6,
		"Mail": a.Mail.Enabled(), "DoubanProxy": a.Config.DoubanProxy != "",
		"DoubanPace": pacerStatus(a.Ingest.Douban.Pace), "Backlog": a.backlogText(ctx),
		"Users": userCount, "MediaTotal": total, "MediaComplete": complete, "MediaHidden": hidden, "MediaEmbedded": embedded,
		"Jobs": jobCounts, "OpenFeedback": openFeedback, "PendingCollections": pendingCollections,
		"Fingerprints": fingerprints, "FingerprintsConfirmed": confirmed, "FingerprintsRejected": rejected,
		"TMDB": a.Ingest.TMDB.Enabled(), "AI": a.Ingest.AI.Enabled(), "Embedder": a.Ingest.Embedder.Enabled(),
	})
}

func (a *App) adminTask(c *gin.Context) {
	kinds := map[string]string{"charts": ingest.KindCharts, "sweep": ingest.KindSweep}
	if a.Ingest.ShortSync {
		// 这两个任务只有开了 HONGGUO_SYNC 才注册，没开时不接受，免得排进去没人处理。
		kinds["short-lists"], kinds["short-sitemaps"] = ingest.KindShortLists, ingest.KindShortSitemaps
	}
	kind, ok := kinds[c.PostForm("task")]
	if !ok {
		httpx.Error(c, http.StatusBadRequest, "未知的任务")
		return
	}
	queued, err := a.Queue.Enqueue(c.Request.Context(), kind, "", jobs.Options{Priority: 10})
	if err != nil {
		a.fail(c, err)
		return
	}
	if !queued {
		adminBack(c, "/admin", "这个任务已经在排队或运行中")
		return
	}
	adminBack(c, "/admin", "任务已加入队列")
}

func pager(c *gin.Context, total int) gin.H {
	page, _ := httpx.Page(c, adminPageSize)
	query := c.Request.URL.Query()
	query.Del("notice")
	link := func(target int) string {
		query.Set("page", strconv.Itoa(target))
		return c.Request.URL.Path + "?" + query.Encode()
	}
	pages := (total + adminPageSize - 1) / adminPageSize
	result := gin.H{"Page": page, "Pages": max(1, pages), "Total": total}
	if page > 1 {
		result["Prev"] = link(page - 1)
	}
	if page < pages {
		result["Next"] = link(page + 1)
	}
	return result
}

// ---------- 用户 ----------

func (a *App) adminUsers(c *gin.Context) { a.renderAdminUsers(c, gin.H{}) }

func (a *App) renderAdminUsers(c *gin.Context, data gin.H) {
	_, offset := httpx.Page(c, adminPageSize)
	users, total, err := a.Users.List(c.Request.Context(), c.Query("q"), adminPageSize, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	data["Users"], data["Query"], data["Pager"] = users, c.Query("q"), pager(c, total)
	a.adminPage(c, "admin_users.html", "用户", "users", data)
}

// adminUserResetLink 生成重置密码的链接，直接显示在页面上（不放进地址栏，免得留在历史记录里）。
// 用户收不到邮件时，管理员可以通过别的渠道把链接给他。
func (a *App) adminUserResetLink(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	user, err := a.Users.ByID(c.Request.Context(), id)
	if err != nil {
		httpx.Error(c, http.StatusNotFound, "用户不存在")
		return
	}
	data := gin.H{"ResetLink": a.accounts.ResetLink(user), "ResetFor": user.Username}
	if httpx.IsHTMX(c) {
		a.render.Partial(c, "reset_link", data)
		return
	}
	a.renderAdminUsers(c, data)
}

func (a *App) adminUserRole(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	role := c.PostForm("role")
	if role != "user" && role != "admin" {
		httpx.Error(c, http.StatusBadRequest, "角色不正确")
		return
	}
	if id == auth.Current(c).ID {
		adminBack(c, "/admin/users", "不能修改自己的角色")
		return
	}
	if err := a.Users.Update(c.Request.Context(), id, "role", role); err != nil {
		a.fail(c, err)
		return
	}
	adminBack(c, "/admin/users", "角色已更新")
}

func (a *App) adminUserDelete(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	if id == auth.Current(c).ID {
		adminBack(c, "/admin/users", "不能删除自己")
		return
	}
	if err := a.deleteAccount(c.Request.Context(), id); err != nil {
		a.fail(c, err)
		return
	}
	adminBack(c, "/admin/users", "用户已删除（名下的片单转给了其他管理员）")
}

// ---------- 作品 ----------

func (a *App) adminMedia(c *gin.Context) {
	_, offset := httpx.Page(c, adminPageSize)
	kind := c.Query("kind")
	query := c.Query("q")
	// 搜索框里可以直接粘贴豆瓣或红果的链接：取出里面的 ID 来查。
	if _, id := externalRef(query); id != "" {
		query = id
	}
	list, total, err := a.Media.AdminList(c.Request.Context(), query, kind, adminPageSize, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.adminPage(c, "admin_media.html", "作品", "media", gin.H{"List": list, "Query": c.Query("q"), "Kind": kind, "Pager": pager(c, total)})
}

var (
	doubanRef  = regexp.MustCompile(`douban\.com/(?:movie/)?subject/([0-9]{7,9})`)
	hongguoRef = regexp.MustCompile(`series_id=([1-9][0-9]{18})`)
)

// externalRef 认出管理员输入的是哪个来源的作品：豆瓣的 ID 或链接、红果的 series_id 或链接。认不出时 id 为空。
func externalRef(input string) (provider, id string) {
	input = strings.TrimSpace(input)
	switch {
	case media.ValidDoubanID(input):
		return media.ProviderDouban, input
	case media.ValidHongguoID(input):
		return media.ProviderHongguo, input
	}
	if match := doubanRef.FindStringSubmatch(input); match != nil {
		return media.ProviderDouban, match[1]
	}
	if match := hongguoRef.FindStringSubmatch(input); match != nil {
		return media.ProviderHongguo, match[1]
	}
	return "", ""
}

// adminMediaAdd 把一部作品收进作品库：豆瓣的 ID 或链接，或者红果短剧的链接（用户反馈"搜不到某部短剧"时用）。
func (a *App) adminMediaAdd(c *gin.Context) {
	ctx := c.Request.Context()
	var id int64
	var err error
	switch provider, ref := externalRef(c.PostForm("ref")); provider {
	case media.ProviderDouban:
		id, err = a.Ingest.EnsureDouban(ctx, providers.DoubanItem{ID: ref}, "", ingest.PriorityManual)
	case media.ProviderHongguo:
		id, _, err = a.Media.EnsureHongguo(ctx, ref)
	default:
		adminBack(c, "/admin/media", "认不出来：请填豆瓣 ID（7–9 位数字）、豆瓣链接，或者红果短剧的详情页链接")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	if err := a.Ingest.RefreshNow(ctx, id); err != nil {
		a.fail(c, err)
		return
	}
	adminBack(c, "/admin/media", "已加入作品库，资料会在后台补全")
}

func (a *App) adminMediaRefresh(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	if err := a.Ingest.RefreshNow(c.Request.Context(), id); err != nil {
		a.fail(c, err)
		return
	}
	adminBack(c, "/admin/media", "已加入同步队列")
}

func (a *App) adminMediaHidden(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	hidden := c.PostForm("hidden") == "1"
	if err := a.Media.SetHidden(c.Request.Context(), id, hidden); err != nil {
		a.fail(c, err)
		return
	}
	if hidden {
		adminBack(c, "/admin/media", "作品已隐藏")
		return
	}
	adminBack(c, "/admin/media", "作品已恢复展示")
}

// ---------- 任务 ----------

func (a *App) adminJobs(c *gin.Context) {
	_, offset := httpx.Page(c, adminPageSize)
	status := c.Query("status")
	list, err := a.Queue.List(c.Request.Context(), status, adminPageSize, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	counts, err := a.Queue.Counts(c.Request.Context())
	if err != nil {
		a.fail(c, err)
		return
	}
	total := counts[status]
	if status == "" {
		for _, count := range counts {
			total += count
		}
	}
	a.adminPage(c, "admin_jobs.html", "任务", "jobs", gin.H{"Jobs": list, "Status": status, "Counts": counts, "Pager": pager(c, total)})
}

func (a *App) adminJobsRetry(c *gin.Context) {
	id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
	count, err := a.Queue.Retry(c.Request.Context(), id)
	if err != nil {
		a.fail(c, err)
		return
	}
	adminBack(c, "/admin/jobs", "已重新排队 "+strconv.FormatInt(count, 10)+" 个任务")
}

// ---------- 屏蔽词 ----------

// BlockedKeyword 是一条屏蔽词。
type BlockedKeyword struct {
	ID        int64
	Kind      string
	Keyword   string
	CreatedAt time.Time
}

func (a *App) blockedKeywords(ctx context.Context) ([]BlockedKeyword, error) {
	rows, err := a.Pool.Query(ctx, `SELECT id, kind, keyword, created_at FROM blocked_keywords ORDER BY kind, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []BlockedKeyword{}
	for rows.Next() {
		var keyword BlockedKeyword
		if err := rows.Scan(&keyword.ID, &keyword.Kind, &keyword.Keyword, &keyword.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, keyword)
	}
	return list, rows.Err()
}

func (a *App) adminFilters(c *gin.Context) {
	list, err := a.blockedKeywords(c.Request.Context())
	if err != nil {
		a.fail(c, err)
		return
	}
	a.adminPage(c, "admin_filters.html", "屏蔽词", "filters", gin.H{"Keywords": list})
}

// adminFilterAdd 添加屏蔽词，并立刻隐藏片名命中的作品。
func (a *App) adminFilterAdd(c *gin.Context) {
	kind, keyword := c.PostForm("kind"), strings.TrimSpace(c.PostForm("keyword"))
	if (kind != "copyright" && kind != "nsfw") || len([]rune(keyword)) < 2 || len([]rune(keyword)) > 60 {
		adminBack(c, "/admin/filters", "屏蔽词需要 2–60 个字")
		return
	}
	ctx := c.Request.Context()
	if _, err := a.Pool.Exec(ctx, `INSERT INTO blocked_keywords (kind, keyword) VALUES ($1, $2) ON CONFLICT DO NOTHING`, kind, keyword); err != nil {
		a.fail(c, err)
		return
	}
	if err := a.Media.ApplyBlockedKeywords(ctx, 0); err != nil {
		a.fail(c, err)
		return
	}
	adminBack(c, "/admin/filters", "已添加，片名命中的作品已隐藏")
}

// adminFilterDelete 删除屏蔽词。已经隐藏的作品不会自动恢复，需要在作品管理里手动恢复。
func (a *App) adminFilterDelete(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	if _, err := a.Pool.Exec(c.Request.Context(), `DELETE FROM blocked_keywords WHERE id = $1`, id); err != nil {
		a.fail(c, err)
		return
	}
	adminBack(c, "/admin/filters", "已删除。之前被它隐藏的作品需要在作品管理里手动恢复")
}

// ---------- 反馈 ----------

func (a *App) adminFeedback(c *gin.Context) {
	_, offset := httpx.Page(c, adminPageSize)
	status := c.DefaultQuery("status", "open")
	if status == "all" {
		status = ""
	}
	list, err := a.Feedback.List(c.Request.Context(), 0, status, adminPageSize, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	data := gin.H{"List": list, "Status": c.DefaultQuery("status", "open")}
	page, _ := httpx.Page(c, adminPageSize)
	if len(list) == adminPageSize {
		data["Next"] = "/admin/feedback?status=" + c.DefaultQuery("status", "open") + "&page=" + strconv.Itoa(page+1)
	}
	a.adminPage(c, "admin_feedback.html", "反馈", "feedback", data)
}

func (a *App) adminFeedbackRespond(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	userID, replied, err := a.Feedback.Respond(ctx, id, c.PostForm("status"), c.PostForm("reply"))
	if err == nil && replied {
		err = a.Social.Notify(ctx, social.Notice{UserID: userID, ActorID: auth.Current(c).ID, Kind: "feedback", FeedbackID: id, Body: c.PostForm("reply")})
	}
	if errors.Is(err, feedback.ErrInvalid) {
		httpx.Error(c, http.StatusBadRequest, "状态不正确")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	adminBack(c, "/admin/feedback", "已保存")
}

// backlogText 说明还有多少作品在等采集、最早的等了多久、照现在的速度大概多久能做完。
func (a *App) backlogText(ctx context.Context) string {
	count, oldest, err := a.Queue.Backlog(ctx, ingest.KindRefresh)
	if err != nil || count == 0 || oldest == nil {
		return ""
	}
	text := fmt.Sprintf("等待采集的作品 %d 部，最早的已经等了%s", count, roughDuration(time.Since(*oldest)))
	if pause := a.doubanPause(); pause > 0 {
		return text + "；豆瓣暂停中，恢复后继续"
	}
	if d := a.Ingest.Douban; d != nil && d.Pace != nil {
		interval, _ := d.Pace.Status()
		// 一部作品要向豆瓣发一到两次请求，这里按一次半估算。
		text += "；照现在的速度大约还要" + roughDuration(time.Duration(count)*interval*3/2)
	}
	return text
}

func roughDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "不到 1 分钟"
	case d < time.Hour:
		return fmt.Sprintf(" %d 分钟", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf(" %d 小时", int(d.Hours()))
	}
	return fmt.Sprintf(" %d 天", int(d.Hours()/24))
}

// shortBacklogText 说明还有多少部短剧在等红果的详情。一部短剧只发一次请求。
func (a *App) shortBacklogText(ctx context.Context) string {
	count, oldest, err := a.Queue.Backlog(ctx, ingest.KindShortRefresh)
	if err != nil || count == 0 || oldest == nil {
		return ""
	}
	text := fmt.Sprintf("等待抓取的短剧 %d 部，最早的已经等了%s", count, roughDuration(time.Since(*oldest)))
	if pause := a.hongguoPause(); pause > 0 {
		return text + "；红果暂停中，恢复后继续"
	}
	interval, _ := a.Ingest.Hongguo.Pace.Status()
	return text + "；照现在的速度大约还要" + roughDuration(time.Duration(count)*interval)
}

// pacerStatus 是请求节奏的说明：正常、放慢了、暂停中。
func pacerStatus(pace *providers.Pacer) string {
	if pace == nil {
		return ""
	}
	interval, paused := pace.Status()
	switch {
	case !paused.IsZero():
		return "被限流，暂停到 " + paused.Format("15:04")
	case interval > time.Second:
		return "被限流过，请求间隔放慢到 " + interval.Round(time.Second).String()
	}
	return "正常"
}

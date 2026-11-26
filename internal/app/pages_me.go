package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
	"moovie/internal/doubansync"
	"moovie/internal/extapi"
	"moovie/internal/httpx"
	"moovie/internal/library"
	"moovie/internal/mail"
	"moovie/internal/report"
	"moovie/internal/social"
)

func entryViews(items []library.Item) []CardView {
	views := make([]CardView, 0, len(items))
	for _, item := range items {
		sub := defaultSub(item.Card)
		if item.Entry.Rating > 0 {
			sub = stars(item.Entry.Rating)
		}
		views = append(views, CardView{Card: item.Card, Href: mediaHref(item.Card.ID), Sub: sub})
	}
	return views
}

func stars(rating int) string {
	const filled, empty = "★★★★★", "☆☆☆☆☆"
	rating = min(5, max(0, rating))
	return filled[:rating*3] + empty[:(5-rating)*3]
}

func (a *App) mePage(c *gin.Context) {
	ctx, user := c.Request.Context(), auth.Current(c)
	wishCount, watchedCount, err := a.Library.Counts(ctx, user.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	recent, err := a.Positions.Recent(ctx, user.ID, 12, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	wish, err := a.Library.List(ctx, user.ID, library.StatusWish, 16, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	watched, err := a.Library.List(ctx, user.ID, library.StatusWatched, 16, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	unread, err := a.Social.Unread(ctx, user.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	month := time.Now().Format("2006-01")
	monthly, err := a.Reports.Get(ctx, user.ID, month)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.render.Page(c, http.StatusOK, "me.html", gin.H{
		"Title": "我的", "Nav": "me", "WishCount": wishCount, "WatchedCount": watchedCount, "Unread": unread,
		"Recent": wideViews(recent), "Wish": entryViews(wish), "Watched": entryViews(watched),
		"Report": monthly, "Month": month, "Scripts": []string{"pages/home.js"},
	})
}

const listPageSize = 36

// meList 是想看、看过、观看历史的完整列表。
func (a *App) meList(c *gin.Context) {
	ctx, user := c.Request.Context(), auth.Current(c)
	kind := c.Param("kind")
	page, offset := httpx.Page(c, listPageSize)
	data := gin.H{"Nav": "me", "Kind": kind, "Page": page}
	count := 0
	switch kind {
	case library.StatusWish, library.StatusWatched:
		items, err := a.Library.List(ctx, user.ID, kind, listPageSize, offset)
		if err != nil {
			a.fail(c, err)
			return
		}
		data["Cards"], count = entryViews(items), len(items)
		data["Heading"] = map[string]string{"wish": "想看", "watched": "看过"}[kind]
		data["Empty"] = "这里还没有内容。"
	case "history":
		recent, err := a.Positions.Recent(ctx, user.ID, listPageSize, offset)
		if err != nil {
			a.fail(c, err)
			return
		}
		data["Wide"], count = wideViews(recent), len(recent)
		data["Heading"], data["Empty"] = "观看历史", "还没有观看记录。"
		data["Scripts"] = []string{"pages/home.js"}
	default:
		a.render.NotFound(c)
		return
	}
	data["Title"] = data["Heading"]
	if count == listPageSize {
		data["Next"] = "/me/" + kind + "?page=" + strconv.Itoa(page+1)
	}
	if page > 1 {
		data["Prev"] = "/me/" + kind + "?page=" + strconv.Itoa(page-1)
	}
	a.render.Page(c, http.StatusOK, "grid.html", data)
}

func (a *App) notificationsPage(c *gin.Context) {
	ctx, user := c.Request.Context(), auth.Current(c)
	list, err := a.Social.Notifications(ctx, user.ID, 60, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	unread := 0
	for _, n := range list {
		if !n.Read {
			unread++
		}
	}
	a.render.Page(c, http.StatusOK, "notifications.html", gin.H{"Title": "通知", "Nav": "me", "Notifications": list, "UnreadCount": unread})
}

// openNotification 点开一条通知：标为已读，再跳到相关的页面。
func (a *App) openNotification(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	ctx, user := c.Request.Context(), auth.Current(c)
	n, err := a.Social.Notification(ctx, user.ID, id)
	if err != nil {
		c.Redirect(http.StatusFound, "/notifications")
		return
	}
	if err := a.Social.MarkRead(ctx, user.ID, id); err != nil {
		a.fail(c, err)
		return
	}
	c.Redirect(http.StatusFound, n.Target())
}

func (a *App) readNotification(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	ctx, user := c.Request.Context(), auth.Current(c)
	if err := a.Social.MarkRead(ctx, user.ID, id); err != nil {
		a.fail(c, err)
		return
	}
	n, err := a.Social.Notification(ctx, user.ID, id)
	if err != nil {
		c.Status(http.StatusOK)
		return
	}
	c.Header("HX-Trigger", "notes-changed")
	a.render.Partial(c, "notification", gin.H{"N": n})
}

func (a *App) deleteNotification(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	if err := a.Social.DeleteNotification(c.Request.Context(), auth.Current(c).ID, id); err != nil {
		a.fail(c, err)
		return
	}
	c.Header("HX-Trigger", "notes-changed")
	c.Status(http.StatusOK)
}

func (a *App) readAllNotifications(c *gin.Context) {
	if err := a.Social.MarkAllRead(c.Request.Context(), auth.Current(c).ID); err != nil {
		a.fail(c, err)
		return
	}
	back := "/notifications"
	if c.Query("back") == "/me" {
		back = "/me"
	}
	c.Redirect(http.StatusSeeOther, back)
}

func (a *App) clearNotifications(c *gin.Context) {
	if err := a.Social.ClearNotifications(c.Request.Context(), auth.Current(c).ID); err != nil {
		a.fail(c, err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/notifications")
}

// settingsMessages 把设置页操作结果的代码翻译成提示文字。
var settingsMessages = map[string]string{
	"email_invalid":       "邮箱格式不正确",
	"email_undeliverable": mail.ErrUndeliverable.Error(),
	"mail_quota":          "邮件发得太频繁，请过一会儿再试",
	"mail_disabled":       mail.ErrDisabled.Error(),
	"mail_failed":         mail.ErrSend.Error(),
	"username_invalid":    "用户名需要 2–20 个字符，只能包含中文、字母、数字、下划线和连字符",
	"taken":               "邮箱或用户名已被占用",
	"password_wrong":      "当前密码不正确",
	"password_weak":       "新密码至少 8 位",
	"keyring_required":    "请启用浏览器加密并刷新页面，密码与账号配置需要一起更新",
	"password_conflict":   "账号或同步配置已变化，密码尚未修改，请刷新页面后重试",
	"douban_invalid":      doubansync.ErrInvalidUser.Error(),
	"failed":              "保存失败，请重试",
	"avatar_invalid":      "头像只能是一个 emoji",
	"confirm_missing":     "请在确认框里输入“注销”两个字",
	"last_admin":          "你是唯一的管理员，先在后台把别人设为管理员再注销",
}

func (a *App) settingsPage(c *gin.Context) {
	data := gin.H{
		"Title": "设置", "Nav": "me", "Avatars": auth.Avatars, "Welcome": c.Query("welcome") == "1",
		"Saved": c.Query("saved") != "", "Scripts": []string{"pages/settings.js"}, "SiteURL": a.Config.SiteURL,
		"ExampleSubscription": exampleSubscriptionURL,
	}
	if user := auth.Current(c); user != nil && !slices.Contains(auth.Avatars, user.Avatar) {
		data["CustomAvatar"] = user.Avatar
	}
	if user := auth.Current(c); user != nil {
		count, last, err := a.Ext.Connected(c.Request.Context(), user.ID)
		if err != nil {
			a.fail(c, err)
			return
		}
		data["ExtConnected"], data["ExtLastUsed"] = count, last
	}
	if message, ok := settingsMessages[c.Query("error")]; ok {
		data["Error"] = message
	}
	if c.Query("notice") == "email_sent" {
		data["Notice"] = "确认邮件已经发到新邮箱，点邮件里的链接后才会生效（24 小时内有效）。"
	}
	a.render.Page(c, http.StatusOK, "settings.html", data)
}

func settingsDone(c *gin.Context, section, errCode string) {
	query := url.Values{}
	if errCode != "" {
		query.Set("error", errCode)
	} else {
		query.Set("saved", section)
	}
	c.Redirect(http.StatusSeeOther, "/settings?"+query.Encode()+"#"+section)
}

func (a *App) doubanBind(c *gin.Context) {
	err := a.DoubanSync.Bind(c.Request.Context(), auth.Current(c).ID, c.PostForm("douban_user_id"))
	switch {
	case errors.Is(err, doubansync.ErrInvalidUser):
		settingsDone(c, "douban", "douban_invalid")
	case err != nil:
		c.Error(err)
		settingsDone(c, "douban", "failed")
	default:
		settingsDone(c, "douban", "")
	}
}

func (a *App) doubanUnbind(c *gin.Context) {
	if err := a.DoubanSync.Unbind(c.Request.Context(), auth.Current(c).ID); err != nil {
		c.Error(err)
		settingsDone(c, "douban", "failed")
		return
	}
	settingsDone(c, "douban", "")
}

func (a *App) doubanSync(c *gin.Context) {
	user := auth.Current(c)
	if user.DoubanUserID == "" {
		httpx.Error(c, http.StatusBadRequest, "请先绑定豆瓣账号")
		return
	}
	if _, err := a.DoubanSync.Start(c.Request.Context(), user.ID); err != nil {
		a.fail(c, err)
		return
	}
	a.doubanStatus(c)
}

// doubanStatus 返回同步状态片段。同步进行中时，片段会让页面每 2 秒再取一次。
func (a *App) doubanStatus(c *gin.Context) {
	user := auth.Current(c)
	job, found, err := a.Queue.Latest(c.Request.Context(), doubansync.KindSync, strconv.FormatInt(user.ID, 10))
	if err != nil {
		a.fail(c, err)
		return
	}
	var progress doubansync.Progress
	if found {
		json.Unmarshal(job.Progress, &progress)
	}
	a.render.Partial(c, "douban_status", gin.H{
		"Bound": user.DoubanUserID != "", "Found": found, "Job": job, "Progress": progress,
		"Running": found && (job.Status == "queued" || job.Status == "running"),
	})
}

// ---------- 公开主页和月度报告 ----------

func (a *App) findProfile(c *gin.Context) (social.Profile, bool) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return social.Profile{}, false
	}
	profile, err := a.Social.Profile(c.Request.Context(), viewerID(c), id)
	if errors.Is(err, social.ErrNotFound) {
		a.render.NotFound(c)
		return profile, false
	}
	if err != nil {
		a.fail(c, err)
		return profile, false
	}
	return profile, true
}

func canViewWatchProfile(profile social.Profile, user *auth.User) bool {
	return profile.IsPublic || (user != nil && (profile.ID == user.ID || user.IsAdmin()))
}

func (a *App) visibleProfile(c *gin.Context) (social.Profile, bool) {
	profile, ok := a.findProfile(c)
	if !ok {
		return profile, false
	}
	if !canViewWatchProfile(profile, auth.Current(c)) {
		a.errorPage(c, http.StatusForbidden, "这个主页没有公开", profile.Username+" 没有公开自己的观影主页。")
		return profile, false
	}
	return profile, true
}

func (a *App) userPage(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	profile, ok := a.findProfile(c)
	if !ok {
		return
	}
	ctx, viewer := c.Request.Context(), viewerID(c)
	const pageSize = 24
	page, offset := httpx.Page(c, pageSize)
	lists, err := a.Collections.ByAuthor(ctx, profile.ID, pageSize+1, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	path := "/user/" + strconv.FormatInt(profile.ID, 10)
	data := gin.H{
		"SEO":   SEO{Canonical: a.absolute(path), Type: "profile", NoIndex: true},
		"Title": profile.Username, "Nav": "me", "Profile": profile, "Self": profile.ID == viewer,
		"Collections": firstN(lists, pageSize), "ShareURL": a.absolute(path),
	}
	setPager(data, page, len(lists) > pageSize, pageLink(path+"#author-collections"))
	if page > 1 && len(lists) == 0 {
		a.render.NotFound(c)
		return
	}
	if !canViewWatchProfile(profile, auth.Current(c)) {
		if len(lists) == 0 {
			a.errorPage(c, http.StatusForbidden, "这个主页没有公开", profile.Username+" 没有公开自己的观影主页。")
			return
		}
		data["CollectionsOnly"] = true
		a.render.Page(c, http.StatusOK, "user.html", data)
		return
	}
	wishCount, watchedCount, err := a.Library.Counts(ctx, profile.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	watched, err := a.Library.List(ctx, profile.ID, library.StatusWatched, 16, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	wish, err := a.Library.List(ctx, profile.ID, library.StatusWish, 16, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	comments, err := a.Social.UserComments(ctx, viewer, profile.ID, 10, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	months, err := a.Reports.Months(ctx, profile.ID, 12)
	if err != nil {
		a.fail(c, err)
		return
	}
	data["SEO"] = SEO{Description: profile.Username + " 看过 " + strconv.Itoa(watchedCount) + " 部、想看 " + strconv.Itoa(wishCount) + " 部。", Canonical: a.absolute(path), Type: "profile", NoIndex: true}
	data["WishCount"], data["WatchedCount"] = wishCount, watchedCount
	contributions, err := a.Ext.Contributions(ctx, profile.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	data["Contributions"], data["Tier"] = contributions, extapi.TierFor(contributions)
	data["Watched"], data["Wish"], data["Comments"], data["Months"] = entryViews(watched), entryViews(wish), comments, months
	a.render.Page(c, http.StatusOK, "user.html", data)
}

func (a *App) reportPage(c *gin.Context) {
	profile, ok := a.visibleProfile(c)
	if !ok {
		return
	}
	monthly, err := a.Reports.Get(c.Request.Context(), profile.ID, c.Param("month"))
	if errors.Is(err, report.ErrInvalidMonth) {
		a.render.NotFound(c)
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	seo := a.pageSEO(c.Request.URL.Path, monthly.PersonaTitle+"："+monthly.PersonaLine)
	seo.Type, seo.NoIndex = "article", true
	if len(monthly.Posters) > 0 {
		seo.Image = a.absolute(a.images.URL(monthly.Posters[0].URL, "display", monthly.Posters[0].Title, "poster"))
	}
	a.render.Page(c, http.StatusOK, "report.html", gin.H{
		"Title": profile.Username + " 的 " + monthly.Label() + "观影报告", "Nav": "me", "Profile": profile, "Report": monthly, "SEO": seo,
		"ShareURL": a.absolute(c.Request.URL.Path), "Card": a.reportCard(profile, monthly, c.Request.URL.Path),
		"Self": profile.ID == viewerID(c),
	})
}

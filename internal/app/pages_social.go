package app

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
	"moovie/internal/feedback"
	"moovie/internal/httpx"
	"moovie/internal/social"
)

func (a *App) cinemaPage(c *gin.Context) {
	const pageSize = 20
	following := c.Query("tab") == "following"
	page, offset, valid := httpx.ParsePage(c.Query("page"), pageSize)
	if !valid {
		a.render.NotFound(c)
		return
	}
	if following && auth.Current(c) == nil {
		c.Redirect(http.StatusFound, "/auth/login?redirect="+url.QueryEscape(cinemaURL(following, page)))
		return
	}
	if following {
		c.Header("Cache-Control", "private, no-store")
	}
	comments, err := a.Social.Feed(c.Request.Context(), viewerID(c), following, pageSize+1, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	if page > 1 && len(comments) == 0 {
		a.render.NotFound(c)
		return
	}
	seo := a.pageSEO(cinemaURL(following, page), "看看大家最近在 "+a.Config.SiteName+"看了什么：影迷对电影、剧集、综艺和动漫的最新短评与评分，从真实口碑里发现好片。")
	seo.Title, seo.Keywords = "影院 - 影迷最新短评与评分", []string{"影评", "短评", "电影评分", "剧集评分", "口碑", "好片推荐"}
	seo.NoIndex = following
	if following {
		seo.Title = "影院 · 关注"
	}
	if page > 1 {
		seo.Title += " · 第 " + strconv.Itoa(page) + " 页"
	}
	data := gin.H{"Title": "影院", "Nav": "discover", "Comments": firstN(comments, pageSize), "Following": following, "SEO": seo}
	setPager(data, page, len(comments) > pageSize, func(page int) string { return cinemaURL(following, page) })
	a.render.Page(c, http.StatusOK, "cinema.html", data)
}

func cinemaURL(following bool, page int) string {
	query := url.Values{}
	if following {
		query.Set("tab", "following")
	}
	return pagedURL("/cinema", query, page)
}

func (a *App) toggleLike(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	ctx, user := c.Request.Context(), auth.Current(c)
	if _, err := a.Social.ToggleLike(ctx, user.ID, id); errors.Is(err, social.ErrNotFound) {
		httpx.Error(c, http.StatusNotFound, "短评不存在")
		return
	} else if err != nil {
		a.fail(c, err)
		return
	}
	comment, err := a.Social.Comment(ctx, user.ID, id)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.render.Partial(c, "like_button", gin.H{"C": comment})
}

func (a *App) renderReplies(c *gin.Context, commentID int64) {
	replies, err := a.Social.Replies(c.Request.Context(), commentID)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.render.Partial(c, "replies", gin.H{"Replies": replies, "CommentID": commentID})
}

func (a *App) replies(c *gin.Context) {
	if id, ok := httpx.ParamID(c, "id"); ok {
		a.renderReplies(c, id)
	}
}

func (a *App) addReply(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	err := a.Social.AddReply(c.Request.Context(), auth.Current(c).ID, id, c.PostForm("content"))
	if errors.Is(err, social.ErrNotFound) {
		httpx.Error(c, http.StatusNotFound, "短评不存在")
		return
	}
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	a.renderReplies(c, id)
}

func (a *App) deleteReply(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	user := auth.Current(c)
	commentID, err := a.Social.DeleteReply(c.Request.Context(), user.ID, user.IsAdmin(), id, c.Query("reason"))
	if errors.Is(err, social.ErrNotFound) {
		httpx.Error(c, http.StatusNotFound, "回复不存在")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	a.renderReplies(c, commentID)
}

// removeComment 由管理员删除一条短评，并通知作者。
func (a *App) removeComment(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	user := auth.Current(c)
	if !user.IsAdmin() {
		httpx.Error(c, http.StatusForbidden, "只有管理员可以删除别人的短评")
		return
	}
	err := a.Social.RemoveComment(c.Request.Context(), user.ID, id, c.Query("reason"))
	if errors.Is(err, social.ErrNotFound) {
		httpx.Error(c, http.StatusNotFound, "短评不存在")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(`<p class="comment removed hint">这条短评已删除，已通知作者。</p>`))
}

func (a *App) toggleFollow(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	ctx, user := c.Request.Context(), auth.Current(c)
	if _, err := a.Social.ToggleFollow(ctx, user.ID, id); err != nil {
		httpx.Error(c, http.StatusBadRequest, "操作失败")
		return
	}
	profile, err := a.Social.Profile(ctx, user.ID, id)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.render.Partial(c, "follow_button", gin.H{"Profile": profile})
}

// ---------- 反馈 ----------

func (a *App) feedbackPage(c *gin.Context) {
	mine, err := a.Feedback.List(c.Request.Context(), auth.Current(c).ID, "", 30, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.render.Page(c, http.StatusOK, "feedback.html", gin.H{
		"Title": "反馈", "Nav": "me", "Kinds": feedback.Kinds, "Mine": mine, "Sent": c.Query("sent") == "1",
		"Kind": c.Query("kind"),
	})
}

func (a *App) feedbackSubmit(c *gin.Context) {
	err := a.Feedback.Create(c.Request.Context(), auth.Current(c).ID, c.PostForm("kind"), c.PostForm("content"), c.PostForm("page_url"))
	if errors.Is(err, feedback.ErrInvalid) {
		httpx.Error(c, http.StatusBadRequest, "请选择类型并填写内容（最多 5000 字）")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/feedback?sent=1")
}

// ---------- 说明文档页 ----------

type docSection struct {
	Heading string
	Body    []string
}

type doc struct {
	Title       string
	Description string // 页面描述，{site} 换成站点名
	Sections    []docSection
	Contact     bool // 页面末尾显示联系方式（反馈页、邮箱）
}

// docs 是几个静态说明页的内容。
var docs = map[string]doc{
	"about": {Title: "关于", Description: "关于 {site}：一个观影管理工具，记录想看和看过、打分写短评、整理片单、追剧集更新、看月度观影报告；作品资料来自豆瓣、TMDB、IMDb 和 Wikidata。", Sections: []docSection{
		{"这是什么", []string{
			"这是一个观影管理工具：记录想看和看过、给作品打分写短评、整理片单、追更新、看月度观影报告。",
			"作品资料来自豆瓣、TMDB、IMDb 和 Wikidata 等公开的影视资料库。",
		}},
		{"关于播放", []string{
			"本站不提供、不推荐、也不保存任何视频或播放源。",
			"站内的播放器是一个通用播放器。如果你有自己的播放源，可以在设置里填写订阅地址，由你的浏览器直接连接这些源。本站的服务器不会访问它们，也不知道你填了什么。",
		}},
	}},
	"terms": {Title: "服务协议", Description: "{site}服务协议：账号使用、自行配置的播放源、短评与回复等用户发布内容的责任说明。", Sections: []docSection{
		{"账号", []string{"你需要对自己账号下的行为负责，并妥善保管密码。", "我们可以在账号被用于违法或滥用行为时暂停或终止服务。"}},
		{"你自己配置的内容", []string{
			"播放源完全由你自己配置。你需要确认自己有权访问和使用这些内容，并遵守所在地的法律。",
			"本站不对你配置的第三方内容的合法性、可用性或安全性负责。",
		}},
		{"你发布的内容", []string{"短评、回复、弹幕等内容由发布者负责。违法、侵权或骚扰他人的内容会被删除。"}},
	}},
	"privacy": {Title: "隐私政策", Description: "{site}隐私政策：我们保存哪些数据、哪些数据只在你的设备上加密保存、公开范围，以及如何导出数据和注销账号。", Sections: []docSection{
		{"我们保存什么", []string{
			"账号信息：邮箱、用户名、加密后的密码。",
			"你的标记：想看、看过、评分、短评、片单。",
			"播放进度：只记录作品、集数和进度，不记录你用的是哪个播放源。",
		}},
		{"我们不保存什么", []string{
			"你的订阅地址和设备密钥：为了在你登录的各台设备之间同步，会先在你的浏览器里用登录密码加密，再把密文保存到服务器。服务器只有密文和加密用的随机数，不保存你的密码（只保存无法还原的哈希），所以解不开。",
			"作品库里没有的作品，其观看历史在你的浏览器里加密后才上传。密钥只在你的设备上，服务器无法解密。",
			"广告跳过功能的指纹库只有指纹和票数，不记录是谁投的票，也不记录指纹来自哪里。",
		}},
		{"公开范围", []string{"你的观影记录和报告默认不公开，可以在设置里打开。新片单仅自己可见，开启链接访问后，持有链接的人可以查看。通过审核的片单会显示在广场和作者主页。公开片单的浏览量按日去重，访客的去重记录仅用于近期热度。短评对所有登录用户可见。"}},
		{"你的权利", []string{
			"导出：在「设置 → 我的数据」可以随时下载你在本站的全部数据（JSON），或者想看和看过的列表（CSV）。加密的观看历史由你的设备解密后一起导出。",
			"注销：在「设置 → 注销账号」可以注销。账号和它的所有数据会立即删除，无法恢复；你提交过的反馈会保留，但不再和你关联。注销时会给你的邮箱发一封确认邮件。",
		}},
	}},
	"dmca": {Title: "版权投诉", Description: "{site}版权投诉说明：本站只展示作品资料，不存储、不传输视频文件；认为内容侵权时如何联系我们处理。", Sections: []docSection{
		{"本站的内容", []string{
			"本站只展示作品的资料信息（片名、简介、海报等），不存储、不传输任何视频文件，也不提供播放源。",
		}},
		{"如何投诉", []string{
			"如果你认为本站展示的某些内容侵犯了你的权利，可以用下面任意一种方式联系我们，请说明权利人、涉及的页面地址、权属证明和联系方式。",
			"我们会在核实后尽快处理，包括不再展示相关条目。",
		}},
	}, Contact: true},
}

func (a *App) docPage(slug string) gin.HandlerFunc {
	return func(c *gin.Context) {
		a.render.Page(c, http.StatusOK, "doc.html", gin.H{"Title": docs[slug].Title, "Doc": docs[slug], "Bare": auth.Current(c) == nil, "Nav": "me",
			"ContactEmail": a.Config.ContactEmail, "SEO": a.pageSEO("/"+slug, strings.ReplaceAll(docs[slug].Description, "{site}", a.Config.SiteName))})
	}
}

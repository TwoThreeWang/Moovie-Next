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
		"Title": "反馈", "Nav": "me", "Kinds": feedback.Kinds, "Mine": mine, "Kind": c.Query("kind"),
	})
}

func (a *App) feedbackSubmit(c *gin.Context) {
	id, err := a.Feedback.Create(c.Request.Context(), auth.Current(c).ID, c.PostForm("kind"), c.PostForm("content"), c.PostForm("page_url"))
	if errors.Is(err, feedback.ErrInvalid) {
		httpx.Error(c, http.StatusBadRequest, "请选择类型并填写内容（最多 5000 字）")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/feedback/"+strconv.FormatInt(id, 10)+"?sent=1")
}

// feedbackThread 是一条反馈的对话页，只有提交人能看。打开时把这条反馈的通知标为已读。
func (a *App) feedbackThread(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	ctx, user := c.Request.Context(), auth.Current(c)
	f, err := a.Feedback.Get(ctx, id, user.ID)
	if errors.Is(err, feedback.ErrNotFound) {
		a.errorPage(c, http.StatusNotFound, "这条反馈不存在", "可以回到反馈页查看你提交过的反馈。")
		return
	}
	if err == nil {
		err = a.Social.MarkFeedbackRead(ctx, user.ID, id)
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	a.render.Page(c, http.StatusOK, "feedback_thread.html", gin.H{
		"Title": f.KindLabel(), "Nav": "me", "F": f, "Sent": c.Query("sent") == "1", "AutoCloseDays": feedback.AutoCloseDays,
	})
}

// feedbackReply 是提交人在对话里继续回复，已关闭的会重新打开。
func (a *App) feedbackReply(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	user := auth.Current(c)
	_, err := a.Feedback.Reply(c.Request.Context(), id, user.ID, user.ID, false, c.PostForm("content"))
	a.feedbackDone(c, id, err)
}

// feedbackClose 是提交人自己关闭对话（问题已经解决了）。
func (a *App) feedbackClose(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	_, err := a.Feedback.Close(c.Request.Context(), id, auth.Current(c).ID, "")
	a.feedbackDone(c, id, err)
}

func (a *App) feedbackDone(c *gin.Context, id int64, err error) {
	switch {
	case errors.Is(err, feedback.ErrInvalid):
		httpx.Error(c, http.StatusBadRequest, "请填写内容（最多 5000 字）")
	case errors.Is(err, feedback.ErrNotFound):
		httpx.Error(c, http.StatusNotFound, "反馈不存在")
	case err != nil:
		a.fail(c, err)
	default:
		c.Redirect(http.StatusSeeOther, "/feedback/"+strconv.FormatInt(id, 10))
	}
}

// ---------- 说明文档页 ----------

type docSection struct {
	Heading string
	Body    []string
	Items   []docItem // 段落后面的列表（联系方式、其他项目），文字都能直接选中复制
}

// docItem 是列表里的一行：「标签：内容」，内容有 URL 时是链接（http 开头的在新标签页打开），Note 是后面的补充说明。
type docItem struct {
	Label string
	Text  string
	URL   string
	Note  string
}

type doc struct {
	Title       string
	Description string // 页面描述，{site} 换成站点名
	Sections    []docSection
	Contact     *docContact // 页面末尾显示联系方式（反馈页、邮箱），为 nil 时不显示
}

// docContact 是页面末尾的两种联系方式：站内反馈（要登录）和邮件（不用注册）。
type docContact struct {
	FeedbackKind string // 打开反馈页时预选的类型，空表示不预选
	FeedbackHint string
	MailSubject  string
	MailHint     string
}

// docs 是几个静态说明页的内容。
var docs = map[string]doc{
	"about": {Title: "关于", Description: "关于 {site}：一个观影管理工具，记录想看和看过、打分写短评、整理片单、追剧集更新、看月度观影报告；作品资料来自豆瓣、TMDB、IMDb 和 Wikidata。", Sections: []docSection{
		{Heading: "这是什么", Body: []string{
			"这是一个观影管理工具：记录想看和看过、给作品打分写短评、整理片单、追剧集更新、看月度观影报告。",
			"作品资料来自豆瓣、TMDB、IMDb 和 Wikidata 等公开的影视资料库。装上浏览器插件，还能把你在豆瓣的标记同步过来。",
			"手机、平板和电脑都能用，也可以添加到桌面，用起来像一个 App。",
		}},
		{Heading: "名称的由来", Body: []string{
			"Moovie 来自 Movie，多出来的那个 o 代表 more：更多想看的，更多看过的。",
			"念快了像奶牛“哞”的一声，所以中文名叫影牛，Logo 也是一头牛。",
		}},
		{Heading: "我们的理念", Body: []string{
			"看过的电影会忘，想看的片子会丢。我们想做一个安静的地方，把你看过的、想看的、喜欢的都好好记下来，过几年回头翻，还能想起当时的心情。",
			"不追踪你，不卖你的数据。观影记录默认只有你自己能看到，全部数据随时可以导出带走。",
		}},
		{Heading: "关于播放", Body: []string{
			"本站不提供、不推荐、也不保存任何视频或播放源。",
			"站内的播放器是一个通用播放器。如果你有自己的播放源，可以在设置里填写订阅地址，由你的浏览器直接连接这些源。本站的服务器不会访问它们，也不知道你填了什么。",
		}},
		{Heading: "关于广告", Body: []string{
			"为了支付服务器、带宽和图片存储的费用，本站在作品详情页和播放页的侧栏放了少量广告，每一条都标有“广告”。",
			"广告是本站人工添加的图片或文字，不加载第三方广告脚本。我们不追踪你，不会用你的账号信息或观影记录来投放广告，也不会把这些数据提供给广告主。",
			"广告不会出现在视频画面上，不会弹窗，也不会自动跳转。点击后会在新标签页打开广告主的网站，那里的内容和服务由广告主负责。",
			"有的广告图片放在广告主自己的服务器上，加载时对方能看到你的 IP 地址这类一般的访问信息，但看不到你在看哪个页面。",
			"发现虚假、违法或误导的广告，请用下面的联系方式告诉我们，核实后会立刻下架。",
		}},
		{Heading: "广告合作", Body: []string{
			"本站的用户是爱看电影、爱记录的影迷。广告显示在作品详情页和播放页，手机和电脑上都有，适合影视周边、会员服务、数码产品等。",
			"广告可以是一张图片（按 60px 高铺满宽度显示，建议 720×120 左右），也可以是一两行文字，都可以带一个链接。",
			"投放流程：发邮件说明想推广的内容、时长和预算 → 我们回复报价和排期 → 确认后付款，支持 USDT 和 Stripe → 发来素材（图片或文字，加上链接）→ 审核通过后，通常 24 小时内上线。",
			"不接受博彩、色情、诈骗、恶意软件和其他违法的广告；素材不能有弹窗、强制跳转或自动播放的声音。不合适的广告我们有权拒绝。",
			"合作邮件请在标题里注明“广告合作”，一般 1–2 个工作日内回复。",
		}},
		{Heading: "联系我们", Items: []docItem{
			{Label: "邮箱", Text: "support[at]c2v2.com", Note: "把 [at] 换成 @，不需要注册账号"},
			{Label: "站内反馈", Text: "提交反馈", URL: "/feedback", Note: "问题反馈、功能建议、求片、举报广告。需要登录，回复会在通知里告诉你"},
			{Label: "GitHub", Text: "TwoThreeWang/Moovie-Next", URL: "https://github.com/TwoThreeWang/Moovie-Next"},
			{Label: "TG 群", Text: "Moovie 交流群", URL: "https://t.me/+gkAu28AMZS1lOGY1"},
			{Label: "X (Twitter)", Text: "@0xEvanDev", URL: "https://x.com/0xEvanDev"},
		}},
		{Heading: "我的其他项目", Items: []docItem{
			{Label: "竹林", Text: "zhulink.vip", URL: "https://zhulink.vip/", Note: "开源的去算法推荐社区，连接优质内容与真实用户讨论"},
		}},
	}},
	"terms": {Title: "服务协议", Description: "{site}服务协议：账号使用、自行配置的播放源、短评与回复等用户发布内容的责任说明。", Sections: []docSection{
		{Heading: "账号", Body: []string{"你需要对自己账号下的行为负责，并妥善保管密码。", "我们可以在账号被用于违法或滥用行为时暂停或终止服务。"}},
		{Heading: "你自己配置的内容", Body: []string{
			"播放源完全由你自己配置。你需要确认自己有权访问和使用这些内容，并遵守所在地的法律。",
			"本站不对你配置的第三方内容的合法性、可用性或安全性负责。",
		}},
		{Heading: "你发布的内容", Body: []string{"短评、回复、弹幕等内容由发布者负责。违法、侵权或骚扰他人的内容会被删除。"}},
	}},
	"privacy": {Title: "隐私政策", Description: "{site}隐私政策：我们保存哪些数据、哪些数据只在你的设备上加密保存、公开范围，以及如何导出数据和注销账号。", Sections: []docSection{
		{Heading: "我们保存什么", Body: []string{
			"账号信息：邮箱、用户名、加密后的密码。",
			"你的标记：想看、看过、评分、短评、片单。",
			"播放进度：只记录作品、集数和进度，不记录你用的是哪个播放源。",
		}},
		{Heading: "我们不保存什么", Body: []string{
			"你的订阅地址和设备密钥：为了在你登录的各台设备之间同步，会先在你的浏览器里用登录密码加密，再把密文保存到服务器。服务器只有密文和加密用的随机数，不保存你的密码（只保存无法还原的哈希），所以解不开。",
			"作品库里没有的作品，其观看历史在你的浏览器里加密后才上传。密钥只在你的设备上，服务器无法解密。",
			"广告跳过功能的指纹库只有指纹和票数，不记录是谁投的票，也不记录指纹来自哪里。",
		}},
		{Heading: "浏览器插件", Body: []string{
			"浏览器插件是可选的。装了以后，插件用你浏览器里的豆瓣登录状态读你自己的豆瓣标记，把豆瓣返回的内容交给本站导入；不读取、不上传你的豆瓣密码和 Cookie。",
			"在豆瓣页面上，插件把作品的豆瓣 ID 发给本站查你的标记；资料过期时，把页面上的片名、海报、简介等作品资料交给本站，不含你的豆瓣账号信息。插件帮本站补全作品资料时，向豆瓣请求不带 Cookie。本站记录你补全过哪些作品，用于在你的主页显示补全资料的称号和处理错误资料；你的主页公开时，名字、称号和补全数量会出现在贡献榜上。",
			"插件只在豆瓣和本站上运行，不记录你浏览了哪些网页。插件的连接凭证只保存哈希，随时可以在「设置 → 浏览器插件」断开。",
		}},
		{Heading: "站内广告", Body: []string{
			"站内的广告不加载第三方脚本，我们也不记录你看了或点了哪条广告。有的广告图片放在广告主自己的服务器上，加载时对方能看到你的 IP 地址这类一般的访问信息，但看不到你在看哪个页面。",
		}},
		{Heading: "公开范围", Body: []string{"你的观影记录和报告默认不公开，可以在设置里打开。新片单仅自己可见，开启链接访问后，持有链接的人可以查看。通过审核的片单会显示在广场和作者主页。公开片单的浏览量按日去重，访客的去重记录仅用于近期热度。短评对所有登录用户可见。"}},
		{Heading: "你的权利", Body: []string{
			"导出：在「设置 → 我的数据」可以随时下载你在本站的全部数据（JSON），或者想看和看过的列表（CSV）。加密的观看历史由你的设备解密后一起导出。",
			"注销：在「设置 → 注销账号」可以注销。账号和它的所有数据会立即删除，无法恢复；你提交过的反馈会保留，但不再和你关联。注销时会给你的邮箱发一封确认邮件。",
		}},
	}},
	"dmca": {Title: "版权投诉", Description: "{site}版权投诉说明：本站只展示作品资料，不存储、不传输视频文件；认为内容侵权时如何联系我们处理。", Sections: []docSection{
		{Heading: "本站的内容", Body: []string{
			"本站只展示作品的资料信息（片名、简介、海报等），不存储、不传输任何视频文件，也不提供播放源。",
		}},
		{Heading: "如何投诉", Body: []string{
			"如果你认为本站展示的某些内容侵犯了你的权利，可以用下面任意一种方式联系我们，请说明权利人、涉及的页面地址、权属证明和联系方式。",
			"我们会在核实后尽快处理，包括不再展示相关条目。",
		}},
	}, Contact: &docContact{
		FeedbackKind: "dmca",
		FeedbackHint: "在反馈页选择“版权投诉”。需要登录，处理进度会在通知里告诉你。",
		MailSubject:  "版权投诉",
		MailHint:     "不需要注册账号，适合权利人或代理机构。",
	}},
}

func (a *App) docPage(slug string) gin.HandlerFunc {
	return func(c *gin.Context) {
		a.render.Page(c, http.StatusOK, "doc.html", gin.H{"Title": docs[slug].Title, "Doc": docs[slug], "Bare": auth.Current(c) == nil, "Nav": "me",
			"ContactEmail": a.Config.ContactEmail, "SEO": a.pageSEO("/"+slug, strings.ReplaceAll(docs[slug].Description, "{site}", a.Config.SiteName))})
	}
}

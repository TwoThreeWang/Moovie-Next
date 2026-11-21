package app

import (
	"cmp"
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
	"moovie/internal/httpx"
	"moovie/internal/ingest"
	"moovie/internal/media"
)

func (a *App) publicRoutes(r gin.IRouter) {
	r.GET("/", a.index)
	r.GET("/install", func(c *gin.Context) {
		seo := a.pageSEO("/install", "把 "+a.Config.SiteName+"添加到 iPhone、安卓手机或电脑的桌面：不用下载安装包，点图标就能像 App 一样全屏打开。按设备分步骤说明，安卓和电脑支持一键安装。")
		seo.Title, seo.Keywords = "添加到桌面 - iPhone、安卓和电脑安装教程", []string{"添加到桌面", "添加到主屏幕", "PWA 安装", "网页 App"}
		a.render.Page(c, http.StatusOK, "install.html", gin.H{"Title": "添加到桌面", "Nav": "me", "Bare": auth.Current(c) == nil, "Scripts": []string{"pages/install.js"},
			"SEO": seo})
	})
	r.GET("/changelog", a.changelogPage)
	for slug := range docs {
		r.GET("/"+slug, a.docPage(slug))
	}

	// 作品资料、榜单、片单、短评、公开主页：不登录也能看，搜索引擎也能收录。播放和个人数据仍然需要登录。
	r.GET("/discover", a.discoverPage)
	r.GET("/discover/:type", a.discoverPage)
	r.GET("/api/htmx/discover", a.discoverMore)
	r.GET("/search", a.searchPage)
	r.GET("/api/htmx/search", a.searchResults)
	r.GET("/api/htmx/row/chart/:kind", a.rowChart)
	r.GET("/api/htmx/row/collections", a.rowCollections)
	r.GET("/m/:id", a.detailPage)
	r.GET("/api/htmx/m/:id/comments", a.mediaComments)
	r.GET("/api/htmx/m/:id/similar", a.mediaSimilar)
	r.GET("/cinema", a.cinemaPage)
	r.GET("/api/htmx/comments/:id/replies", a.replies)
	r.GET("/list", a.listIndex)
	r.GET("/list/:slug", a.listDetail)
	r.POST("/api/collections/:slug/view", httpx.RateLimit(a.collectionViews), a.collectionView)
	r.GET("/user/:id", a.userPage)
	r.GET("/user/:id/monthly/:month", a.reportPage)
	r.GET("/m/:id/similar", a.similarPage)
	r.GET("/trends", a.trendsPage)
	r.GET("/m3u8", a.m3u8Page)
	// 老站的作品地址（豆瓣 ID），见 legacyRedirect。老站的片单、用户主页、播放页没有迁移，不接。
	r.GET("/movie/:douban_id", a.legacyRedirect(""))
	r.GET("/watch/:douban_id", a.legacyRedirect(""))
	r.GET("/similar/:douban_id", a.legacyRedirect("/similar"))
	r.GET("/api/htmx/m/:id/ready", a.mediaReady)
	r.POST("/api/search-log", httpx.RateLimit(a.searchLog), a.recordSearch)
}

func (a *App) memberRoutes(r gin.IRouter) {
	// 浏览
	r.GET("/foryou", a.forYouPage)
	r.GET("/api/htmx/row/foryou", a.rowForYou)
	r.GET("/api/htmx/row/friends", a.rowFriends)

	// 作品和播放
	r.GET("/api/htmx/m/:id/mark-form", a.markForm)
	r.POST("/api/library/:id/wish", a.toggleWish)
	r.POST("/api/library/:id/watched", a.markWatched)
	r.DELETE("/api/library/:id", a.removeMark)
	r.POST("/settings/delete-account", a.deleteOwnAccount)
	r.GET("/settings/export.json", a.exportJSON)
	r.GET("/settings/export.csv", a.exportCSV)
	r.GET("/play/local", a.playLocal)
	r.GET("/play/:id", a.playPage)
	r.POST("/api/media/resolve", a.resolveAPI)

	// 社区
	r.POST("/api/comments/:id/like", a.toggleLike)
	r.POST("/api/comments/:id/replies", auth.RequireVoice(), a.addReply)
	r.DELETE("/api/replies/:id", a.deleteReply)
	r.POST("/api/comments/:id/remove", a.removeComment)
	r.POST("/api/users/:id/follow", a.toggleFollow)
	r.GET("/feedback", a.feedbackPage)
	r.POST("/api/feedback", a.feedbackSubmit)

	// 我的
	r.GET("/me", a.mePage)
	r.GET("/me/collections", a.myCollections)
	r.GET("/me/collections/favorites", a.favoriteCollections)
	r.GET("/me/collections/new", a.collectionEdit)
	r.GET("/me/collections/search", a.collectionSearch)
	r.GET("/me/collections/:id", a.collectionEdit)
	r.POST("/me/collections", a.collectionSave)
	r.POST("/me/collections/preview", a.collectionPreview)
	r.POST("/me/collections/:id/visibility", a.collectionVisibility)
	r.POST("/me/collections/:id/favorite", a.collectionFavorite)
	r.GET("/me/:kind", a.meList)
	r.GET("/notifications", a.notificationsPage)
	r.GET("/notifications/:id/open", a.openNotification)
	r.POST("/notifications/read-all", a.readAllNotifications)
	r.POST("/notifications/clear", a.clearNotifications)
	r.POST("/api/notifications/:id/read", a.readNotification)
	r.DELETE("/api/notifications/:id", a.deleteNotification)
	r.GET("/settings", a.settingsPage)
	r.POST("/settings/douban/bind", a.doubanBind)
	r.POST("/settings/douban/unbind", a.doubanUnbind)
	r.POST("/settings/douban/sync", a.doubanSync)
	r.GET("/api/htmx/douban-status", a.doubanStatus)
}

// index 是首页：未登录显示产品介绍，登录后显示内容。
func (a *App) index(c *gin.Context) {
	user := auth.Current(c)
	if user == nil {
		a.render.Page(c, http.StatusOK, "landing.html", gin.H{"Title": a.Config.SiteName, "Bare": true, "SEO": a.siteSEO()})
		return
	}
	ctx := c.Request.Context()
	recent, err := a.Positions.Recent(ctx, user.ID, 12, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	airing, err := a.Media.AiringOn(ctx, user.ID, time.Now(), 12)
	if err != nil {
		a.fail(c, err)
		return
	}
	today := make([]CardView, 0, len(airing))
	for _, item := range airing {
		today = append(today, CardView{Card: item.Card, Href: mediaHref(item.Card.ID), Badge: "新", Sub: item.Label()})
	}
	hero, err := a.heroCards(ctx)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.render.Page(c, http.StatusOK, "home.html", gin.H{
		"Title": a.Config.SiteName, "Nav": "home", "Hero": hero, "SEO": a.siteSEO(),
		"Recent": wideViews(recent), "Today": today, "Scripts": []string{"pages/home.js"},
	})
}

// heroCards 从电影榜和剧集榜里挑几部有剧照的作品放在首页顶部。
func (a *App) heroCards(ctx context.Context) ([]media.Card, error) {
	movies, err := a.Media.Chart(ctx, media.TypeMovie, 12)
	if err != nil {
		return nil, err
	}
	shows, err := a.Media.Chart(ctx, media.TypeTV, 12)
	if err != nil {
		return nil, err
	}
	hero := []media.Card{}
	for index := 0; index < max(len(movies), len(shows)) && len(hero) < 5; index++ {
		for _, list := range [][]media.Card{shows, movies} {
			if index < len(list) && list[index].Backdrop != "" && list[index].Summary != "" && len(hero) < 5 {
				hero = append(hero, list[index])
			}
		}
	}
	return hero, nil
}

func (a *App) row(c *gin.Context, row RowView) {
	if len(row.Cards) == 0 && len(row.Wide) == 0 {
		c.Status(http.StatusOK) // 空内容：HTMX 会把占位行换成空
		return
	}
	a.render.Partial(c, "row", gin.H{"Row": row})
}

func (a *App) rowForYou(c *gin.Context) {
	cards, err := a.Recommend.ForUser(c.Request.Context(), auth.Current(c).ID, 20)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.row(c, RowView{Title: "为你推荐", Sub: "根据你的想看和看过", More: "/foryou", Cards: cardViews(cards)})
}

func (a *App) rowChart(c *gin.Context) {
	kind := c.Param("kind")
	if !media.ValidType(kind) {
		c.Status(http.StatusNotFound)
		return
	}
	cards, err := a.Media.Chart(c.Request.Context(), kind, 24)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.row(c, RowView{Title: "热门" + media.TypeLabel(kind), More: "/discover/" + kind, Cards: cardViews(cards)})
}

func (a *App) rowCollections(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	list, err := a.Collections.Browse(c.Request.Context(), "new", 8, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	if len(list) == 0 {
		c.Status(http.StatusOK)
		return
	}
	a.render.Partial(c, "row_collections", gin.H{"Collections": list})
}

func (a *App) rowFriends(c *gin.Context) {
	comments, err := a.Social.Feed(c.Request.Context(), auth.Current(c).ID, true, 16, 0)
	if err != nil {
		a.fail(c, err)
		return
	}
	seen := map[int64]bool{}
	cards := []CardView{}
	for _, comment := range comments {
		if seen[comment.MediaID] {
			continue
		}
		seen[comment.MediaID] = true
		cards = append(cards, CardView{
			Card: media.Card{ID: comment.MediaID, Title: comment.MediaTitle, Poster: comment.MediaPoster, Year: comment.MediaYear},
			Href: mediaHref(comment.MediaID), Sub: comment.Username + " 看过",
		})
	}
	a.row(c, RowView{Title: "朋友在看", More: "/cinema?tab=following", Cards: cards})
}

// ---------- 发现 ----------

// Genres 是发现页的类型筛选项。
var Genres = []string{"剧情", "喜剧", "爱情", "悬疑", "犯罪", "科幻", "动作", "动画", "奇幻", "恐怖", "家庭", "纪录片", "历史", "战争", "音乐"}

const discoverPageSize = 30

// shortTopics 缓存短剧频道的题材筛选项：从最热的几千部短剧里统计，一小时算一次就够了。
type shortTopics struct {
	mu     sync.Mutex
	list   []string
	expiry time.Time
}

func (a *App) shortTopicList(ctx context.Context) []string {
	a.topics.mu.Lock()
	defer a.topics.mu.Unlock()
	if time.Now().Before(a.topics.expiry) {
		return a.topics.list
	}
	list, err := a.Media.ShortTopics(ctx, 20)
	if err != nil {
		a.Log.Warn("统计短剧题材失败", "error", err)
		return a.topics.list
	}
	a.topics.list, a.topics.expiry = list, time.Now().Add(time.Hour)
	return list
}

func discoverFilter(c *gin.Context, mediaType string, offset int) media.Filter {
	sort := c.Query("sort")
	if sort != "rating" && sort != "new" {
		sort = "hot"
	}
	filter := media.Filter{
		Type: mediaType, Genre: c.Query("genre"), Sort: sort,
		Decade: httpx.QueryInt(c, "decade", 0), Limit: discoverPageSize + 1, Offset: offset,
	}
	if mediaType == media.TypeShort && media.IsShortForm(c.Query("form")) {
		filter.Form = c.Query("form")
	}
	return filter
}

func discoverPageURL(filter media.Filter, page int, fragment bool) string {
	query := url.Values{}
	if filter.Genre != "" {
		query.Set("genre", filter.Genre)
	}
	if filter.Form != "" {
		query.Set("form", filter.Form)
	}
	if filter.Sort != "hot" {
		query.Set("sort", filter.Sort)
	}
	if filter.Decade > 0 {
		query.Set("decade", strconv.Itoa(filter.Decade))
	}
	if fragment {
		query.Set("type", filter.Type)
		return pagedURL("/api/htmx/discover", query, page)
	}
	path := "/discover"
	if filter.Type != "" {
		path += "/" + filter.Type
	}
	return pagedURL(path, query, page)
}

func discoverCardsData(cards []media.Card, filter media.Filter, page int) gin.H {
	data := gin.H{"Cards": cardViews(firstN(cards, discoverPageSize))}
	if len(cards) > discoverPageSize {
		data["Next"] = discoverPageURL(filter, page+1, false)
		data["More"] = discoverPageURL(filter, page+1, true)
	}
	return data
}

// discoverSEO 是发现页的 SEO。标题按人们的搜索方式写（"热门电影推荐"、"高分悬疑电影推荐"）。
// 只有默认排序、没有题材和年代筛选的分类页允许收录；短剧不收录。
func (a *App) discoverSEO(filter media.Filter, page int) SEO {
	label := media.TypeLabel(filter.Type)
	subject := label
	if subject == "" {
		subject = "影视"
	}
	if filter.Genre != "" {
		subject = filter.Genre + label
	}
	if filter.Form != "" {
		subject = filter.Form
	}
	if filter.Decade > 0 {
		subject = strconv.Itoa(filter.Decade) + " 年代" + subject
	}
	order := map[string]string{"rating": "高分", "new": "最新"}[filter.Sort]
	if order == "" {
		order = "热门"
	}
	seo := a.pageSEO(discoverPageURL(filter, page, false), "浏览 "+a.Config.SiteName+"收录的"+order+subject+
		"：按类型、题材、年代和评分筛选，查看每部作品的简介、演员表、评分和短评，找到下一部想看的"+cmp.Or(label, "好片")+"。")
	seo.Title = order + subject + "推荐 - 按类型、年代和评分筛选"
	seo.Keywords = []string{order + subject, subject + "推荐", "高分" + subject, subject + "排行榜", label, filter.Genre}
	if filter.Type == "" && filter.Genre == "" && filter.Decade == 0 {
		seo.Title = "发现好片 - " + order + "电影、剧集、综艺和动漫推荐"
		seo.Description = "在 " + a.Config.SiteName + "发现" + order + "的电影、剧集、综艺和动漫：按类型、题材、年代和评分筛选，查看简介、演员表、评分和短评，找到下一部想看的好片。"
		seo.Keywords = []string{"发现好片", "电影推荐", "剧集推荐", "综艺推荐", "动漫推荐", order + "影视"}
	}
	seo.NoIndex = filter.Type == media.TypeShort || filter.Genre != "" || filter.Decade > 0 || filter.Sort != "hot"
	if page > 1 {
		label := "第 " + strconv.Itoa(page) + " 页"
		seo.Title += " · " + label
		seo.Description = label + "：" + seo.Description
	}
	return seo
}

func (a *App) discoverPage(c *gin.Context) {
	mediaType := c.Param("type")
	page, offset, valid := httpx.ParsePage(c.Query("page"), discoverPageSize)
	if !valid || (mediaType != "" && !media.ValidType(mediaType)) {
		a.render.NotFound(c)
		return
	}
	filter := discoverFilter(c, mediaType, offset)
	cards, err := a.Media.Discover(c.Request.Context(), filter)
	if err != nil {
		a.fail(c, err)
		return
	}
	if page > 1 && len(cards) == 0 {
		a.render.NotFound(c)
		return
	}
	title := "发现"
	if mediaType != "" {
		title += media.TypeLabel(mediaType)
	}
	if filter.Genre != "" {
		title = filter.Genre + media.TypeLabel(mediaType)
	}
	if filter.Form != "" {
		title = filter.Form + " · " + title
	}
	seo := a.discoverSEO(filter, page)
	data := discoverCardsData(cards, filter, page)
	data["Title"], data["Nav"], data["SEO"] = title, "discover", seo
	data["Types"], data["Genres"], data["Filter"] = media.Types, Genres, filter
	if page > 1 {
		data["Prev"] = discoverPageURL(filter, page-1, false)
	}
	if mediaType == media.TypeShort {
		// 短剧频道：题材用短剧自己的标签，另有剧种一栏；排序按红果的热度、评分、上架时间。
		data["Genres"], data["Forms"], data["Short"] = a.shortTopicList(c.Request.Context()), media.ShortForms, true
		if filter.Genre == "" && filter.Form == "" && filter.Offset == 0 && filter.Sort == "hot" {
			rows := []RowView{}
			for _, chart := range []struct{ kind, title, sub string }{
				{ingest.ChartShortHot, "热播榜", "红果热度"}, {ingest.ChartShortNew, "新剧榜", "近三天上架"},
			} {
				list, err := a.Media.Chart(c.Request.Context(), chart.kind, 24)
				if err != nil {
					a.fail(c, err)
					return
				}
				if len(list) > 0 {
					rows = append(rows, RowView{Title: chart.title, Sub: chart.sub, Cards: cardViews(list)})
				}
			}
			data["Rows"] = rows
		}
	}
	a.render.Page(c, http.StatusOK, "discover.html", data)
}

func (a *App) discoverMore(c *gin.Context) {
	mediaType := c.Query("type")
	page, offset, valid := httpx.ParsePage(c.Query("page"), discoverPageSize)
	if !valid || (mediaType != "" && !media.ValidType(mediaType)) {
		c.Status(http.StatusNotFound)
		return
	}
	filter := discoverFilter(c, mediaType, offset)
	cards, err := a.Media.Discover(c.Request.Context(), filter)
	if err != nil {
		a.fail(c, err)
		return
	}
	// 作品可能在浏览期间被移除。空片段让滚动加载正常结束。
	a.render.Partial(c, "grid_cards", discoverCardsData(cards, filter, page))
}

// ---------- 搜索 ----------

func (a *App) searchPage(c *gin.Context) {
	// "大家都在搜"：有热搜就用热搜，没有（刚上线、搜的人少）就用热门剧集榜。
	trending, err := a.Trends.Top(c.Request.Context(), 7, 2, 10)
	if err != nil {
		a.fail(c, err)
		return
	}
	var hot []media.Card
	if len(trending) < 5 {
		if hot, err = a.Media.Chart(c.Request.Context(), media.TypeTV, 10); err != nil {
			a.fail(c, err)
			return
		}
		trending = nil
	}
	query := strings.TrimSpace(c.Query("q"))
	seo := a.pageSEO("/search", "在 "+a.Config.SiteName+"搜索电影、剧集、综艺、动漫和短剧：按片名、演员或导演查找作品，查看简介、演员表和短评，还能看到大家正在搜的热门关键词。")
	seo.Title, seo.Keywords = "搜索电影、剧集、综艺和动漫", []string{"影视搜索", "电影搜索", "电视剧搜索", "找电影", "演员搜索"}
	if query != "" { // 搜索结果页不收录
		seo.Title, seo.NoIndex = "“"+clip(query, 30)+"”的搜索结果", true
		seo.Description = "在 " + a.Config.SiteName + "搜索“" + clip(query, 30) + "”相关的电影、剧集、综艺和动漫，查看简介、演员表和短评。"
		seo.Keywords = []string{query}
	}
	a.render.Page(c, http.StatusOK, "search.html", gin.H{
		"Title": "搜索", "Nav": "search", "Query": query, "Hot": hot, "Trending": trending,
		"Scripts": []string{"pages/search.js"}, "SEO": seo,
	})
}

// searchResults 在作品库里搜索。结果太少时去豆瓣查一次联想，把新条目收进作品库再搜一遍。
func (a *App) searchResults(c *gin.Context) {
	query := strings.TrimSpace(c.Query("q"))
	if query == "" || len([]rune(query)) > 60 {
		c.Status(http.StatusOK)
		return
	}
	ctx := c.Request.Context()
	cards, err := a.Media.Search(ctx, query, 36)
	if err != nil {
		a.fail(c, err)
		return
	}
	if len(cards) < 3 && len([]rune(query)) >= 2 && a.discover.Allow(visitorKey(c)) {
		probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		if err := a.Ingest.Discover(probeCtx, query); err == nil {
			if again, err := a.Media.Search(ctx, query, 36); err == nil {
				cards = again
			}
		}
		cancel()
	}
	a.render.Partial(c, "search_results", gin.H{"Cards": cardViews(cards), "Query": query})
}

func (a *App) forYouPage(c *gin.Context) {
	cards, err := a.Recommend.ForUser(c.Request.Context(), auth.Current(c).ID, 60)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.render.Page(c, http.StatusOK, "grid.html", gin.H{
		"Title": "为你推荐", "Nav": "home", "Heading": "为你推荐", "Lead": "根据你的想看和看过生成，每 6 小时更新一次。",
		"Cards": cardViews(cards), "Empty": "标记几部想看或看过的作品，这里就会出现推荐。",
	})
}

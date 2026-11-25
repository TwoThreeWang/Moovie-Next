package app

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/httpx"
	"moovie/internal/ingest"
	"moovie/internal/media"
	"moovie/internal/providers"
)

// 公开页面（不登录也能看）的搜索引擎优化、相似推荐、热搜榜。

// SEO 是页面给搜索引擎和分享卡片用的信息。没有设置 SEO 的页面（登录后才有的页面）一律 noindex。
// 站点名后缀、默认描述和长度限制在 prepareHead（head.go）里统一处理，这里只放页面自己的内容。
type SEO struct {
	Title       string // 搜索结果里显示的标题，不带站点名；为空时用页面标题
	Description string
	Keywords    []string // 页面自己的关键词；站点名和通用关键词会自动补上
	Canonical   string
	Image       string
	ImageAlt    string
	Type        string // og:type：website、video.movie、video.tv_show、profile
	NoIndex     bool   // 内容太少或属于个人的页面：不收录，但链接照常跟踪
	JSONLD      any
}

// absolute 把站内地址转成完整地址。
func (a *App) absolute(path string) string {
	if path == "" || strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return a.Config.SiteURL + path
}

func clip(text string, limit int) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return string(runes)
}

// mediaSEO 生成作品详情页的标题、描述和结构化数据。
//
// 标题和描述按人们实际搜索的方式写（片名 + 年份 + 类型；导演、主演、评分、简介），
// 结构化数据区分电影、单季和整剧，并附上面包屑。
func (a *App) mediaSEO(m media.Media) SEO {
	kind, ogType, section, sectionPath := "Movie", "video.movie", "电影", "/discover/movie"
	seasonPage := m.IsSeries() && !m.IsShort()
	if m.IsSeries() {
		kind, ogType = "TVSeries", "video.tv_show"
		section, sectionPath = m.TypeLabel(), "/discover/"+m.Type
	}
	if seasonPage {
		kind = "TVSeason"
	}
	url := a.absolute(mediaHref(m.ID))
	image := a.absolute(a.images.URL(m.Poster, "display", m.Title, "poster"))

	title := m.Title
	if m.Year > 0 {
		title += "（" + strconv.Itoa(m.Year) + "）"
	}
	title += " - " + m.TypeLabel() + "简介、演员表、短评"

	parts := []string{}
	if len(m.Directors) > 0 {
		parts = append(parts, "导演："+strings.Join(firstN(m.Directors, 2), "、"))
	}
	if len(m.Actors) > 0 {
		parts = append(parts, "主演："+strings.Join(firstN(m.Actors, 4), "、"))
	}
	if len(m.Genres) > 0 {
		parts = append(parts, "类型："+strings.Join(firstN(m.Genres, 3), "、"))
	}
	if m.RatingDouban > 0 {
		parts = append(parts, "豆瓣 "+strconv.FormatFloat(m.RatingDouban, 'f', 1, 64)+" 分")
	}
	description := strings.Join(parts, "；")
	if description != "" {
		description += "。"
	}
	if m.Summary != "" {
		description += m.Summary
	} else {
		description += "《" + m.Title + "》的简介、分集、短评和相似推荐。"
	}

	work := map[string]any{"@type": kind, "@id": url + "#work", "name": m.Title, "url": url, "inLanguage": "zh-CN",
		"dateModified": m.UpdatedAt.Format(time.RFC3339)}
	if m.OriginalTitle != "" && m.OriginalTitle != m.Title {
		work["alternateName"] = m.OriginalTitle
	}
	if m.Year > 0 {
		work["datePublished"] = strconv.Itoa(m.Year)
	}
	if image != "" {
		work["image"] = image
	}
	if m.Summary != "" {
		work["description"] = clip(m.Summary, 300)
	}
	if len(m.Genres) > 0 {
		work["genre"] = m.Genres
	}
	if len(m.Countries) > 0 {
		work["countryOfOrigin"] = m.Countries
	}
	people := func(names []string, limit int) []map[string]string {
		list := []map[string]string{}
		for _, name := range firstN(names, limit) {
			list = append(list, map[string]string{"@type": "Person", "name": name})
		}
		return list
	}
	if len(m.Directors) > 0 {
		work["director"] = people(m.Directors, 3)
	}
	if len(m.Actors) > 0 {
		work["actor"] = people(m.Actors, 8)
	}
	if m.IsSeries() && m.EpisodeCount > 0 {
		work["numberOfEpisodes"] = m.EpisodeCount
	}
	if !m.IsSeries() && m.DurationMinutes > 0 {
		work["duration"] = "PT" + strconv.Itoa(m.DurationMinutes) + "M"
	}
	sameAs := []string{}
	if m.DoubanID != "" {
		sameAs = append(sameAs, "https://movie.douban.com/subject/"+m.DoubanID+"/")
	}
	if seasonPage {
		if m.Season > 0 {
			work["seasonNumber"] = m.Season
		}
		work["partOfSeries"] = mediaSeriesSchema(m)
	} else {
		if m.IMDbID != "" {
			sameAs = append(sameAs, "https://www.imdb.com/title/"+m.IMDbID+"/")
		}
		if m.WikidataID != "" {
			sameAs = append(sameAs, "https://www.wikidata.org/wiki/"+m.WikidataID)
		}
	}
	if len(sameAs) > 0 {
		work["sameAs"] = sameAs
	}
	crumbs := breadcrumbs([][2]string{{"首页", a.absolute("/")}, {section, a.absolute(sectionPath)}, {m.Title, url}})
	return SEO{
		Title: title, Description: clip(description, descriptionLimit), Keywords: mediaKeywords(m),
		Canonical: url, Image: image, ImageAlt: "《" + m.Title + "》海报", Type: ogType,
		// 资料还没补全的作品内容太少，先不让收录
		// 短剧先不让搜索引擎收录：简介是从红果原样来的，收录了也排不过官网，反而容易招来投诉。
		NoIndex: m.MetadataStatus != "complete" || m.Title == "" || m.IsShort(),
		JSONLD:  map[string]any{"@context": "https://schema.org", "@graph": []any{work, crumbs}},
	}
}

// mediaKeywords 是作品详情页的关键词：片名、原名、"片名 年份"、类型、题材、导演和主演。
func mediaKeywords(m media.Media) []string {
	words := []string{m.Title}
	if m.OriginalTitle != "" && m.OriginalTitle != m.Title {
		words = append(words, m.OriginalTitle)
	}
	if m.Year > 0 {
		words = append(words, m.Title+" "+strconv.Itoa(m.Year))
	}
	words = append(words, m.Title+m.TypeLabel(), m.TypeLabel())
	words = append(words, firstN(m.Genres, 2)...)
	words = append(words, firstN(m.Directors, 1)...)
	return append(words, firstN(m.Actors, 2)...)
}

func mediaSeriesSchema(m media.Media) map[string]any {
	name, _ := media.SplitSeason(m.Title)
	series := map[string]any{"@type": "TVSeries", "name": name}
	links := []string{}
	if m.IMDbID != "" {
		links = append(links, "https://www.imdb.com/title/"+m.IMDbID+"/")
	}
	if id, ok := strings.CutPrefix(m.TMDBID, "tv/"); ok {
		if number, err := strconv.ParseUint(id, 10, 64); err == nil && number > 0 {
			links = append(links, "https://www.themoviedb.org/tv/"+strconv.FormatUint(number, 10))
		}
	}
	if len(links) > 0 {
		series["@id"], series["sameAs"] = links[0], links
	}
	return series
}

// breadcrumbs 生成面包屑的结构化数据。
func breadcrumbs(items [][2]string) map[string]any {
	list := make([]map[string]any, 0, len(items))
	for index, item := range items {
		list = append(list, map[string]any{"@type": "ListItem", "position": index + 1, "name": item[0], "item": item[1]})
	}
	return map[string]any{"@type": "BreadcrumbList", "itemListElement": list}
}

// siteSEO 是首页的 SEO：标题和描述用站点的（见 head.go），结构化数据告诉搜索引擎站点叫什么。
func (a *App) siteSEO() SEO {
	seo := a.pageSEO("/", a.siteDescription())
	seo.Keywords = []string{"电影推荐", "剧集推荐", "影视资料", "影评", "片单", "追剧", "观影记录"}
	seo.JSONLD = map[string]any{"@context": "https://schema.org", "@type": "WebSite", "name": a.Config.SiteName, "url": a.absolute("/"),
		"description": a.siteDescription(), "inLanguage": "zh-CN"}
	return seo
}

func (a *App) pageSEO(path, description string) SEO {
	return SEO{Description: description, Canonical: a.absolute(path), Type: "website"}
}

// ---------- 相似推荐 ----------

func (a *App) similarPage(c *gin.Context) {
	m, ok := a.loadMedia(c)
	if !ok {
		return
	}
	cards, err := a.Media.Similar(c.Request.Context(), m, 48)
	if err != nil {
		a.fail(c, err)
		return
	}
	seo := a.pageSEO(mediaHref(m.ID)+"/similar", fmt.Sprintf("喜欢《%s》的人可能也喜欢的作品：根据类型、导演、演员和剧情推荐的相似电影和剧集。", m.Title))
	seo.Image = a.absolute(a.images.URL(m.Poster, "display", m.Title, "poster"))
	seo.NoIndex = true // 内容是详情页的延伸，不单独收录；里面的链接照常跟踪
	a.render.Page(c, http.StatusOK, "grid.html", gin.H{
		"Title": "和《" + m.Title + "》相似的作品", "Nav": "discover", "Heading": "和《" + m.Title + "》相似的作品",
		"Lead": "根据类型、导演、演员和剧情推荐。", "Cards": cardViews(cards), "Empty": "暂时还没有找到相似的作品。", "SEO": seo,
		"BackLink": mediaHref(m.ID), "BackText": "返回《" + m.Title + "》",
	})
}

// ---------- 热搜榜 ----------

// recentSet 记住最近出现过的键，用来去重（同一个人短时间内反复搜同一个词只算一次）。
type recentSet struct {
	mu   sync.Mutex
	ttl  time.Duration
	seen map[string]time.Time
}

func newRecentSet(ttl time.Duration) *recentSet {
	return &recentSet{ttl: ttl, seen: map[string]time.Time{}}
}

// Add 记下一个键。最近已经出现过时返回 false。
func (r *recentSet) Add(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if at, ok := r.seen[key]; ok && now.Sub(at) < r.ttl {
		return false
	}
	if len(r.seen) > 50000 {
		for existing, at := range r.seen {
			if now.Sub(at) >= r.ttl {
				delete(r.seen, existing)
			}
		}
	}
	r.seen[key] = now
	return true
}

// recordSearch 记一次作品库搜索，用来生成热搜榜。浏览器在用户停下来、并且有结果时才发。
func (a *App) recordSearch(c *gin.Context) {
	var body struct {
		Q string `json:"q"`
	}
	c.ShouldBindJSON(&body)
	keyword := strings.TrimSpace(body.Q)
	if keyword == "" || len([]rune(keyword)) > 40 {
		c.Status(http.StatusNoContent)
		return
	}
	if a.searched.Add(visitorKey(c) + "|" + media.Normalize(keyword)) {
		if err := a.Trends.Record(c.Request.Context(), keyword); err != nil {
			c.Error(err)
		}
	}
	c.Status(http.StatusNoContent)
}

func (a *App) trendsPage(c *gin.Context) {
	days, label := 1, "今天"
	if c.Query("range") == "week" {
		days, label = 7, "本周"
	}
	keywords, err := a.Trends.Top(c.Request.Context(), days, 2, 50)
	if err != nil {
		a.fail(c, err)
		return
	}
	seo := a.pageSEO("/trends", a.Config.SiteName+"影视热搜榜：大家"+label+"都在搜哪些电影、剧集、综艺和动漫，按搜索人数实时排行，也可以切换查看"+map[bool]string{true: "今天", false: "本周"}[days > 1]+"的热搜。")
	seo.Title, seo.Keywords = "影视热搜榜 - 大家都在搜的电影和剧集", []string{"影视热搜榜", "电影热搜", "电视剧热搜", "热门电影", "热门剧集"}
	if days > 1 {
		seo.Canonical = a.absolute("/trends?range=week")
		seo.Title = "本周影视热搜榜 - 大家都在搜的电影和剧集"
	}
	a.render.Page(c, http.StatusOK, "trends.html", gin.H{
		"Title": "热搜榜", "Nav": "trends", "Keywords": keywords, "Week": days > 1, "Label": label, "SEO": seo,
	})
}

// ---------- 作品资料获取中 ----------

// mediaReady 给"资料获取中"的提示轮询：资料补全后让页面刷新；等太久就不再等。
func (a *App) mediaReady(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	m, err := a.Media.Get(c.Request.Context(), id)
	if errors.Is(err, media.ErrNotFound) {
		c.Status(http.StatusNotFound)
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	attempt := httpx.QueryInt(c, "n", 0)
	if m.MetadataSyncedAt != nil {
		c.Header("HX-Refresh", "true")
		c.Status(http.StatusOK)
		return
	}
	a.render.Partial(c, "fetching", gin.H{"ID": m.ID, "Attempt": attempt + 1, "GiveUp": attempt >= 20, "Douban": m.DoubanID, "Type": m.Type})
}

// ---------- robots.txt 和站点地图 ----------

func (a *App) robots(c *gin.Context) {
	c.String(http.StatusOK, strings.Join([]string{
		"User-agent: *",
		"Allow: /",
		"Disallow: /api/", "Disallow: /play/", "Disallow: /me", "Disallow: /settings", "Disallow: /notifications",
		"Disallow: /admin", "Disallow: /auth/", "Disallow: /feedback", "Disallow: /offline",
		"Disallow: /search?", "Disallow: /foryou",
		"", "Sitemap: " + a.Config.SiteURL + "/sitemap.xml", "",
	}, "\n"))
}

// ---------- 老站地址 ----------

// legacyRedirect 接住老站（moovie.c2v2.com）用豆瓣 ID 的地址，跳到新站的作品页。老站 nginx 把没登录的访问
// 原路径 301 过来，搜索引擎的收录和外链就转到新站，不需要新旧 ID 对照表。suffix 是作品页之后的路径。
//
// 已有资料的作品永久跳转；库里没有的先收录、排队抓资料，再临时跳转到"正在获取资料"的作品页，
// 豆瓣确认没有这个条目时，老地址就返回 404，不会被永久指向一个空页面。
func (a *App) legacyRedirect(suffix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, doubanID := c.Request.Context(), c.Param("douban_id")
		if !media.ValidDoubanID(doubanID) {
			a.render.NotFound(c)
			return
		}
		m, err := a.Media.ByDoubanID(ctx, doubanID)
		switch {
		case errors.Is(err, media.ErrNotFound):
			id, err := a.Ingest.EnsureDouban(ctx, providers.DoubanItem{ID: doubanID}, "", ingest.PriorityDiscover)
			if err != nil {
				a.fail(c, err)
				return
			}
			c.Redirect(http.StatusFound, mediaHref(id)+suffix)
		case err != nil:
			a.fail(c, err)
		case m.Hidden || m.MetadataStatus == "not_found":
			a.render.NotFound(c)
		case m.MetadataStatus == "complete":
			c.Redirect(http.StatusMovedPermanently, mediaHref(m.ID)+suffix)
		default:
			c.Redirect(http.StatusFound, mediaHref(m.ID)+suffix)
		}
	}
}

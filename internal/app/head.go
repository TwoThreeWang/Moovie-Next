package app

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// 页面 <head> 里的标题、描述、关键词、robots、canonical 和分享标签都在这里统一生成，模板只负责输出。
// 各页面只给出自己的 SEO（标题不带站点名、描述、关键词），站点名后缀、默认描述、长度限制都在这一处。

const (
	// siteTagline 是首页标题里站点名后面的那句话。
	siteTagline = "发现你的下一部电影"
	// descriptionLimit 是描述的最大字数。中文搜索结果大约显示 80～120 个字，再长会被截断。
	descriptionLimit = 120
	// keywordLimit 是关键词的最大个数。
	keywordLimit = 10
	// shareImage 是页面没有自己的图片时，分享卡片用的图（站点图标）。
	shareImage = "/static/img/icon-512.png"
)

// siteKeywords 是站点的通用关键词，页面没有自己的关键词时用它，有的时候也补在后面。
var siteKeywords = []string{"电影", "剧集", "综艺", "动漫", "影评", "片单", "观影记录"}

// HeadMeta 是 <head> 里要输出的内容，由 prepareHead 生成，放在模板数据的 Meta 里。
type HeadMeta struct {
	Title       string // <title>：页面标题 - 站点名
	ShareTitle  string // og:title / twitter:title：不带站点名（站点名在 og:site_name 里）
	Description string
	Keywords    string
	Robots      string // 空表示允许收录
	Canonical   string
	Public      bool // 是否输出 Open Graph、Twitter Card 和结构化数据（只给公开页面）
	Type        string
	Image       string
	ImageAlt    string
	LargeImage  bool // 页面有自己的图（海报、封面）：Twitter 用大图卡片
	JSONLD      any
}

// siteDescription 是首页和没有自己描述的页面用的描述。
func (a *App) siteDescription() string {
	return a.Config.SiteName + "：" + siteTagline + "。记录想看和看过，追剧集更新，看热门榜单、影迷片单和相似推荐，每个月一份观影报告。"
}

// homeTitle 是首页的完整标题。
func (a *App) homeTitle() string { return a.Config.SiteName + " - " + siteTagline }

// prepareHead 在整页渲染前生成 Meta。
//
// 标题：页面的 SEO.Title，没有时用页面的 Title，后面加" - 站点名"；页面标题为空或就是站点名时（首页）用"站点名 - 发现你的下一部电影"。
// 没有设置 SEO 的页面是登录后才有的页面（或错误页），一律 noindex，只输出标题、默认描述和关键词。
func (a *App) prepareHead(c *gin.Context, data gin.H) {
	site := a.Config.SiteName
	pageTitle, _ := data["Title"].(string)
	seo, public := data["SEO"].(SEO)
	if public && seo.Title != "" {
		pageTitle = seo.Title
	}
	meta := HeadMeta{Title: pageTitle + " - " + site, ShareTitle: pageTitle}
	if pageTitle == "" || pageTitle == site {
		meta.Title, meta.ShareTitle = a.homeTitle(), a.homeTitle()
	}
	meta.Description = clip(seo.Description, descriptionLimit)
	if meta.Description == "" {
		meta.Description = a.siteDescription()
	}
	meta.Keywords = a.keywords(seo.Keywords)
	if !public {
		meta.Robots = "noindex"
		data["Meta"] = meta
		return
	}
	if seo.NoIndex {
		meta.Robots = "noindex,follow"
	}
	meta.Public, meta.Canonical, meta.JSONLD = true, seo.Canonical, seo.JSONLD
	meta.Type = seo.Type
	if meta.Type == "" {
		meta.Type = "website"
	}
	meta.Image, meta.ImageAlt, meta.LargeImage = seo.Image, seo.ImageAlt, seo.Image != ""
	if meta.Image == "" {
		meta.Image, meta.ImageAlt = a.absolute(shareImage), site
	}
	data["Meta"] = meta
}

// keywords 把页面的关键词和站点名、通用关键词合在一起，去重后最多 keywordLimit 个。
func (a *App) keywords(page []string) string {
	list := make([]string, 0, keywordLimit)
	seen := map[string]bool{}
	add := func(limit int, words ...string) {
		for _, word := range words {
			word = strings.TrimSpace(strings.NewReplacer(",", " ", "，", " ").Replace(word))
			if word == "" || seen[word] || len(list) >= limit {
				continue
			}
			seen[word] = true
			list = append(list, word)
		}
	}
	add(keywordLimit-1, page...) // 留一个位置给站点名
	if len(list) < 4 {
		add(keywordLimit-1, siteKeywords...)
	}
	add(keywordLimit, a.Config.SiteName)
	return strings.Join(list, ",")
}

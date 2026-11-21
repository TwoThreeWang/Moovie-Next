package app

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// playerFAQ 是 M3U8 播放器页的常见问题。页面上显示的和结构化数据里的是同一份，
// 搜索引擎要求两者一致。
var playerFAQ = []struct{ Q, A string }{
	{"支持哪些格式？", "支持 m3u8（HLS）和 MP4 链接。多清晰度的 m3u8 会按网速自动切换清晰度。"},
	{"我的链接会被上传吗？", "不会。链接放在网址的 # 后面，只在你的浏览器里使用；视频由浏览器直接从对方的服务器加载，本站服务器既收不到链接，也不经手视频。"},
	{"为什么有的链接播放不了？", "常见原因有四个：链接已经过期；对方服务器不允许其他网站播放（跨域限制）；链接是 HTTP 地址，浏览器不允许在 HTTPS 页面里播放；链接需要登录或特定的请求头。"},
	{"能记住播放进度吗？", "能。进度保存在这台设备的浏览器里，下次打开同一个链接会从上次的位置继续。最近播放的 10 个链接也只存在本机，可以随时清空。"},
	{"能跳过视频里的广告吗？", "能。播放器会分析播放清单，识别插在正片里的广告片段；大家确认过的广告会自动跳过。"},
	{"手机上能用吗？", "iPhone、安卓手机和电脑的浏览器都能用。支持倍速、画中画、全屏，手机上左右滑动调进度、双击快退快进；电脑上可以用空格暂停、方向键快退快进。"},
}

// m3u8Page 是在线 M3U8 播放器：不用登录，粘贴链接就能播。
// 链接只在浏览器里用（放在 # 后面），服务器只输出页面。
func (a *App) m3u8Page(c *gin.Context) {
	seo := a.pageSEO("/m3u8", "免费在线 M3U8 播放器：粘贴 m3u8、HLS 或 MP4 链接直接在浏览器里播放，不用下载软件、不用注册。支持倍速、画中画、记住进度、自动跳过广告，手机电脑都能用。")
	seo.Title = "在线 M3U8 播放器 - 免费播放 m3u8 / HLS / MP4 链接" // prepareHead 会在后面加上站点名
	seo.Keywords = []string{"M3U8 播放器", "在线 M3U8 播放器", "m3u8 在线播放", "HLS 播放器", "MP4 在线播放", "网页播放器"}
	questions := make([]map[string]any, 0, len(playerFAQ))
	for _, item := range playerFAQ {
		questions = append(questions, map[string]any{"@type": "Question", "name": item.Q, "acceptedAnswer": map[string]any{"@type": "Answer", "text": item.A}})
	}
	seo.JSONLD = map[string]any{"@context": "https://schema.org", "@graph": []any{
		map[string]any{
			"@type": "WebApplication", "name": "在线 M3U8 播放器", "url": a.absolute("/m3u8"), "inLanguage": "zh-CN",
			"applicationCategory": "MultimediaApplication", "operatingSystem": "Any", "browserRequirements": "需要支持 JavaScript 的现代浏览器",
			"description": seo.Description, "offers": map[string]any{"@type": "Offer", "price": "0", "priceCurrency": "CNY"},
		},
		map[string]any{"@type": "FAQPage", "mainEntity": questions},
		breadcrumbs([][2]string{{a.Config.SiteName, a.absolute("/")}, {"M3U8 播放器", a.absolute("/m3u8")}}),
	}}
	a.render.Page(c, http.StatusOK, "play.html", gin.H{
		"Title": "在线 M3U8 播放器", "Nav": "", "CSS": []string{"player"}, "BodyClass": "player-page",
		"Scripts": []string{"pages/play.js"}, "Manual": true, "FAQ": playerFAQ, "SEO": seo,
	})
}

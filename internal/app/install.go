package app

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"moovie/web"
)

// 和"添加到桌面"有关的路由：应用清单、Service Worker、离线页。

func (a *App) installRoutes(router *gin.Engine) {
	router.GET("/manifest.webmanifest", a.manifest)
	router.GET("/sw.js", a.serviceWorker)
	router.GET("/offline", func(c *gin.Context) {
		a.render.Page(c, http.StatusOK, "offline.html", gin.H{"Title": "没有网络", "Bare": true})
	})
}

func (a *App) manifest(c *gin.Context) {
	icon := func(src, sizes, purpose string) gin.H {
		return gin.H{"src": src, "sizes": sizes, "type": "image/png", "purpose": purpose}
	}
	body, _ := json.Marshal(gin.H{
		"id":               "/",
		"name":             a.Config.SiteName,
		"short_name":       a.Config.SiteName,
		"description":      "记录想看和看过，追更新，发现下一部。",
		"lang":             "zh-CN",
		"start_url":        "/?from=app",
		"scope":            "/",
		"display":          "standalone",
		"orientation":      "any",
		"background_color": "#0a0a0c",
		"theme_color":      "#0a0a0c",
		"icons": []gin.H{
			icon("/static/img/icon-192.png", "192x192", "any"),
			icon("/static/img/icon-512.png", "512x512", "any"),
			icon("/static/img/icon-maskable-512.png", "512x512", "maskable"),
		},
		// 安卓上长按桌面图标出现的快捷入口
		"shortcuts": []gin.H{
			{"name": "搜索", "url": "/search", "icons": []gin.H{icon("/static/img/icon-192.png", "192x192", "any")}},
			{"name": "观看历史", "url": "/me/history", "icons": []gin.H{icon("/static/img/icon-192.png", "192x192", "any")}},
		},
	})
	c.Header("Cache-Control", "no-cache")
	c.Data(http.StatusOK, "application/manifest+json; charset=utf-8", body)
}

// serviceWorker 返回 Service Worker 脚本，并把静态文件的版本号填进去，新版本上线后旧缓存会被清掉。
// 必须放在网站根路径下，才能管到所有页面。
func (a *App) serviceWorker(c *gin.Context) {
	var body []byte
	var err error
	if a.Config.Dev() {
		body, err = os.ReadFile("web/static/sw.js")
	}
	if body == nil || err != nil {
		body, err = fs.ReadFile(web.Files, "static/sw.js")
	}
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "no-cache")
	c.Header("Service-Worker-Allowed", "/")
	c.Data(http.StatusOK, "text/javascript; charset=utf-8", []byte(strings.ReplaceAll(string(body), "__VERSION__", a.assetV)))
}

// backTarget 返回二级页面的返回地址。底部导航能直接到达的页面、自带返回按钮的详情页和播放页返回空字符串。
// 添加到桌面后没有浏览器的返回按钮，二级页面要自己提供一个。
func backTarget(path string) string {
	switch {
	case path == "/" || path == "/discover" || path == "/search" || path == "/me" || strings.HasPrefix(path, "/discover/"):
		return ""
	case strings.HasPrefix(path, "/m/") || strings.HasPrefix(path, "/play/") || strings.HasPrefix(path, "/auth/") || path == "/offline" || path == "/manifest.webmanifest":
		return ""
	case strings.HasPrefix(path, "/list") || path == "/cinema" || path == "/foryou":
		return "/discover"
	case strings.HasPrefix(path, "/admin/"):
		return "/admin"
	}
	return "/me"
}

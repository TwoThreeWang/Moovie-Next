package app

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (a *App) errorPage(c *gin.Context, status int, heading, message string) {
	c.Header("Cache-Control", "no-store")
	c.Set("errorPage", true)
	primary, primaryText := "/", "回到首页"
	secondary, secondaryText := "/discover", "发现作品"
	switch status {
	case http.StatusNotFound:
		if heading == "" {
			heading = "页面找不到了"
		}
		primary, primaryText = "/search", "搜索作品"
		if c.Request.URL.Path == "/list" || strings.HasPrefix(c.Request.URL.Path, "/list/") {
			primary, primaryText = "/list", "浏览片单"
			secondary, secondaryText = "/search", "搜索作品"
		}
	case http.StatusForbidden:
		if heading == "" {
			heading = "无法访问"
		}
	case http.StatusTooManyRequests:
		heading = "操作太频繁"
	case http.StatusInternalServerError, http.StatusServiceUnavailable:
		heading = "服务暂时不可用"
		secondary, secondaryText = "/feedback", "提交反馈"
		path := c.Request.URL.EscapedPath()
		if c.Request.Method == http.MethodGet && strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") {
			primary, primaryText = c.Request.URL.RequestURI(), "重试"
		}
	default:
		if heading == "" {
			heading = "请求未能完成"
		}
	}
	a.render.Page(c, status, "error.html", gin.H{
		"Title": heading, "Heading": heading, "Message": message, "Status": status,
		"Primary": primary, "PrimaryText": primaryText, "Secondary": secondary, "SecondaryText": secondaryText,
	})
}

package app

import (
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"
)

// pagedURL 给路径加上查询参数和页码；第 1 页不带 page 参数，和 canonical 保持一致。
func pagedURL(path string, query url.Values, page int) string {
	query.Del("page")
	if page > 1 {
		query.Set("page", strconv.Itoa(page))
	}
	return (&url.URL{Path: path, RawQuery: query.Encode()}).String()
}

// pageLink 对一个已有的地址（可以带查询参数和锚点）生成各页的链接。
func pageLink(raw string) func(int) string {
	base, _ := url.Parse(raw)
	return func(page int) string {
		link := *base
		link.RawQuery = ""
		next, _ := url.Parse(pagedURL(base.Path, base.Query(), page))
		link.RawQuery = next.RawQuery
		return link.String()
	}
}

// setPager 写入上一页、下一页链接；more 表示多取的那一条存在，即还有下一页。
func setPager(data gin.H, page int, more bool, link func(int) string) {
	if page > 1 {
		data["Prev"] = link(page - 1)
	}
	if more {
		data["Next"] = link(page + 1)
	}
}

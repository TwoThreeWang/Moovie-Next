// Package ui 负责渲染页面模板和 HTMX 片段。
package ui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Renderer 持有解析好的模板。
type Renderer struct {
	fsys   fs.FS
	reload bool
	common func(c *gin.Context) gin.H

	mu       sync.RWMutex
	pages    map[string]*template.Template
	partials *template.Template
}

// New 创建渲染器。reload 为 true 时每次请求都重新读模板（开发用）。
// common 返回每个页面都需要的数据（当前用户、CSRF 令牌等）。
func New(fsys fs.FS, reload bool, common func(c *gin.Context) gin.H) (*Renderer, error) {
	renderer := &Renderer{fsys: fsys, reload: reload, common: common}
	if err := renderer.load(); err != nil {
		return nil, err
	}
	return renderer, nil
}

func (r *Renderer) load() error {
	partialFiles, err := fs.Glob(r.fsys, "templates/partials/*.html")
	if err != nil {
		return err
	}
	base := template.New("base").Funcs(Funcs)
	if len(partialFiles) > 0 {
		if base, err = base.ParseFS(r.fsys, partialFiles...); err != nil {
			return fmt.Errorf("解析片段模板: %w", err)
		}
	}
	withLayout, err := template.Must(base.Clone()).ParseFS(r.fsys, "templates/layout.html")
	if err != nil {
		return fmt.Errorf("解析布局模板: %w", err)
	}
	pageFiles, err := fs.Glob(r.fsys, "templates/pages/*.html")
	if err != nil {
		return err
	}
	pages := make(map[string]*template.Template, len(pageFiles))
	for _, file := range pageFiles {
		page, err := template.Must(withLayout.Clone()).ParseFS(r.fsys, file)
		if err != nil {
			return fmt.Errorf("解析页面模板 %s: %w", file, err)
		}
		pages[path.Base(file)] = page
	}
	r.mu.Lock()
	r.pages, r.partials = pages, base
	r.mu.Unlock()
	return nil
}

func (r *Renderer) data(c *gin.Context, data gin.H) gin.H {
	merged := gin.H{}
	if r.common != nil {
		for key, value := range r.common(c) {
			merged[key] = value
		}
	}
	for key, value := range data {
		merged[key] = value
	}
	return merged
}

// buffers 是渲染页面用的缓冲。重复使用可以少产生很多待回收的内存：一个页面二十多 KB，
// 每次都新建的话，缓冲从小到大翻倍扩容，实际要分配好几倍的量。
var buffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func (r *Renderer) execute(c *gin.Context, status int, tmpl *template.Template, name string, data gin.H) {
	buffer := buffers.Get().(*bytes.Buffer)
	buffer.Reset()
	defer func() {
		if buffer.Cap() <= 1<<20 {
			buffers.Put(buffer)
		}
	}()
	if err := tmpl.ExecuteTemplate(buffer, name, r.data(c, data)); err != nil {
		c.Error(err)
		c.String(http.StatusInternalServerError, "页面渲染失败")
		return
	}
	c.Data(status, "text/html; charset=utf-8", buffer.Bytes())
}

// Page 渲染一个完整页面（带布局）。
func (r *Renderer) Page(c *gin.Context, status int, name string, data gin.H) {
	if r.reload {
		if err := r.load(); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
	}
	r.mu.RLock()
	page, ok := r.pages[name]
	r.mu.RUnlock()
	if !ok {
		c.String(http.StatusInternalServerError, "模板不存在: "+name)
		return
	}
	r.execute(c, status, page, "layout", data)
}

// Partial 渲染一个片段（给 HTMX 用）。
func (r *Renderer) Partial(c *gin.Context, name string, data gin.H) {
	if r.reload {
		if err := r.load(); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
	}
	r.mu.RLock()
	partials := r.partials
	r.mu.RUnlock()
	r.execute(c, http.StatusOK, partials, name, data)
}

// NotFound 渲染 404 页面。
func (r *Renderer) NotFound(c *gin.Context) {
	r.Page(c, http.StatusNotFound, "message.html", gin.H{"Title": "页面不存在", "Heading": "页面不存在", "Message": "你访问的页面不存在，或者已经被移除。"})
}

// ---------- 模板函数 ----------

var (
	doubanSize  = regexp.MustCompile(`/(s|m|l)_ratio_poster/|/(s|m|l)/public/`)
	tmdbSize    = regexp.MustCompile(`/t/p/[a-z0-9]+/`)
	hongguoSize = regexp.MustCompile(`tplv-shrink:[0-9]+:[0-9]+`)
)

// Image 把图片地址转成适合展示的地址。size 取 s / m / l。
//
// 豆瓣图片有防盗链，必须经过站内图片代理；TMDB 图片可以直接引用，只需要换成合适的尺寸。
func Image(raw, size string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case strings.HasSuffix(host, ".doubanio.com"):
		sized := doubanSize.ReplaceAllStringFunc(raw, func(match string) string {
			if strings.Contains(match, "ratio_poster") {
				return "/" + size + "_ratio_poster/"
			}
			return "/" + size + "/public/"
		})
		return "/img/" + base64.RawURLEncoding.EncodeToString([]byte(sized))
	case strings.HasSuffix(host, "-novel.byteimg.com"):
		// 红果的封面按宽度缩放：地址里的 tplv-shrink:640:0 换成要的宽度。也走站内代理，统一备份。
		width := map[string]string{"s": "320", "m": "480", "l": "640"}[size]
		if width == "" {
			width = "480"
		}
		return "/img/" + base64.RawURLEncoding.EncodeToString([]byte(hongguoSize.ReplaceAllString(raw, "tplv-shrink:"+width+":0")))
	case host == "image.tmdb.org":
		// TMDB 的图片也走站内代理：统一备份到 Telegram，也不让浏览器直接连第三方。
		width := map[string]string{"s": "w342", "m": "w780", "l": "w1280"}[size]
		if width == "" {
			width = "w780"
		}
		return "/img/" + base64.RawURLEncoding.EncodeToString([]byte(tmdbSize.ReplaceAllString(raw, "/t/p/"+width+"/")))
	}
	return raw
}

// TimeAgo 把时间显示成"3 小时前"这样的相对时间。接受 time.Time 或 *time.Time。
func TimeAgo(value any) string {
	var t time.Time
	switch typed := value.(type) {
	case time.Time:
		t = typed
	case *time.Time:
		if typed != nil {
			t = *typed
		}
	}
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	}
	return t.Format("2006-01-02")
}

// Funcs 是所有模板可用的函数。
var Funcs = template.FuncMap{
	"img":     Image,
	"timeago": TimeAgo,
	"join":    strings.Join,
	"ratings": func() []int { return []int{5, 4, 3, 2, 1} },
	"date": func(value any) string {
		switch typed := value.(type) {
		case time.Time:
			return typed.Format("2006-01-02")
		case *time.Time:
			if typed != nil {
				return typed.Format("2006-01-02")
			}
		}
		return ""
	},
	"floatOf": func(value int) float64 { return float64(value) },
	"add":     func(a, b int) int { return a + b },
	"sub":     func(a, b int) int { return a - b },
	"seq": func(from, to int) []int {
		values := make([]int, 0, max(0, to-from+1))
		for i := from; i <= to; i++ {
			values = append(values, i)
		}
		return values
	},
	"dict": func(pairs ...any) map[string]any {
		result := make(map[string]any, len(pairs)/2)
		for i := 0; i+1 < len(pairs); i += 2 {
			result[fmt.Sprint(pairs[i])] = pairs[i+1]
		}
		return result
	},
	// json 只用在 data-* 属性里：html/template 会按属性上下文转义它。
	"json": func(value any) string {
		encoded, _ := json.Marshal(value)
		return string(encoded)
	},
	"rating": func(value float64) string {
		if value <= 0 {
			return ""
		}
		return fmt.Sprintf("%.1f", value)
	},
	"stars": func(rating int) string {
		if rating <= 0 {
			return ""
		}
		return strings.Repeat("★", rating) + strings.Repeat("☆", max(0, 5-rating))
	},
	"first": func(values []string, count int) []string {
		if len(values) > count {
			return values[:count]
		}
		return values
	},
	"pct": func(position, duration float64) int {
		if duration <= 0 {
			return 0
		}
		return int(min(100, max(0, position/duration*100)))
	},
	"minutes": func(seconds float64) int { return int(seconds / 60) },
	"truncate": func(value string, limit int) string {
		runes := []rune(value)
		if len(runes) <= limit {
			return value
		}
		return string(runes[:limit]) + "…"
	},
}

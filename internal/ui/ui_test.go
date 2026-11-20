package ui_test

import (
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/ui"
	"moovie/web"
)

// 所有模板都必须能解析：模板写错时在这里就能发现，不用等到页面被访问。
func TestTemplatesParse(t *testing.T) {
	if _, err := ui.New(web.Files, false, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRendererImageFunctionIsScoped(t *testing.T) {
	files := fstest.MapFS{
		"templates/layout.html":     {Data: []byte(`{{define "layout"}}{{template "content" .}}{{end}}`)},
		"templates/pages/test.html": {Data: []byte(`{{define "content"}}{{img "https://image.tmdb.org/t/p/original/a.jpg" "thumb" "电影" "poster"}}{{end}}`)},
	}
	newRenderer := func(value string) *ui.Renderer {
		r, err := ui.New(files, true, nil, func(raw, size, title, purpose string) string { return value })
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	one, two := newRenderer("/one"), newRenderer("/two")
	for _, item := range []struct {
		r    *ui.Renderer
		want string
	}{{one, "/one"}, {two, "/two"}, {one, "/one"}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		item.r.Page(c, 200, "test.html", nil)
		if w.Body.String() != item.want {
			t.Fatalf("应用之间不能互相覆盖图片签名函数: %q", w.Body.String())
		}
	}
}

func TestTimeAgo(t *testing.T) {
	now := time.Now()
	past := now.Add(-3 * time.Hour)
	if got := ui.TimeAgo(past); got != "3 小时前" {
		t.Errorf("TimeAgo = %q", got)
	}
	if got := ui.TimeAgo(&past); got != "3 小时前" {
		t.Errorf("TimeAgo(指针) = %q", got)
	}
	var missing *time.Time
	if got := ui.TimeAgo(missing); got != "" {
		t.Errorf("空指针应该返回空字符串，实际 %q", got)
	}
}

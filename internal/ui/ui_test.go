package ui_test

import (
	"encoding/base64"
	"testing"
	"time"

	"moovie/internal/ui"
	"moovie/web"
)

// 所有模板都必须能解析：模板写错时在这里就能发现，不用等到页面被访问。
func TestTemplatesParse(t *testing.T) {
	if _, err := ui.New(web.Files, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestImage(t *testing.T) {
	cases := []struct{ raw, size, want string }{
		{"", "s", ""},
		{"http://img1.doubanio.com/a.jpg", "s", ""}, // 只接受 HTTPS
		// TMDB 的图片也走站内代理，并换成对应尺寸
		{"https://image.tmdb.org/t/p/original/abc.jpg", "s", "/img/" + base64.RawURLEncoding.EncodeToString([]byte("https://image.tmdb.org/t/p/w342/abc.jpg"))},
		{"https://image.tmdb.org/t/p/original/abc.jpg", "l", "/img/" + base64.RawURLEncoding.EncodeToString([]byte("https://image.tmdb.org/t/p/w1280/abc.jpg"))},
		{"https://example.com/x.jpg", "m", "https://example.com/x.jpg"},
	}
	for _, c := range cases {
		if got := ui.Image(c.raw, c.size); got != c.want {
			t.Errorf("Image(%q, %q) = %q，期望 %q", c.raw, c.size, got, c.want)
		}
	}
	// 豆瓣图片走站内代理，并换成对应尺寸。
	got := ui.Image("https://img9.doubanio.com/view/photo/l_ratio_poster/public/p1.jpg", "s")
	if got == "" || got[:5] != "/img/" {
		t.Errorf("豆瓣图片应该走 /img/ 代理，实际 %q", got)
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

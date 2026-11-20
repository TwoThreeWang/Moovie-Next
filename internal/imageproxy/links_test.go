package imageproxy

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestStableSignedLinks(t *testing.T) {
	h := New(nil, "secret")
	raw := "https://image.tmdb.org/t/p/original/a.jpg"
	link := h.URL(raw, "thumb", "电影：名字/第二部", "poster")
	if link == "" || link != New(nil, "secret").URL(raw, "thumb", "电影：名字/第二部", "poster") {
		t.Fatal("同一素材的地址必须跨实例、跨请求保持稳定")
	}
	u, _ := url.Parse(link)
	encoded := strings.TrimPrefix(u.Path, "/img/")
	ref, size, err := h.verify(encoded, u.Query())
	if err != nil || size != "thumb" || ref.URL != raw || ref.Title != "电影：名字_第二部" {
		t.Fatalf("签名无法还原服务端元数据: %+v %q %v", ref, size, err)
	}
	if got := ref.filename(size, "image/png", raw); !strings.HasPrefix(got, "电影：名字_第二部_海报_缩略图_") || !strings.HasSuffix(got, ".png") {
		t.Fatalf("文件名缺少片名、用途、规格或扩展名: %q", got)
	}
	for name, mutate := range map[string]func(url.Values){
		"改原图规格": func(q url.Values) { q.Set("size", "original") },
		"删除签名":  func(q url.Values) { q.Del("sig") },
		"重复规格":  func(q url.Values) { q.Add("size", "thumb") },
		"重复签名":  func(q url.Values) { q.Add("sig", q.Get("sig")) },
		"任意宽度":  func(q url.Values) { q.Set("width", "10000") },
	} {
		t.Run(name, func(t *testing.T) {
			q := u.Query()
			mutate(q)
			if _, _, err := h.verify(encoded, q); err == nil {
				t.Fatal("参数篡改必须拒绝")
			}
		})
	}
	ref.Title = "伪造片名"
	data, _ := json.Marshal(ref)
	if _, _, err := h.verify(base64.RawURLEncoding.EncodeToString(data), u.Query()); err == nil {
		t.Fatal("改文件名元数据必须拒绝")
	}
	if _, _, err := New(nil, "other-secret").verify(encoded, u.Query()); err == nil {
		t.Fatal("不同应用的密钥不能互相签发")
	}
	if New(nil, "").URL(raw, "thumb", "电影", "poster") != "" {
		t.Fatal("缺少密钥不能生成可用链接")
	}
	if h.URL(raw, "arbitrary", "电影", "poster") != "" || h.URL(raw, "thumb", "电影", "unknown") != "" {
		t.Fatal("不能签发未知规格或用途")
	}
}

func TestImageSources(t *testing.T) {
	tmdb := "https://image.tmdb.org/t/p/original/a.jpg"
	cases := []struct {
		raw, size, purpose, want string
		resize                   int
	}{
		{tmdb, "thumb", "poster", "https://image.tmdb.org/t/p/w342/a.jpg", 0},
		{tmdb, "display", "poster", "https://image.tmdb.org/t/p/w780/a.jpg", 0},
		{tmdb, "thumb", "still", "https://image.tmdb.org/t/p/w780/a.jpg", 0},
		{tmdb, "display", "still", "https://image.tmdb.org/t/p/w1280/a.jpg", 0},
		{tmdb, "original", "poster", tmdb, 0},
		{"https://img1.doubanio.com/view/photo/l_ratio_poster/public/a.jpg", "thumb", "poster", "https://img1.doubanio.com/view/photo/m_ratio_poster/public/a.jpg", 0},
		{"https://img1.doubanio.com/view/photo/s/public/a.jpg", "original", "still", "https://img1.doubanio.com/view/photo/l/public/a.jpg", 0},
		{"https://p6-novel.byteimg.com/a~tplv-shrink:640:0.image", "thumb", "poster", "https://p6-novel.byteimg.com/a~tplv-shrink:320:0.image", 0},
		{"https://p6-novel.byteimg.com/a~tplv-shrink:640:0.image", "original", "poster", "https://p6-novel.byteimg.com/a~tplv-shrink:640:0.image", 0},
		{"https://img1.doubanio.com/a.jpg", "thumb", "poster", "https://img1.doubanio.com/a.jpg", 480},
		{"https://img1.doubanio.com/a.jpg", "display", "poster", "https://img1.doubanio.com/a.jpg", 1280},
	}
	for _, c := range cases {
		target, resize := (imageRef{URL: c.raw, Purpose: c.purpose}).source(c.size)
		if target != c.want || resize != c.resize {
			t.Errorf("%s/%s: %q, %d; want %q, %d", c.purpose, c.size, target, resize, c.want, c.resize)
		}
	}
}

func TestLongFilenameMetadataIsStable(t *testing.T) {
	for _, title := range []string{strings.Repeat("甲", 79) + " 尾部", strings.Repeat("电影", 100), strings.Repeat("🎬", 100), "../\"\r\n"} {
		clean := cleanTitle(title)
		if cleanTitle(clean) != clean || len(clean) > 192 || strings.ContainsAny(clean, "/\\\r\n\"") {
			t.Fatalf("文件名清理必须安全且幂等: %q", clean)
		}
		h := New(nil, "secret")
		u, _ := url.Parse(h.URL("https://image.tmdb.org/t/p/original/a.jpg", "thumb", title, "poster"))
		if _, _, err := h.verify(strings.TrimPrefix(u.Path, "/img/"), u.Query()); err != nil {
			t.Fatal("长片名不能导致自己签发的链接无法使用", err)
		}
	}
}

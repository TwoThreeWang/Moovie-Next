package imageproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func imageRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.SetTrustedProxies(nil)
	h.Register(r)
	return r
}

func fetchImage(r http.Handler, link string, headers map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, link, nil)
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	r.ServeHTTP(w, request)
	return w
}

func TestProxySignaturesCachingAndSEO(t *testing.T) {
	data := []byte("upstream jpeg bytes")
	var hits atomic.Int32
	h := New(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		if r.URL.Path != "/t/p/w342/a.jpg" {
			t.Errorf("缩略图回源路径错误: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}, "secret")
	r := imageRouter(h)
	link := h.URL("https://image.tmdb.org/t/p/original/a.jpg", "thumb", "电影", "poster")
	for _, headers := range []map[string]string{nil, {"User-Agent": "Googlebot-Image/1.0"}, {"Sec-Fetch-Site": "cross-site", "Referer": "https://other.example/"}} {
		w := fetchImage(r, link, headers)
		if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "public") || !bytes.Equal(w.Body.Bytes(), data) {
			t.Fatalf("公开图应该稳定可抓取: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Set-Cookie") != "" || w.Header().Get("Vary") != "" {
			t.Fatal("不能引入访客身份或来源相关缓存变体")
		}
	}
	w := fetchImage(r, link, nil)
	cached := fetchImage(r, link, map[string]string{"If-None-Match": "W/" + w.Header().Get("ETag")})
	if cached.Code != 304 || cached.Body.Len() != 0 {
		t.Fatal("相同图片应支持条件请求")
	}
	head := httptest.NewRecorder()
	r.ServeHTTP(head, httptest.NewRequest(http.MethodHead, link, nil))
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != w.Header().Get("Content-Length") || head.Header().Get("ETag") != w.Header().Get("ETag") {
		t.Fatal("HEAD 应返回与 GET 一致的元数据，不发送图片内容")
	}
	before := hits.Load()
	for _, invalid := range []string{
		"/img/" + base64.RawURLEncoding.EncodeToString([]byte("https://image.tmdb.org/t/p/original/a.jpg")),
		strings.Replace(link, "size=thumb", "size=original", 1),
		link + "&size=thumb", link + "&extra=1",
	} {
		w := fetchImage(r, invalid, nil)
		if w.Code != 403 || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("拒绝响应不能缓存: %d %s", w.Code, w.Header().Get("Cache-Control"))
		}
	}
	if hits.Load() != before {
		t.Fatal("无效签名不能触发回源")
	}
	if w := fetchImage(r, link, nil); w.Code != 200 {
		t.Fatal("拒绝恶意请求不能影响正常链接")
	}
}

func TestImageFailuresAreNotCacheable(t *testing.T) {
	for name, response := range map[string]func() *http.Response{
		"upstream forbidden": func() *http.Response {
			return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("no"))}
		},
		"not an image": func() *http.Response {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader("html"))}
		},
		"too large": func() *http.Response {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(io.LimitReader(zeroReader{}, maxImageBytes+1))}
		},
		"interrupted body": func() *http.Response {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(&brokenReader{})}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := New(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return response(), nil })}, "secret")
			w := fetchImage(imageRouter(h), h.URL("https://image.tmdb.org/t/p/original/a.jpg", "original", "电影", "poster"), nil)
			if w.Code != 502 || w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() != 0 {
				t.Fatalf("不能把失败或不完整内容作为可缓存的成功图: %d %s", w.Code, w.Header().Get("Cache-Control"))
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type brokenReader struct{ sent bool }

func (r *brokenReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, "partial"), nil
}

func TestOriginalRateLimitDoesNotBlockThumbnails(t *testing.T) {
	h := New(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(strings.NewReader("image"))}, nil
	})}, "secret")
	r := imageRouter(h)
	raw := "https://image.tmdb.org/t/p/original/a.jpg"
	for range 60 {
		if w := fetchImage(r, h.URL(raw, "original", "电影", "poster"), nil); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	w := fetchImage(r, h.URL(raw, "original", "电影", "poster"), nil)
	if w.Code != 429 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Retry-After") == "" {
		t.Fatal("原图限流不能被缓存")
	}
	if w := fetchImage(r, h.URL(raw, "thumb", "电影", "poster"), nil); w.Code != 200 {
		t.Fatal("原图限流不能阻塞缩略图")
	}
}

func TestFallbackResizeAndOriginal(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1200; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 180, G: 70, B: 40, A: 255})
		}
	}
	var original bytes.Buffer
	if err := jpeg.Encode(&original, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	h := New(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(bytes.NewReader(original.Bytes()))}, nil
	})}, "secret")
	r := imageRouter(h)
	raw := "https://img1.doubanio.com/no-size-interface.jpg"
	w := fetchImage(r, h.URL(raw, "thumb", "电影", "poster"), nil)
	config, _, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	if err != nil || w.Code != 200 || config.Width != 480 || config.Height != 320 {
		t.Fatalf("缩图必须保持比例: %d %+v %v", w.Code, config, err)
	}
	w = fetchImage(r, h.URL(raw, "original", "电影", "poster"), nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), original.Bytes()) {
		t.Fatal("原图必须保留源文件内容")
	}
	transparent := image.NewNRGBA(image.Rect(0, 0, 800, 400))
	var pngData bytes.Buffer
	png.Encode(&pngData, transparent)
	resized, typ, err := resizeImage(pngData.Bytes(), "image/png", 480, noReserve)
	if err != nil || typ != "image/png" || len(resized) == 0 {
		t.Fatal("透明图必须保留透明度")
	}
}

func TestOriginRedirectCannotEscapeAllowlist(t *testing.T) {
	var leaked atomic.Int32
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "image.tmdb.org" {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"http://127.0.0.1/private"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		leaked.Add(1)
		return nil, io.EOF
	})}
	if _, err := openOrigin(context.Background(), client, "https://image.tmdb.org/a.jpg"); err == nil || leaked.Load() != 0 {
		t.Fatal("不能跟随重定向访问内网")
	}
}

func TestRejectMalformedQuery(t *testing.T) {
	h := New(nil, "secret")
	u, _ := url.Parse(h.URL("https://image.tmdb.org/t/p/original/a.jpg", "thumb", "电影", "poster"))
	w := fetchImage(imageRouter(h), u.String()+"&broken=%zz", nil)
	if w.Code != 400 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("错误查询不能缓存")
	}
}

func TestNativeThumbnailMissingFallsBackToSource(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	var original bytes.Buffer
	jpeg.Encode(&original, img, nil)
	h := New(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/w342/") {
			return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("gone"))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(bytes.NewReader(original.Bytes()))}, nil
	})}, "secret")
	w := fetchImage(imageRouter(h), h.URL("https://image.tmdb.org/t/p/original/a.jpg", "thumb", "电影", "poster"), nil)
	config, _, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	if err != nil || w.Code != 200 || config.Width != 480 || config.Height != 320 {
		t.Fatalf("缺少上游尺寸时应该本机缩图兜底: %d %+v %v", w.Code, config, err)
	}
}

func TestResizeRejectsHugeCanvasBeforeDecoding(t *testing.T) {
	var data bytes.Buffer
	png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	raw := data.Bytes()
	binary.BigEndian.PutUint32(raw[16:20], 20_000_000)
	binary.BigEndian.PutUint32(raw[29:33], crc32.ChecksumIEEE(raw[12:29]))
	if _, _, err := resizeImage(raw, "image/png", 480, noReserve); err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("必须先检查解码后的像素数: %v", err)
	}
}

func noReserve(int64) error { return nil }

func TestResizeKeepsSmallImageAsIs(t *testing.T) {
	var small bytes.Buffer
	jpeg.Encode(&small, image.NewRGBA(image.Rect(0, 0, 300, 200)), nil)
	reserved := false
	data, typ, err := resizeImage(small.Bytes(), "image/jpeg", 480, func(int64) error { reserved = true; return nil })
	if err != nil || typ != "image/jpeg" || !bytes.Equal(data, small.Bytes()) || reserved {
		t.Fatal("不大于目标尺寸的图片应原样返回，不解码也不重新编码")
	}
}

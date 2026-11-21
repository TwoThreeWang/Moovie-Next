package imageproxy

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/db/testdb"
	"moovie/internal/telegram"
)

// rewrite 把对豆瓣图片域名的请求转到测试服务器。
type rewrite struct{ target *url.URL }

func (r rewrite) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.URL.Scheme, request.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(request)
}

func TestTelegramMirror(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	image := []byte("\x89PNG fake poster bytes")

	var originHits, uploads atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "https://movie.douban.com/" {
			http.Error(w, "防盗链", http.StatusForbidden)
			return
		}
		originHits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Write(image)
	}))
	defer origin.Close()
	originURL, _ := url.Parse(origin.URL)

	var stored []byte
	var limited, downloadsFail atomic.Bool
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/botTOKEN/sendDocument":
			uploads.Add(1)
			if limited.CompareAndSwap(true, false) { // 限流一次
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, `{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":1}}`)
				return
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil || r.FormValue("chat_id") != "-1001234" {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			file, header, _ := r.FormFile("document")
			stored, _ = io.ReadAll(file)
			if header.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(header.Filename, "测试电影_海报_原图_") || !strings.HasSuffix(header.Filename, ".png") {
				http.Error(w, "bad file", http.StatusBadRequest)
				return
			}
			io.WriteString(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":-1001234},"document":{"file_id":"F1","file_unique_id":"U1","file_size":22}}}`)
		case r.URL.Path == "/botTOKEN/getFile":
			io.WriteString(w, `{"ok":true,"result":{"file_path":"documents/f1.png"}}`)
		case r.URL.Path == "/file/botTOKEN/documents/f1.png":
			if downloadsFail.Load() {
				http.Error(w, "gone", http.StatusNotFound)
				return
			}
			w.Write(stored)
		default:
			http.NotFound(w, r)
		}
	}))
	defer tg.Close()

	client := &http.Client{Transport: rewrite{originURL}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mirror := newMirror(pool, telegram.New("TOKEN", "-1001234", tg.URL, nil), logger) // 不启动后台协程，测试里手动 drain
	mirror.gap = 0
	handler := New(client, "test-secret")
	handler.Mirror = mirror
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler.Register(router)

	raw := "https://img9.doubanio.com/view/photo/l/public/p1.png"
	get := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, handler.URL(raw, "original", "测试电影", "poster"), nil))
		return recorder
	}

	// 第一次：从原图取，同时把取到的图片交给备份器（不再下载第二次）
	first := get()
	if first.Code != http.StatusOK || first.Header().Get("X-Image-From") != "origin" || first.Body.String() != string(image) {
		t.Fatalf("第一次应该从原图取: %d %s", first.Code, first.Header().Get("X-Image-From"))
	}
	get() // 还没传完时再看一次：同一张图不重复排
	if len(mirror.pending) != 1 {
		t.Fatalf("应该排了一张，实际 %d 张", len(mirror.pending))
	}

	// 上传：第一次被 Telegram 限流，按它说的时间等一下再传，不算失败
	limited.Store(true)
	start := time.Now()
	mirror.drain()
	if uploads.Load() != 2 || time.Since(start) < time.Second {
		t.Errorf("被限流后应该等 retry_after 再传一次: 上传 %d 次，等了 %s", uploads.Load(), time.Since(start))
	}
	record, found, err := mirror.Lookup(ctx, raw)
	if err != nil || !found || record.OriginalURL != raw || record.FileID != "F1" || record.ContentType != "image/png" || record.Bytes != len(image) || string(stored) != string(image) {
		t.Fatalf("备份记录不对: %+v %v", record, err)
	}
	if originHits.Load() != 2 {
		t.Errorf("原图只应该被浏览器的两次请求下载，备份不再单独下载，实际 %d 次", originHits.Load())
	}

	// 之后从 Telegram 取
	second := get()
	if second.Header().Get("X-Image-From") != "telegram" || second.Body.String() != string(image) || second.Header().Get("Content-Type") != "image/png" {
		t.Errorf("备份之后应该从 Telegram 取: %s %q", second.Header().Get("X-Image-From"), second.Body.String())
	}
	// Telegram 取不到时退回原图
	downloadsFail.Store(true)
	if third := get(); third.Header().Get("X-Image-From") != "origin" || third.Body.String() != string(image) {
		t.Errorf("Telegram 取不到时应该退回原图: %s", third.Header().Get("X-Image-From"))
	}
	// 已经备份过的：Telegram 临时取不到时退回原图，但不会再排一次备份
	if len(mirror.pending) != 0 {
		t.Errorf("已经备份过的图片不应该再排队: %d", len(mirror.pending))
	}
}

func TestThumbnailFromLegacyBackupAndBrokenBackupFallback(t *testing.T) {
	pool := testdb.Pool(t)
	var original bytes.Buffer
	jpeg.Encode(&original, image.NewRGBA(image.Rect(0, 0, 1200, 800)), nil)
	raw := "https://img1.doubanio.com/legacy-without-size.jpg"
	_, err := pool.Exec(context.Background(), `INSERT INTO image_mirrors (url_hash,original_url,tg_file_id,content_type,bytes) VALUES ($1,$2,'OLD','image/jpeg',$3)`, hashURL(raw), raw, original.Len())
	if err != nil {
		t.Fatal(err)
	}
	var broken atomic.Bool
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			io.WriteString(w, `{"ok":true,"result":{"file_path":"documents/old.jpg"}}`)
			return
		}
		if broken.Load() {
			w.Write(original.Bytes()[:original.Len()/2])
			return
		}
		w.Write(original.Bytes())
	}))
	defer tg.Close()
	var originHits atomic.Int32
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		originHits.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(bytes.NewReader(original.Bytes()))}, nil
	})}
	h := New(client, "secret")
	h.Mirror = newMirror(pool, telegram.New("TOKEN", "123", tg.URL, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.markFailed(raw) // 原站失败冷却不能阻止使用完整的旧 TG 备份。
	r := imageRouter(h)
	link := h.URL(raw, "thumb", "旧电影", "poster")
	w := fetchImage(r, link, nil)
	config, _, decodeErr := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	if w.Code != 200 || decodeErr != nil || config.Width != 480 || originHits.Load() != 0 {
		t.Fatalf("旧原图备份应该能生成缩略图: %d %+v %v", w.Code, config, decodeErr)
	}
	if len(h.Mirror.pending) != 1 || !strings.HasPrefix(h.Mirror.pending[0].name, "旧电影_海报_缩略图_") {
		t.Fatal("新规格必须使用片名和用途命名")
	}
	broken.Store(true)
	h.mu.Lock()
	delete(h.failed, raw)
	h.mu.Unlock()
	w = fetchImage(r, link, nil)
	if w.Code != 200 || originHits.Load() != 1 {
		t.Fatal("TG 返回不完整内容时必须回退原站，不能把坏数据当成成功图片")
	}
}

func TestLocalFallbackDoesNotTakeUpstreamSizeKey(t *testing.T) {
	pool := testdb.Pool(t)
	var small bytes.Buffer
	jpeg.Encode(&small, image.NewRGBA(image.Rect(0, 0, 300, 450)), nil)
	raw := "https://img1.doubanio.com/view/photo/s_ratio_poster/public/p1.jpg"
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/l_ratio_poster/") {
			return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("gone"))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(bytes.NewReader(small.Bytes()))}, nil
	})}
	h := New(client, "secret")
	h.Mirror = newMirror(pool, telegram.New("TOKEN", "123", "http://127.0.0.1:1", nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if w := fetchImage(imageRouter(h), h.URL(raw, "display", "电影", "poster"), nil); w.Code != 200 {
		t.Fatalf("展示图应该用来源版本兜底: %d", w.Code)
	}
	// 豆瓣的 display 和 original 都指向 l 图；兜底得到的小图不能记在 l 图的地址下，否则原图请求会拿到它。
	if len(h.Mirror.pending) != 1 || h.Mirror.pending[0].raw != raw+"#moovie-display-v1" {
		t.Fatalf("兜底缩图必须按本机缩图的 key 备份: %+v", h.Mirror.pending)
	}
}

// 卡片图：用豆瓣 m 图在 Telegram 的备份在本机缩小，不请求豆瓣；缩小后的结果按自己的键备份。
func TestCardSizeResizesFromExistingBackup(t *testing.T) {
	pool := testdb.Pool(t)
	var medium bytes.Buffer
	jpeg.Encode(&medium, image.NewRGBA(image.Rect(0, 0, 540, 800)), nil)
	raw := "https://img1.doubanio.com/view/photo/l_ratio_poster/public/p1.jpg"
	mURL := "https://img1.doubanio.com/view/photo/m_ratio_poster/public/p1.jpg"
	if _, err := pool.Exec(context.Background(), `INSERT INTO image_mirrors (url_hash,original_url,tg_file_id,content_type,bytes) VALUES ($1,$2,'M','image/jpeg',$3)`,
		hashURL(mURL), mURL, medium.Len()); err != nil {
		t.Fatal(err)
	}
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			io.WriteString(w, `{"ok":true,"result":{"file_path":"documents/m.jpg"}}`)
			return
		}
		w.Write(medium.Bytes())
	}))
	defer tg.Close()
	var originHits atomic.Int32
	h := New(&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		originHits.Add(1)
		return nil, io.ErrUnexpectedEOF
	})}, "secret")
	h.Mirror = newMirror(pool, telegram.New("TOKEN", "123", tg.URL, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := fetchImage(imageRouter(h), h.URL(raw, "card", "某电影", "poster"), nil)
	config, _, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	if w.Code != 200 || err != nil || config.Width != 243 || config.Height != 360 || originHits.Load() != 0 {
		t.Fatalf("卡片图应该由 m 图备份缩到 243×360、不请求豆瓣: %d %+v %v 回源 %d 次", w.Code, config, err, originHits.Load())
	}
	if len(h.Mirror.pending) != 1 || !strings.HasPrefix(h.Mirror.pending[0].name, "某电影_海报_卡片图_") || h.Mirror.pending[0].raw == mURL {
		t.Fatalf("卡片图要按自己的键备份，不能占用 m 图的键: %+v", h.Mirror.pending)
	}
}

// Telegram 备份读得太慢时改从来源取；来源也太慢时按时返回失败，不会一直占着执行位。
func TestSlowBackupAndSourceGiveUpInTime(t *testing.T) {
	pool := testdb.Pool(t)
	defer func(previous func(string) waits) { waitsFor = previous }(waitsFor)
	waitsFor = func(string) waits { return waits{backup: 200 * time.Millisecond, source: 400 * time.Millisecond} }

	var picture bytes.Buffer
	jpeg.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 300, 450)), nil)
	raw := "https://img1.doubanio.com/view/photo/m_ratio_poster/public/slow.jpg"
	pool.Exec(context.Background(), `INSERT INTO image_mirrors (url_hash,original_url,tg_file_id,content_type,bytes) VALUES ($1,$2,'SLOW','image/jpeg',$3)`,
		hashURL(raw), raw, picture.Len())
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			io.WriteString(w, `{"ok":true,"result":{"file_path":"documents/slow.jpg"}}`)
			return
		}
		select { // 下载卡住
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer tg.Close()
	var originSlow atomic.Bool
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if originSlow.Load() {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(bytes.NewReader(picture.Bytes()))}, nil
	})}
	h := New(client, "secret")
	h.Mirror = newMirror(pool, telegram.New("TOKEN", "123", tg.URL, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := imageRouter(h)

	started := time.Now()
	w := fetchImage(r, h.URL(raw, "thumb", "慢图", "poster"), nil)
	if w.Code != 200 || time.Since(started) > 2*time.Second {
		t.Fatalf("备份卡住时应该很快改从来源取: %d，用了 %v", w.Code, time.Since(started))
	}

	originSlow.Store(true)
	other := "https://img1.doubanio.com/view/photo/m_ratio_poster/public/other.jpg"
	started = time.Now()
	w = fetchImage(r, h.URL(other, "thumb", "慢图", "poster"), nil)
	if w.Code != http.StatusBadGateway || time.Since(started) > 2*time.Second || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("来源卡住时应该按时失败、且不被缓存: %d %q，用了 %v", w.Code, w.Header().Get("Cache-Control"), time.Since(started))
	}
}

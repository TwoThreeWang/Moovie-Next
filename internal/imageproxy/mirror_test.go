package imageproxy

import (
	"context"
	"encoding/base64"
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
			if header.Header.Get("Content-Type") != "image/png" || !strings.HasSuffix(header.Filename, ".png") {
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
	handler := New(client)
	handler.Mirror = mirror
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler.Register(router)

	raw := "https://img9.doubanio.com/view/photo/l/public/p1.png"
	get := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/img/"+base64.RawURLEncoding.EncodeToString([]byte(raw)), nil))
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

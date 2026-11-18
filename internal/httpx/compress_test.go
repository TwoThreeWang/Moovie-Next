package httpx

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

var testTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func compressRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Compress("/img/"))
	page := strings.Repeat("<p>影牛</p>", 500)
	router.GET("/page", func(c *gin.Context) { c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(page)) })
	router.GET("/missing", func(c *gin.Context) { c.Data(http.StatusNotFound, "text/html; charset=utf-8", []byte(page)) })
	router.GET("/small", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	router.GET("/photo", func(c *gin.Context) { c.Data(http.StatusOK, "image/png", []byte(page)) })
	router.GET("/img/x", func(c *gin.Context) { c.Data(http.StatusOK, "text/html", []byte(page)) })
	router.GET("/unchanged", func(c *gin.Context) { c.Status(http.StatusNotModified) })
	router.GET("/file", func(c *gin.Context) {
		http.ServeContent(c.Writer, c.Request, "a.css", testTime, strings.NewReader(page))
	})
	return router
}

func fetch(router *gin.Engine, path string, headers ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for index := 0; index+1 < len(headers); index += 2 {
		request.Header.Set(headers[index], headers[index+1])
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func unzip(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	reader, err := gzip.NewReader(recorder.Body)
	if err != nil {
		t.Fatalf("响应不是 gzip: %v", err)
	}
	body, _ := io.ReadAll(reader)
	return string(body)
}

func TestCompress(t *testing.T) {
	router := compressRouter()
	page := strings.Repeat("<p>影牛</p>", 500)

	response := fetch(router, "/page", "Accept-Encoding", "gzip, br")
	if response.Header().Get("Content-Encoding") != "gzip" || response.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("页面应该被压缩: %v", response.Header())
	}
	if response.Body.Len() >= len(page)/2 || unzip(t, response) != page {
		t.Error("压缩后的内容不对")
	}

	// 状态码原样保留。
	response = fetch(router, "/missing", "Accept-Encoding", "gzip")
	if response.Code != http.StatusNotFound || unzip(t, response) != page {
		t.Errorf("404 页面也应该压缩并保留状态码，得到 %d", response.Code)
	}

	// 静态文件：带着 Content-Length 来的，压缩后要去掉。
	response = fetch(router, "/file", "Accept-Encoding", "gzip")
	if response.Header().Get("Content-Length") != "" || unzip(t, response) != page {
		t.Errorf("静态文件压缩后不应该再带原来的长度: %v", response.Header())
	}

	for name, plain := range map[string]*httptest.ResponseRecorder{
		"浏览器不支持 gzip": fetch(router, "/page"),
		"内容太小":        fetch(router, "/small", "Accept-Encoding", "gzip"),
		"图片":          fetch(router, "/photo", "Accept-Encoding", "gzip"),
		"放行的路径":       fetch(router, "/img/x", "Accept-Encoding", "gzip"),
		"分段请求":        fetch(router, "/file", "Accept-Encoding", "gzip", "Range", "bytes=0-1999"),
	} {
		if plain.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s：不应该压缩", name)
		}
	}
	if body := fetch(router, "/page").Body.String(); body != page {
		t.Error("不压缩时内容应该原样返回")
	}
	if partial := fetch(router, "/file", "Range", "bytes=0-1999"); partial.Code != http.StatusPartialContent || partial.Body.Len() != 2000 {
		t.Errorf("分段请求应该返回 206 和 2000 字节，得到 %d / %d", partial.Code, partial.Body.Len())
	}
	if response := fetch(router, "/unchanged", "Accept-Encoding", "gzip"); response.Code != http.StatusNotModified || response.Body.Len() != 0 {
		t.Errorf("304 应该没有内容，得到 %d / %d 字节", response.Code, response.Body.Len())
	}
}

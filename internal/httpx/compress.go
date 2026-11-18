package httpx

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

const (
	compressMinBytes = 1024    // 比这小的响应不压缩：省不了几个字节
	pooledMaxBytes   = 1 << 20 // 比这大的缓冲用完就丢，不放回池里长期占着内存
)

var (
	bodyPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}
	gzipPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}
)

// bufferedWriter 把响应体先攒在内存里，不直接发出去。
type bufferedWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

func (w *bufferedWriter) Write(data []byte) (int, error)       { return w.body.Write(data) }
func (w *bufferedWriter) WriteString(text string) (int, error) { return w.body.WriteString(text) }
func (w *bufferedWriter) Written() bool                        { return w.body.Len() > 0 || w.ResponseWriter.Written() }
func (w *bufferedWriter) Size() int                            { return w.body.Len() }

// Flush 什么都不做：响应头要等到知道压不压缩之后才能发。
func (w *bufferedWriter) Flush() {}

// Compress 用 gzip 压缩响应。skipPrefixes 里的路径原样放行（比如边下载边转发的图片）。
//
// 做法是先把响应攒在内存里，处理函数结束后一次压完再发。一个压缩器要占大约 1 MB 内存，
// 这样它只在压缩的那几十微秒里被占用，同时存在的数量和 CPU 核数差不多；
// 如果在请求一开始就取一个压缩器，等数据库的那段时间它也被占着，200 个并发请求就是 200 多 MB。
func Compress(skipPrefixes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, prefix := range skipPrefixes {
			if strings.HasPrefix(c.Request.URL.Path, prefix) {
				return
			}
		}
		original := c.Writer
		body := bodyPool.Get().(*bytes.Buffer)
		body.Reset()
		c.Writer = &bufferedWriter{ResponseWriter: original, body: body}
		c.Next()
		c.Writer = original
		if body.Len() > 0 {
			send(c, body.Bytes())
		}
		if body.Cap() <= pooledMaxBytes {
			bodyPool.Put(body)
		}
	}
}

func send(c *gin.Context, body []byte) {
	header := c.Writer.Header()
	if len(body) < compressMinBytes || !compressible(header.Get("Content-Type")) ||
		header.Get("Content-Encoding") != "" || header.Get("Content-Range") != "" {
		c.Writer.Write(body)
		return
	}
	header.Add("Vary", "Accept-Encoding")
	if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
		c.Writer.Write(body)
		return
	}
	header.Set("Content-Encoding", "gzip")
	header.Del("Content-Length")
	writer := gzipPool.Get().(*gzip.Writer)
	writer.Reset(c.Writer)
	writer.Write(body)
	writer.Close()
	writer.Reset(io.Discard)
	gzipPool.Put(writer)
}

// compressible 判断这种内容值不值得压缩。图片、视频、字体本身已经压过了。
func compressible(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "json") ||
		strings.Contains(contentType, "javascript") || strings.Contains(contentType, "xml")
}

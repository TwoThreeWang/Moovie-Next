// Package adskip 维护众包的广告指纹库。
//
// 服务器只存"指纹 + 确认数 + 否认数"，并把结论列表发给所有人；是不是广告由浏览器在本地比对。
// 这样服务器不知道谁在播什么，也不存任何和源、影片、集数有关的信息。
package adskip

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
	"moovie/internal/db"
	"moovie/internal/httpx"
)

// 状态判定的阈值，和浏览器端的自动跳过规则配套。
const (
	MinConfirmCount = 5
	MinConfirmRatio = 0.80
	MinRejectCount  = 3
	// PrefixBytes 是列表里每个指纹保留的字节数。16 字节足够避免碰撞，文件小一半。
	PrefixBytes = 16
)

// Status 根据票数返回指纹的状态：confirmed、rejected 或 pending。
func Status(confirm, reject int) string {
	if confirm >= MinConfirmCount && float64(confirm)/float64(confirm+reject) >= MinConfirmRatio {
		return "confirmed"
	}
	if reject >= MinRejectCount && reject > confirm {
		return "rejected"
	}
	return "pending"
}

// Fingerprint 是后台看到的一个指纹。
type Fingerprint struct {
	Hex       string
	Confirm   int
	Reject    int
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Admin 分页列出指纹（后台只读），最近有人投票的在前。status 为 confirmed / rejected / pending 时只看这一种。
func (s *Store) Admin(ctx context.Context, status string, limit, offset int) ([]Fingerprint, int, error) {
	rows, err := s.pool.Query(ctx, `SELECT encode(fingerprint, 'hex'), confirm_count, reject_count, created_at, updated_at
FROM ad_fingerprints ORDER BY updated_at DESC`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var all []Fingerprint
	for rows.Next() {
		var f Fingerprint
		if err := rows.Scan(&f.Hex, &f.Confirm, &f.Reject, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, 0, err
		}
		f.Status = Status(f.Confirm, f.Reject)
		if status == "" || f.Status == status {
			all = append(all, f)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	total := len(all)
	if offset > total {
		offset = total
	}
	return all[offset:min(total, offset+limit)], total, nil
}

// Store 读写 ad_fingerprints 表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

// Vote 给一个指纹投确认票或否认票。不记录是谁投的。
func (s *Store) Vote(ctx context.Context, fingerprint []byte, confirm bool) error {
	confirmDelta, rejectDelta := 0, 1
	if confirm {
		confirmDelta, rejectDelta = 1, 0
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO ad_fingerprints (fingerprint, confirm_count, reject_count) VALUES ($1, $2, $3)
ON CONFLICT (fingerprint) DO UPDATE SET confirm_count = ad_fingerprints.confirm_count + EXCLUDED.confirm_count,
reject_count = ad_fingerprints.reject_count + EXCLUDED.reject_count, updated_at = now()`, fingerprint, confirmDelta, rejectDelta)
	return err
}

// Lists 返回已确认和已否认的指纹前缀，各自按字节序排好。
func (s *Store) Lists(ctx context.Context) (confirmed, rejected [][]byte, err error) {
	rows, err := s.pool.Query(ctx, `SELECT fingerprint, confirm_count, reject_count FROM ad_fingerprints
WHERE confirm_count >= $1 OR reject_count >= $2 ORDER BY fingerprint`, MinConfirmCount, MinRejectCount)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var fingerprint []byte
		var confirm, reject int
		if err := rows.Scan(&fingerprint, &confirm, &reject); err != nil {
			return nil, nil, err
		}
		switch Status(confirm, reject) {
		case "confirmed":
			confirmed = append(confirmed, fingerprint[:PrefixBytes])
		case "rejected":
			rejected = append(rejected, fingerprint[:PrefixBytes])
		}
	}
	return confirmed, rejected, rows.Err()
}

// Counts 返回指纹库的统计数字。
func (s *Store) Counts(ctx context.Context) (total, confirmed, rejected int, err error) {
	confirmedList, rejectedList, err := s.Lists(ctx)
	if err != nil {
		return
	}
	err = s.pool.QueryRow(ctx, `SELECT count(*) FROM ad_fingerprints`).Scan(&total)
	return total, len(confirmedList), len(rejectedList), err
}

// Encode 把两组指纹前缀编码成下发给浏览器的二进制文件：
//
//	"MVFP" | 版本(1) | 前缀字节数(1) | 保留(2) | 已确认数量(4, 大端) | 已否认数量(4, 大端) | 已确认前缀… | 已否认前缀…
func Encode(confirmed, rejected [][]byte) []byte {
	var buffer bytes.Buffer
	buffer.WriteString("MVFP")
	buffer.Write([]byte{1, PrefixBytes, 0, 0})
	binary.Write(&buffer, binary.BigEndian, uint32(len(confirmed)))
	binary.Write(&buffer, binary.BigEndian, uint32(len(rejected)))
	for _, prefix := range confirmed {
		buffer.Write(prefix)
	}
	for _, prefix := range rejected {
		buffer.Write(prefix)
	}
	return buffer.Bytes()
}

// ---------- HTTP ----------

// Handler 提供指纹列表和投票接口。
type Handler struct {
	store *Store
	votes *httpx.Limiter
	ttl   time.Duration

	mu      sync.Mutex
	body    []byte
	etag    string
	builtAt time.Time
}

// NewHandler 创建 Handler。列表在内存里缓存 10 分钟。
func NewHandler(store *Store) *Handler {
	return &Handler{store: store, votes: httpx.NewLimiter(30, time.Minute), ttl: 10 * time.Minute}
}

// Register 挂载路由。
func (h *Handler) Register(router gin.IRouter) {
	// 指纹列表谁都能取（游客也用在线播放器）：里面只有片段特征的哈希和票数，取的人也不用说自己在看什么。
	// 投票要登录，免得有人匿名刷票把广告洗白，或者把正片标成广告。
	router.GET("/api/ad-fingerprints/list", h.list)
	router.POST("/api/ad-fingerprints/vote", auth.Require(), httpx.RateLimit(h.votes), h.vote)
}

func (h *Handler) current(ctx context.Context) ([]byte, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.body != nil && time.Since(h.builtAt) < h.ttl {
		return h.body, h.etag, nil
	}
	confirmed, rejected, err := h.store.Lists(ctx)
	if err != nil {
		// 数据库暂时不可用时继续用旧列表。
		if h.body != nil {
			return h.body, h.etag, nil
		}
		return nil, "", err
	}
	h.body = Encode(confirmed, rejected)
	sum := sha256.Sum256(h.body)
	h.etag = `"` + hex.EncodeToString(sum[:8]) + `"`
	h.builtAt = time.Now()
	return h.body, h.etag, nil
}

// list 返回指纹列表。所有用户拿到的内容相同，请求里不带任何和播放有关的信息。
func (h *Handler) list(c *gin.Context) {
	body, etag, err := h.current(c.Request.Context())
	if err != nil {
		c.Error(err)
		httpx.Error(c, http.StatusServiceUnavailable, "指纹列表暂时不可用")
		return
	}
	c.Header("ETag", etag)
	c.Header("Cache-Control", "private, max-age=600")
	if c.GetHeader("If-None-Match") == etag {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/octet-stream", body)
}

// vote 接收一票。日志里不记录请求体，也不把用户和指纹关联起来。
func (h *Handler) vote(c *gin.Context) {
	var body struct {
		Fingerprint string `json:"fingerprint"`
		Vote        string `json:"vote"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	fingerprint, err := hex.DecodeString(body.Fingerprint)
	if err != nil || len(fingerprint) != 32 || (body.Vote != "confirm" && body.Vote != "reject") {
		httpx.Error(c, http.StatusBadRequest, "指纹或投票不正确")
		return
	}
	if err := h.store.Vote(c.Request.Context(), fingerprint, body.Vote == "confirm"); err != nil {
		c.Error(err)
		httpx.Error(c, http.StatusInternalServerError, "投票失败")
		return
	}
	c.Status(http.StatusNoContent)
}

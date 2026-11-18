// Package vault 保存端到端加密的观看历史。
//
// 没有元数据的作品，观看历史由浏览器加密后上传。服务器只看到一个不透明的记录 ID 和一段密文，
// 既不知道片名，也不知道用户用的是哪个源。
package vault

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"moovie/internal/auth"
	"moovie/internal/db"
	"moovie/internal/httpx"
)

const (
	// MaxRecords 是每个用户最多能存的记录数。服务器看不到内容，不限制的话会被当成网盘。
	MaxRecords = 2000
	// MaxBlobBytes 是单条密文的最大长度。
	MaxBlobBytes = 1024
)

// Record 是一条加密记录。Blob 为 nil 表示已删除。
type Record struct {
	RID     []byte
	Blob    []byte
	Version int64
}

// ErrConflict 表示提交时依据的版本已经过期。
var ErrConflict = errors.New("记录已被其他设备修改")

// ErrFull 表示记录数已达上限。
var ErrFull = errors.New("加密历史已达上限")

// Store 读写 private_history 表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

// Put 写入一条记录。base 是客户端上次看到的版本（新记录为 0）；
// 和服务器上的版本不一致时返回 ErrConflict 和服务器上的当前记录，由客户端解密后合并。
func (s *Store) Put(ctx context.Context, userID int64, rid, blob []byte, base int64) (version int64, current *Record, err error) {
	if len(rid) != 32 || len(blob) > MaxBlobBytes {
		return 0, nil, errors.New("记录格式不正确")
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var existingVersion int64
		var existingBlob []byte
		scanErr := tx.QueryRow(ctx, `SELECT server_version, blob FROM private_history WHERE user_id = $1 AND rid = $2 FOR UPDATE`,
			userID, rid).Scan(&existingVersion, &existingBlob)
		switch {
		case errors.Is(scanErr, pgx.ErrNoRows):
			if base != 0 {
				return ErrConflict
			}
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM private_history WHERE user_id = $1`, userID).Scan(&count); err != nil {
				return err
			}
			if count >= MaxRecords {
				return ErrFull
			}
			return tx.QueryRow(ctx, `INSERT INTO private_history (user_id, rid, blob) VALUES ($1, $2, $3) RETURNING server_version`,
				userID, rid, blob).Scan(&version)
		case scanErr != nil:
			return scanErr
		case existingVersion != base:
			current = &Record{RID: rid, Blob: existingBlob, Version: existingVersion}
			return ErrConflict
		}
		return tx.QueryRow(ctx, `UPDATE private_history SET blob = $3, updated_at = now(),
server_version = nextval('private_history_version_seq') WHERE user_id = $1 AND rid = $2 RETURNING server_version`,
			userID, rid, blob).Scan(&version)
	})
	return version, current, err
}

// Since 返回某个版本之后变化过的记录，以及当前的最大版本。
func (s *Store) Since(ctx context.Context, userID, since int64, limit int) ([]Record, int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT rid, blob, server_version FROM private_history
WHERE user_id = $1 AND server_version > $2 ORDER BY server_version LIMIT $3`, userID, since, limit)
	if err != nil {
		return nil, since, err
	}
	defer rows.Close()
	records := []Record{}
	version := since
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.RID, &r.Blob, &r.Version); err != nil {
			return nil, since, err
		}
		version = r.Version
		records = append(records, r)
	}
	return records, version, rows.Err()
}

// ---------- HTTP ----------

type wireRecord struct {
	RID     string  `json:"rid"`
	Blob    *string `json:"blob"`
	Version int64   `json:"version"`
}

func toWire(r Record) wireRecord {
	wire := wireRecord{RID: hex.EncodeToString(r.RID), Version: r.Version}
	if r.Blob != nil {
		encoded := base64.StdEncoding.EncodeToString(r.Blob)
		wire.Blob = &encoded
	}
	return wire
}

// Handler 提供加密历史的同步接口。
type Handler struct {
	store *Store
	limit *httpx.Limiter
}

// NewHandler 创建 Handler。
func NewHandler(store *Store) *Handler {
	return &Handler{store: store, limit: httpx.NewLimiter(120, time.Minute)}
}

// Register 挂载路由。
func (h *Handler) Register(router gin.IRouter) {
	api := router.Group("/api/private-history", auth.Require())
	api.GET("", h.since)
	api.PUT("/:rid", h.put)
}

// RegisterKeyring 挂载钥匙圈的接口。
func (h *Handler) RegisterKeyring(router gin.IRouter) {
	api := router.Group("/api/keyring", auth.Require())
	api.GET("", h.getKeyring)
	api.PUT("", h.putKeyring)
}

func (h *Handler) since(c *gin.Context) {
	since, _ := strconv.ParseInt(c.Query("since"), 10, 64)
	records, version, err := h.store.Since(c.Request.Context(), auth.Current(c).ID, since, 500)
	if err != nil {
		c.Error(err)
		httpx.Error(c, http.StatusInternalServerError, "读取失败")
		return
	}
	items := make([]wireRecord, 0, len(records))
	for _, record := range records {
		items = append(items, toWire(record))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "version": version})
}

func (h *Handler) put(c *gin.Context) {
	user := auth.Current(c)
	if !h.limit.Allow(strconv.FormatInt(user.ID, 10)) {
		httpx.Error(c, http.StatusTooManyRequests, "操作太频繁")
		return
	}
	rid, err := hex.DecodeString(c.Param("rid"))
	if err != nil || len(rid) != 32 {
		httpx.Error(c, http.StatusBadRequest, "记录 ID 不正确")
		return
	}
	var body struct {
		Blob *string `json:"blob"`
		Base int64   `json:"base"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	var blob []byte
	if body.Blob != nil {
		if blob, err = base64.StdEncoding.DecodeString(*body.Blob); err != nil || len(blob) == 0 || len(blob) > MaxBlobBytes {
			httpx.Error(c, http.StatusBadRequest, "密文格式不正确")
			return
		}
	}
	version, current, err := h.store.Put(c.Request.Context(), user.ID, rid, blob, body.Base)
	switch {
	case errors.Is(err, ErrConflict):
		response := gin.H{"error": "conflict"}
		if current != nil {
			response["current"] = toWire(*current)
		}
		c.JSON(http.StatusConflict, response)
	case errors.Is(err, ErrFull):
		httpx.Error(c, http.StatusInsufficientStorage, "加密历史已达上限")
	case err != nil:
		c.Error(err)
		httpx.Error(c, http.StatusInternalServerError, "保存失败")
	default:
		c.JSON(http.StatusOK, gin.H{"version": version})
	}
}

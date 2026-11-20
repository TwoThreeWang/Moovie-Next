package vault

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"moovie/internal/auth"
	"moovie/internal/httpx"
)

// Keyring 是一个账号的钥匙圈：设备主密钥和订阅地址的密文。
// 浏览器用登录密码经 PBKDF2 派生的密钥加密（AES-GCM），服务器只存密文、盐和迭代次数。
type Keyring struct {
	Salt       []byte
	Iterations int
	Blob       []byte
	Version    int64
	UpdatedAt  time.Time
}

// ErrNoKeyring 表示这个账号还没有钥匙圈。
var ErrNoKeyring = errors.New("还没有钥匙圈")

// GetKeyring 读取钥匙圈。
func (s *Store) GetKeyring(ctx context.Context, userID int64) (*Keyring, error) {
	var k Keyring
	err := s.pool.QueryRow(ctx, `SELECT salt, iterations, blob, version, updated_at FROM keyrings WHERE user_id = $1`, userID).
		Scan(&k.Salt, &k.Iterations, &k.Blob, &k.Version, &k.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoKeyring
	}
	return &k, err
}

// PutKeyring 保存钥匙圈。base 是客户端看到的版本（还没有时为 0）；和服务器上的不一致时返回 ErrConflict 和当前的钥匙圈。
func (s *Store) PutKeyring(ctx context.Context, userID int64, k Keyring, base int64) (int64, *Keyring, error) {
	var version int64
	var err error
	if base == 0 {
		err = s.pool.QueryRow(ctx, `INSERT INTO keyrings (user_id, salt, iterations, blob) VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO NOTHING RETURNING version`, userID, k.Salt, k.Iterations, k.Blob).Scan(&version)
	} else {
		err = s.pool.QueryRow(ctx, `UPDATE keyrings SET salt = $2, iterations = $3, blob = $4, version = version + 1, updated_at = now()
WHERE user_id = $1 AND version = $5 RETURNING version`, userID, k.Salt, k.Iterations, k.Blob, base).Scan(&version)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		current, getErr := s.GetKeyring(ctx, userID)
		if getErr != nil && !errors.Is(getErr, ErrNoKeyring) {
			return 0, nil, getErr
		}
		return 0, current, ErrConflict
	}
	return version, nil, err
}

type wireKeyring struct {
	Salt       string `json:"salt"`
	Iterations int    `json:"iterations"`
	Blob       string `json:"blob"`
	Version    int64  `json:"version"`
}

func keyringWire(k *Keyring) wireKeyring {
	return wireKeyring{Salt: base64.StdEncoding.EncodeToString(k.Salt), Iterations: k.Iterations,
		Blob: base64.StdEncoding.EncodeToString(k.Blob), Version: k.Version}
}

func (h *Handler) getKeyring(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	k, err := h.store.GetKeyring(c.Request.Context(), auth.Current(c).ID)
	if errors.Is(err, ErrNoKeyring) {
		c.JSON(http.StatusNotFound, gin.H{"error": "none"})
		return
	}
	if err != nil {
		c.Error(err)
		httpx.Error(c, http.StatusInternalServerError, "读取失败")
		return
	}
	c.JSON(http.StatusOK, keyringWire(k))
}

func (h *Handler) putKeyring(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	user := auth.Current(c)
	if !h.limit.Allow("k" + strconv.FormatInt(user.ID, 10)) {
		httpx.Error(c, http.StatusTooManyRequests, "操作太频繁")
		return
	}
	var body struct {
		wireKeyring
		Base int64 `json:"base"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	salt, err1 := base64.StdEncoding.DecodeString(body.Salt)
	blob, err2 := base64.StdEncoding.DecodeString(body.Blob)
	if err1 != nil || err2 != nil || len(salt) != 16 || len(blob) < 29 || len(blob) > 4096 || body.Iterations < 100000 || body.Iterations > 5000000 {
		httpx.Error(c, http.StatusBadRequest, "钥匙圈格式不正确")
		return
	}
	version, current, err := h.store.PutKeyring(c.Request.Context(), user.ID, Keyring{Salt: salt, Iterations: body.Iterations, Blob: blob}, body.Base)
	switch {
	case errors.Is(err, ErrConflict):
		response := gin.H{"error": "conflict"}
		if current != nil {
			response["current"] = keyringWire(current)
		}
		c.JSON(http.StatusConflict, response)
	case err != nil:
		c.Error(err)
		httpx.Error(c, http.StatusInternalServerError, "保存失败")
	default:
		c.JSON(http.StatusOK, gin.H{"version": version})
	}
}

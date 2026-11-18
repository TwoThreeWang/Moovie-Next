package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/httpx"
)

const sessionCookie = "mv_session"

// Sessions 签发和校验登录凭证。凭证是无状态的：用户 ID、密码版本、过期时间，加上 HMAC 签名。
type Sessions struct {
	secret []byte
	ttl    time.Duration
	users  *Store
	now    func() time.Time
}

// NewSessions 创建 Sessions。
func NewSessions(secret string, ttl time.Duration, users *Store) *Sessions {
	return &Sessions{secret: []byte(secret), ttl: ttl, users: users, now: time.Now}
}

func (s *Sessions) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Token 生成登录凭证。
func (s *Sessions) Token(userID int64, epoch int) string {
	payload := fmt.Sprintf("%d.%d.%d", userID, epoch, s.now().Add(s.ttl).Unix())
	return payload + "." + s.sign(payload)
}

// Parse 校验凭证，返回用户 ID 和密码版本。
func (s *Sessions) Parse(token string) (userID int64, epoch int, ok bool) {
	index := strings.LastIndexByte(token, '.')
	if index < 0 {
		return 0, 0, false
	}
	payload, signature := token[:index], token[index+1:]
	if !hmac.Equal([]byte(signature), []byte(s.sign(payload))) {
		return 0, 0, false
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 3 {
		return 0, 0, false
	}
	userID, err1 := strconv.ParseInt(parts[0], 10, 64)
	epoch, err2 := strconv.Atoi(parts[1])
	expires, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || s.now().Unix() > expires {
		return 0, 0, false
	}
	return userID, epoch, true
}

// SignIn 写入登录 Cookie。
func (s *Sessions) SignIn(c *gin.Context, user *User) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: sessionCookie, Value: s.Token(user.ID, user.SessionEpoch), Path: "/",
		MaxAge: int(s.ttl.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(c),
	})
}

// SignOut 清除登录 Cookie。
func (s *Sessions) SignOut(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(c)})
}

func isHTTPS(c *gin.Context) bool {
	return c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https"
}

// Load 是全局中间件：有有效凭证时把用户放进请求上下文，没有也放行。
func (s *Sessions) Load() gin.HandlerFunc {
	return func(c *gin.Context) {
		if token, err := c.Cookie(sessionCookie); err == nil {
			if userID, epoch, ok := s.Parse(token); ok {
				user, err := s.users.ByID(c.Request.Context(), userID)
				if err == nil && user.SessionEpoch == epoch && !user.Banned() {
					c.Set("user", user)
				}
			}
		}
		c.Next()
	}
}

// Current 返回当前登录的用户，未登录返回 nil。
func Current(c *gin.Context) *User {
	if value, ok := c.Get("user"); ok {
		return value.(*User)
	}
	return nil
}

// Require 要求登录。页面请求跳到登录页，接口和 HTMX 请求返回 401。
func Require() gin.HandlerFunc {
	return func(c *gin.Context) {
		if Current(c) != nil {
			c.Next()
			return
		}
		login := "/auth/login?redirect=" + url.QueryEscape(c.Request.URL.RequestURI())
		switch {
		case httpx.WantsJSON(c):
			httpx.Error(c, http.StatusUnauthorized, "请先登录")
		case httpx.IsHTMX(c):
			c.Header("HX-Redirect", "/auth/login")
			c.AbortWithStatus(http.StatusUnauthorized)
		case c.Request.Method == http.MethodGet:
			c.Redirect(http.StatusFound, login)
			c.Abort()
		default:
			httpx.Error(c, http.StatusUnauthorized, "请先登录")
		}
	}
}

// RequireAdmin 要求管理员身份。要放在 Require 之后。
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !Current(c).IsAdmin() {
			httpx.Error(c, http.StatusForbidden, "需要管理员权限")
			return
		}
		c.Next()
	}
}

// SafeRedirect 只接受站内的相对路径，防止登录后被带到别的网站。
func SafeRedirect(target string) string {
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || strings.HasPrefix(target, "/\\") || strings.HasPrefix(target, "/auth/") {
		return "/"
	}
	return target
}

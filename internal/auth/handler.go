package auth

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"moovie/internal/httpx"
	"moovie/internal/mail"
	"moovie/internal/pow"
)

// PageRenderer 是 auth 需要的渲染能力，由 ui.Renderer 实现。
type PageRenderer interface {
	Page(c *gin.Context, status int, name string, data gin.H)
}

// Handler 处理登录、注册和账号设置。
type Handler struct {
	users    *Store
	sessions *Sessions
	render   PageRenderer
	login    *httpx.Limiter
	signup   *httpx.Limiter
	mailIP   *httpx.Limiter
	// PoW 是登录、注册表单的人机校验；为 nil 时不校验（测试用）。
	PoW *pow.Issuer
	// Mail 发注册密码、重置链接和确认新邮箱的邮件；为 nil 时这些功能不可用。
	Mail *mail.Mailer
	// Secret 用来签名重置密码、确认邮箱的链接。
	Secret   []byte
	SiteURL  string
	SiteName string
}

// challenge 给表单签发人机校验的挑战。
func (h *Handler) challenge(scope string) string {
	if h.PoW == nil {
		return ""
	}
	return h.PoW.Issue(scope)
}

// human 校验表单里的人机校验答案。
func (h *Handler) human(c *gin.Context, scope string) bool {
	return h.PoW == nil || h.PoW.Verify(scope, c.PostForm(pow.TokenField), c.PostForm(pow.NonceField)) == nil
}

// NewHandler 创建 Handler。
func NewHandler(users *Store, sessions *Sessions, render PageRenderer) *Handler {
	return &Handler{
		users: users, sessions: sessions, render: render,
		login:  httpx.NewLimiter(10, time.Minute),
		signup: httpx.NewLimiter(10, time.Hour),
		mailIP: httpx.NewLimiter(10, time.Hour),
	}
}

// Register 挂载路由。
func (h *Handler) Register(router gin.IRouter) {
	router.GET("/auth/login", h.loginPage)
	router.POST("/auth/login", httpx.RateLimit(h.login), h.loginSubmit)
	router.GET("/auth/register", h.registerPage)
	router.POST("/auth/register", httpx.RateLimit(h.signup), h.registerSubmit)
	router.POST("/auth/logout", h.logout)
	router.GET("/auth/forgot", h.forgotPage)
	router.POST("/auth/forgot", httpx.RateLimit(h.signup), h.forgotSubmit)
	router.GET("/auth/reset", h.resetPage)
	router.POST("/auth/reset", httpx.RateLimit(h.login), h.resetSubmit)
	router.GET("/auth/email", h.confirmEmail)

	settings := router.Group("/settings", Require())
	settings.POST("/email", h.updateEmail)
	settings.POST("/username", h.updateUsername)
	settings.POST("/password", h.updatePassword)
	settings.POST("/avatar", h.updateAvatar)
	settings.POST("/public", h.updatePublic)
	settings.POST("/ad-skip", h.updateAdSkip)
}

var usernamePattern = regexp.MustCompile(`^[\p{Han}A-Za-z0-9_\-]{2,20}$`)

// dummyHash 用于账号不存在时也做一次 bcrypt 比对，让两种情况耗时一致。
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("moovie-dummy-password"), bcrypt.DefaultCost)

func (h *Handler) loginPage(c *gin.Context) {
	if Current(c) != nil {
		c.Redirect(http.StatusFound, SafeRedirect(c.Query("redirect")))
		return
	}
	h.page(c, http.StatusOK, "login.html", gin.H{"Title": "登录", "Redirect": SafeRedirect(c.Query("redirect")), "Email": c.Query("email"),
		"PoW": h.challenge("login"), "Scripts": []string{"pages/auth.js"}})
}

// wantsJSON：登录、重置密码页由脚本提交时要 JSON 结果。脚本拿到结果后，
// 趁密码还在内存里同步钥匙圈（设备密钥和订阅地址），再跳转。
func wantsJSON(c *gin.Context) bool { return c.GetHeader("Accept") == "application/json" }

func (h *Handler) loginSubmit(c *gin.Context) {
	email, password := c.PostForm("email"), c.PostForm("password")
	redirect := SafeRedirect(c.PostForm("redirect"))
	if wantsJSON(c) {
		h.loginJSON(c, email, password, redirect)
		return
	}
	if !h.human(c, "login") {
		h.page(c, http.StatusBadRequest, "login.html", gin.H{
			"Title": "登录", "Redirect": redirect, "Email": email, "Error": "安全校验没有通过，请再试一次",
			"PoW": h.challenge("login"), "Scripts": []string{"pages/auth.js"},
		})
		return
	}
	user, err := h.users.ByEmail(c.Request.Context(), email)
	hash := dummyHash
	if err == nil {
		hash = []byte(user.PasswordHash)
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || err != nil {
		h.page(c, http.StatusUnauthorized, "login.html", gin.H{
			"Title": "登录", "Redirect": redirect, "Email": email, "Error": "邮箱或密码不正确",
			"PoW": h.challenge("login"), "Scripts": []string{"pages/auth.js"},
		})
		return
	}
	if user.Banned() {
		h.page(c, http.StatusForbidden, "login.html", gin.H{
			"Title": "登录", "Redirect": redirect, "Email": email, "Error": "账号" + user.RestrictionText() + "。有疑问可以发邮件联系我们。",
			"PoW": h.challenge("login"), "Scripts": []string{"pages/auth.js"},
		})
		return
	}
	h.sessions.SignIn(c, user)
	c.Redirect(http.StatusSeeOther, redirect)
}

func (h *Handler) loginJSON(c *gin.Context, email, password, redirect string) {
	fail := func(status int, message string) {
		c.JSON(status, gin.H{"error": message, "pow": h.challenge("login")})
	}
	if !h.human(c, "login") {
		fail(http.StatusBadRequest, "安全校验没有通过，请再试一次")
		return
	}
	user, err := h.users.ByEmail(c.Request.Context(), email)
	hash := dummyHash
	if err == nil {
		hash = []byte(user.PasswordHash)
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || err != nil {
		fail(http.StatusUnauthorized, "邮箱或密码不正确")
		return
	}
	if user.Banned() {
		fail(http.StatusForbidden, "账号"+user.RestrictionText()+"。有疑问可以发邮件联系我们。")
		return
	}
	h.sessions.SignIn(c, user)
	c.JSON(http.StatusOK, gin.H{"redirect": redirect, "user_id": user.ID})
}

func (h *Handler) registerPage(c *gin.Context) {
	if Current(c) != nil {
		c.Redirect(http.StatusFound, "/")
		return
	}
	h.page(c, http.StatusOK, "register.html", gin.H{"Title": "注册", "PoW": h.challenge("register"), "Scripts": []string{"pages/auth.js"}})
}

func (h *Handler) logout(c *gin.Context) {
	h.sessions.SignOut(c)
	c.Redirect(http.StatusSeeOther, "/")
}

func validatePassword(password string) string {
	if utf8.RuneCountInString(password) < 8 {
		return "密码至少 8 位"
	}
	if len(password) > 72 {
		return "密码不能超过 72 个字节"
	}
	return ""
}

// done 把设置页的操作结果带回设置页显示。
func done(c *gin.Context, section, errCode string) {
	query := url.Values{}
	if errCode != "" {
		query.Set("error", errCode)
	} else {
		query.Set("saved", section)
	}
	c.Redirect(http.StatusSeeOther, "/settings?"+query.Encode()+"#"+section)
}

func (h *Handler) updateUsername(c *gin.Context) {
	username := strings.TrimSpace(c.PostForm("username"))
	if !usernamePattern.MatchString(username) {
		done(c, "account", "username_invalid")
		return
	}
	err := h.users.Update(c.Request.Context(), Current(c).ID, "username", username)
	if errors.Is(err, ErrTaken) {
		done(c, "account", "taken")
		return
	}
	if err != nil {
		c.Error(err)
		done(c, "account", "failed")
		return
	}
	done(c, "account", "")
}

func (h *Handler) updatePassword(c *gin.Context) {
	user := Current(c)
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(c.PostForm("current"))) != nil {
		done(c, "account", "password_wrong")
		return
	}
	password := c.PostForm("password")
	if validatePassword(password) != "" {
		done(c, "account", "password_weak")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		done(c, "account", "failed")
		return
	}
	epoch, err := h.users.SetPassword(c.Request.Context(), user.ID, string(hash))
	if err != nil {
		c.Error(err)
		done(c, "account", "failed")
		return
	}
	// 其他设备的登录已经失效，当前设备换一张新凭证继续用。
	user.SessionEpoch = epoch
	h.sessions.SignIn(c, user)
	done(c, "account", "")
}

// Avatars 是设置页里列出的头像；用户也可以自己输入任意一个 emoji（见 ValidAvatar）。
var Avatars = []string{"🎬", "🍿", "🎞️", "📺", "🎥", "🌙", "🦊", "🐱", "🐼", "🐧", "🦉", "🐳", "🌵", "🍉", "🚀", "👾"}

// ValidAvatar 判断 s 是不是恰好一个 emoji：单个表情、带肤色或变体的、用 ZWJ 拼起来的（👨‍👩‍👧）、
// 国旗（两个区域字母）、键帽（1️⃣）和地区旗（🏴 加标签字符）都算；文字、多个 emoji 不算。
func ValidAvatar(s string) bool {
	if s == "" || len(s) > 64 || !utf8.ValidString(s) {
		return false
	}
	runes := []rune(s)
	clusters := 0
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == 0x200D || r == 0xFE0F || r == 0xFE0E || r == 0x20E3 || (r >= 0x1F3FB && r <= 0x1F3FF) || (r >= 0xE0020 && r <= 0xE007F):
			if i == 0 {
				return false // 修饰符不能打头
			}
		case r >= 0x1F1E6 && r <= 0x1F1FF: // 国旗：两个区域字母算一个
			if i+1 >= len(runes) || runes[i+1] < 0x1F1E6 || runes[i+1] > 0x1F1FF {
				return false
			}
			i++
			clusters++
		case (r >= '0' && r <= '9') || r == '#' || r == '*': // 键帽：数字后面必须跟着 U+20E3
			if !strings.ContainsRune(string(runes[i:]), 0x20E3) {
				return false
			}
			clusters++
		case isPictograph(r):
			if i == 0 || runes[i-1] != 0x200D {
				clusters++
			}
		default:
			return false
		}
	}
	return clusters == 1
}

func isPictograph(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, r >= 0x2600 && r <= 0x27BF, r >= 0x2300 && r <= 0x23FF,
		r >= 0x2B00 && r <= 0x2BFF, r >= 0x2190 && r <= 0x21FF, r >= 0x25A0 && r <= 0x25FF, r >= 0x2934 && r <= 0x2935:
		return true
	}
	switch r {
	case 0x00A9, 0x00AE, 0x203C, 0x2049, 0x2122, 0x2139, 0x3030, 0x303D, 0x3297, 0x3299:
		return true
	}
	return false
}

func (h *Handler) updateAvatar(c *gin.Context) {
	avatar := strings.TrimSpace(c.PostForm("avatar"))
	if !ValidAvatar(avatar) {
		done(c, "account", "avatar_invalid")
		return
	}
	if err := h.users.Update(c.Request.Context(), Current(c).ID, "avatar", avatar); err != nil {
		c.Error(err)
		done(c, "account", "failed")
		return
	}
	done(c, "account", "")
}

func (h *Handler) updatePublic(c *gin.Context) {
	if err := h.users.Update(c.Request.Context(), Current(c).ID, "is_public", c.PostForm("public") == "on"); err != nil {
		c.Error(err)
		done(c, "privacy", "failed")
		return
	}
	done(c, "privacy", "")
}

func (h *Handler) updateAdSkip(c *gin.Context) {
	if err := h.users.Update(c.Request.Context(), Current(c).ID, "ad_skip_enabled", c.PostForm("enabled") == "on"); err != nil {
		c.Error(err)
		done(c, "playback", "failed")
		return
	}
	done(c, "playback", "")
}

// page 渲染登录、注册这类页面：不显示导航栏，专心完成一件事。
func (h *Handler) page(c *gin.Context, status int, name string, data gin.H) {
	data["Bare"] = true
	h.render.Page(c, status, name, data)
}

// RequireVoice 拦住被禁言的用户（发短评、回复、弹幕的接口用）。
func RequireVoice() gin.HandlerFunc {
	return func(c *gin.Context) {
		if user := Current(c); user.Muted() {
			httpx.Error(c, http.StatusForbidden, "你"+user.RestrictionText()+"，暂时不能发言")
			return
		}
		c.Next()
	}
}

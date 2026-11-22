package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"moovie/internal/mail"
)

// 和邮件有关的流程：注册（发随机密码）、找回密码、修改邮箱。
//
// 防刷分四层：人机校验（注册、找回）→ 邮箱格式和收件域名（当场提示，不占次数）
// → 每个 IP 每小时的发信次数 → 发信层的收件人间隔、每日上限和全站每小时上限。

const (
	resetTTL = 30 * time.Minute
	emailTTL = 24 * time.Hour
)

var errLinkInvalid = errors.New("链接无效或已过期")

// passwordAlphabet 去掉了容易看错的 0/O、1/l/I。
const passwordAlphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// RandomPassword 生成一个 xxxx-xxxx-xxxx 形式的随机密码（约 70 位熵）。
func RandomPassword() string {
	var b strings.Builder
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := 0; i < 12; i++ {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b.WriteByte(passwordAlphabet[n.Int64()])
	}
	return b.String()
}

// usernameBase 从邮箱的 @ 前面一段生成用户名，不合规的字符去掉。
func usernameBase(email string) string {
	local, _, _ := strings.Cut(email, "@")
	var b strings.Builder
	count := 0
	for _, r := range local {
		if count >= 14 {
			break
		}
		if unicode.Is(unicode.Han, r) || (r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-')) {
			b.WriteRune(r)
			count++
		}
	}
	if count < 2 {
		return "影迷"
	}
	return b.String()
}

func randomDigits(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		d, _ := rand.Int(rand.Reader, big.NewInt(10))
		b.WriteByte(byte('0' + d.Int64()))
	}
	return b.String()
}

// createAccount 用自动生成的用户名建账号；用户名被占用时换一个。
func (h *Handler) createAccount(ctx context.Context, email, passwordHash string) (*User, error) {
	base := usernameBase(email)
	name := base
	for attempt := 0; attempt < 6; attempt++ {
		user, err := h.users.Create(ctx, email, name, passwordHash)
		if !errors.Is(err, ErrTaken) {
			return user, err
		}
		if _, lookup := h.users.ByEmail(ctx, email); lookup == nil {
			return nil, ErrTaken // 邮箱被占用，换用户名没用
		}
		name = base + "_" + randomDigits(4)
	}
	return nil, ErrTaken
}

// ---------- 签名链接 ----------

func (h *Handler) sign(parts ...string) string {
	mac := hmac.New(sha256.New, h.Secret)
	mac.Write([]byte(strings.Join(parts, "\x00")))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:18])
}

// resetToken 生成重置密码的令牌。签名里带着当前的密码哈希，密码一改旧链接就失效（只能用一次）。
func (h *Handler) resetToken(user *User, now time.Time) string {
	id, expires := strconv.FormatInt(user.ID, 10), strconv.FormatInt(now.Add(resetTTL).Unix(), 10)
	return id + "." + expires + "." + h.sign("reset", id, expires, user.PasswordHash)
}

// ResetLink 返回某个账号的重置密码链接。邮件发不出去时，管理员可以把它直接发给用户。
func (h *Handler) ResetLink(user *User) string {
	return h.SiteURL + "/auth/reset?t=" + url.QueryEscape(h.resetToken(user, time.Now()))
}

func (h *Handler) checkResetToken(ctx context.Context, token string) (*User, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errLinkInvalid
	}
	id, err1 := strconv.ParseInt(parts[0], 10, 64)
	expires, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || time.Now().Unix() > expires {
		return nil, errLinkInvalid
	}
	user, err := h.users.ByID(ctx, id)
	if err != nil {
		return nil, errLinkInvalid
	}
	if !hmac.Equal([]byte(parts[2]), []byte(h.sign("reset", parts[0], parts[1], user.PasswordHash))) {
		return nil, errLinkInvalid
	}
	return user, nil
}

// emailToken 生成确认新邮箱的令牌。签名里带着当前邮箱，邮箱改过之后旧链接失效。
func (h *Handler) emailToken(user *User, newEmail string, now time.Time) string {
	id, expires := strconv.FormatInt(user.ID, 10), strconv.FormatInt(now.Add(emailTTL).Unix(), 10)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(newEmail))
	return id + "." + expires + "." + encoded + "." + h.sign("email", id, expires, newEmail, user.Email)
}

func (h *Handler) checkEmailToken(ctx context.Context, token string) (*User, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return nil, "", errLinkInvalid
	}
	id, err1 := strconv.ParseInt(parts[0], 10, 64)
	expires, err2 := strconv.ParseInt(parts[1], 10, 64)
	decoded, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || time.Now().Unix() > expires {
		return nil, "", errLinkInvalid
	}
	user, err := h.users.ByID(ctx, id)
	if err != nil {
		return nil, "", errLinkInvalid
	}
	newEmail := string(decoded)
	if !hmac.Equal([]byte(parts[3]), []byte(h.sign("email", parts[0], parts[1], newEmail, user.Email))) {
		return nil, "", errLinkInvalid
	}
	return user, newEmail, nil
}

// ---------- 页面 ----------

// mailQuota 按 IP 限制触发发信的次数。放在邮箱检查之后：填错邮箱不占次数。
func (h *Handler) mailQuota(c *gin.Context) bool { return h.mailIP.Allow(c.ClientIP()) }

const errMailQuota = "操作太频繁，请过一会儿再试"

func (h *Handler) message(c *gin.Context, status int, heading, message, link, linkText string) {
	h.page(c, status, "message.html", gin.H{"Title": heading, "Heading": heading, "Message": message, "Link": link, "LinkText": linkText})
}

func (h *Handler) registerSubmit(c *gin.Context) {
	ctx := c.Request.Context()
	email := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	fail := func(status int, message string, exists bool) {
		h.page(c, status, "register.html", gin.H{"Title": "注册", "Email": email, "Error": message, "Exists": exists,
			"PoW": h.challenge("register"), "Scripts": []string{"pages/auth.js"}})
	}
	// website 是给机器人准备的隐藏字段，正常用户不会填。
	if c.PostForm("website") != "" {
		fail(http.StatusBadRequest, "注册失败，请重试", false)
		return
	}
	if !h.human(c, "register") {
		fail(http.StatusBadRequest, "安全校验没有通过，请再试一次", false)
		return
	}
	if err := mail.Validate(ctx, h.Mail.Checker(), email); err != nil {
		fail(http.StatusBadRequest, err.Error(), false)
		return
	}
	if !h.Mail.Enabled() {
		fail(http.StatusServiceUnavailable, mail.ErrDisabled.Error(), false)
		return
	}
	if _, err := h.users.ByEmail(ctx, email); err == nil {
		fail(http.StatusConflict, "这个邮箱已经注册过了", true)
		return
	}
	if !h.mailQuota(c) {
		fail(http.StatusTooManyRequests, errMailQuota, false)
		return
	}
	password := RandomPassword()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fail(http.StatusInternalServerError, "注册失败，请重试", false)
		return
	}
	user, err := h.createAccount(ctx, email, string(hash))
	if errors.Is(err, ErrTaken) {
		fail(http.StatusConflict, "这个邮箱已经注册过了", true)
		return
	}
	if err != nil {
		c.Error(err)
		fail(http.StatusInternalServerError, "注册失败，请重试", false)
		return
	}
	err = h.Mail.Send(ctx, mail.Message{To: email, Kind: "register", Subject: "欢迎使用" + h.SiteName + "：你的登录密码",
		Title: "欢迎来到" + h.SiteName,
		Text: "你用这个邮箱注册了" + h.SiteName + "，登录信息如下：\n\n" +
			"邮箱：" + email + "\n密码：" + password + "\n\n" +
			"你现在的用户名是「" + user.Username + "」。登录后可以在「设置 → 账号」里修改用户名和密码。",
		Button: mail.Button{Label: "登录" + h.SiteName, URL: h.SiteURL + welcomeLogin(email)},
		Note:   "如果不是你本人注册的，忽略这封邮件即可：没有这个密码，别人无法登录。"})
	if err != nil {
		// 密码没送到，账号就没法用，删掉让用户重新注册。
		_ = h.users.Delete(ctx, user.ID)
		fail(http.StatusServiceUnavailable, err.Error(), false)
		return
	}
	h.page(c, http.StatusOK, "message.html", gin.H{"Title": "查收邮件", "Heading": "密码已经发到你的邮箱",
		"Message": "我们给 " + email + " 发了一封邮件，里面有登录密码。没收到的话，看看垃圾邮件文件夹。",
		"Link":    welcomeLogin(email), "LinkText": "去登录"})
}

func (h *Handler) forgotPage(c *gin.Context) {
	h.page(c, http.StatusOK, "forgot.html", gin.H{"Title": "找回密码", "Email": c.Query("email"),
		"PoW": h.challenge("forgot"), "Scripts": []string{"pages/auth.js"}})
}

func (h *Handler) forgotSubmit(c *gin.Context) {
	ctx := c.Request.Context()
	email := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	fail := func(status int, message string) {
		h.page(c, status, "forgot.html", gin.H{"Title": "找回密码", "Email": email, "Error": message,
			"PoW": h.challenge("forgot"), "Scripts": []string{"pages/auth.js"}})
	}
	if !h.human(c, "forgot") {
		fail(http.StatusBadRequest, "安全校验没有通过，请再试一次")
		return
	}
	if err := mail.Validate(ctx, h.Mail.Checker(), email); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if !h.Mail.Enabled() {
		fail(http.StatusServiceUnavailable, mail.ErrDisabled.Error())
		return
	}
	if !h.mailQuota(c) {
		fail(http.StatusTooManyRequests, errMailQuota)
		return
	}
	// 邮箱没注册过时也显示同样的结果，不让人用这个页面试探谁注册过。
	if user, err := h.users.ByEmail(ctx, email); err == nil {
		err = h.Mail.Send(ctx, mail.Message{To: user.Email, Kind: "reset", Subject: h.SiteName + "：重置密码",
			Title:  "重置密码",
			Text:   "有人（希望是你）在" + h.SiteName + "申请重置这个邮箱对应账号的密码。点下面的按钮设置新密码：",
			Button: mail.Button{Label: "设置新密码", URL: h.ResetLink(user)},
			Note:   "链接 30 分钟内有效，只能用一次。如果不是你本人操作，忽略这封邮件即可，密码不会改变。"})
		if err != nil && !errors.Is(err, mail.ErrUndeliverable) {
			fail(http.StatusServiceUnavailable, err.Error())
			return
		}
	}
	h.message(c, http.StatusOK, "查收邮件", "如果 "+email+" 注册过，重置密码的链接已经发过去了，30 分钟内有效。没收到的话，看看垃圾邮件文件夹。",
		"/auth/login?email="+url.QueryEscape(email), "回到登录")
}

func (h *Handler) resetPage(c *gin.Context) {
	if _, err := h.checkResetToken(c.Request.Context(), c.Query("t")); err != nil {
		h.message(c, http.StatusBadRequest, "链接无效", "这个链接已经用过或者过期了，请重新申请。", "/auth/forgot", "重新申请")
		return
	}
	h.page(c, http.StatusOK, "reset.html", gin.H{"Title": "设置新密码", "Token": c.Query("t"), "Scripts": []string{"pages/reset.js"}})
}

func (h *Handler) resetSubmit(c *gin.Context) {
	ctx, token := c.Request.Context(), c.PostForm("t")
	user, err := h.checkResetToken(ctx, token)
	if err != nil && wantsJSON(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "这个链接已经用过或者过期了，请重新申请。"})
		return
	}
	if err != nil {
		h.message(c, http.StatusBadRequest, "链接无效", "这个链接已经用过或者过期了，请重新申请。", "/auth/forgot", "重新申请")
		return
	}
	password := c.PostForm("password")
	if message := validatePassword(password); message != "" {
		if wantsJSON(c) {
			c.JSON(http.StatusBadRequest, gin.H{"error": message})
			return
		}
		h.page(c, http.StatusBadRequest, "reset.html", gin.H{"Title": "设置新密码", "Token": token, "Error": message})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		h.message(c, http.StatusInternalServerError, "出错了", "设置失败，请重试。", "/auth/forgot", "重新申请")
		return
	}
	epoch, err := h.users.SetPassword(ctx, user.ID, string(hash))
	if err != nil {
		c.Error(err)
		h.message(c, http.StatusInternalServerError, "出错了", "设置失败，请重试。", "/auth/forgot", "重新申请")
		return
	}
	user.SessionEpoch = epoch
	h.sessions.SignIn(c, user)
	if wantsJSON(c) {
		c.JSON(http.StatusOK, gin.H{"redirect": "/", "user_id": user.ID})
		return
	}
	c.Redirect(http.StatusSeeOther, "/")
}

func (h *Handler) updateEmail(c *gin.Context) {
	ctx, user := c.Request.Context(), Current(c)
	email := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	if email == user.Email {
		done(c, "account", "")
		return
	}
	switch err := mail.Validate(ctx, h.Mail.Checker(), email); {
	case errors.Is(err, mail.ErrUndeliverable):
		done(c, "account", "email_undeliverable")
		return
	case err != nil:
		done(c, "account", "email_invalid")
		return
	}
	if _, err := h.users.ByEmail(ctx, email); err == nil {
		done(c, "account", "taken")
		return
	}
	if !h.mailQuota(c) {
		done(c, "account", "mail_quota")
		return
	}
	link := h.SiteURL + "/auth/email?t=" + url.QueryEscape(h.emailToken(user, email, time.Now()))
	err := h.Mail.Send(ctx, mail.Message{To: email, Kind: "email", Subject: h.SiteName + "：确认新邮箱",
		Title:  "确认新邮箱",
		Text:   h.SiteName + "的用户「" + user.Username + "」要把登录邮箱改成这个地址。点下面的按钮确认：",
		Button: mail.Button{Label: "确认新邮箱", URL: link},
		Note:   "链接 24 小时内有效。确认之前，登录邮箱不会改变。如果不是你本人操作，忽略这封邮件即可。"})
	if err != nil {
		done(c, "account", mailErrorCode(err))
		return
	}
	c.Redirect(http.StatusSeeOther, "/settings?notice=email_sent#account")
}

// mailErrorCode 把发信错误转成设置页的提示代码。
func mailErrorCode(err error) string {
	switch {
	case errors.Is(err, mail.ErrUndeliverable):
		return "email_undeliverable"
	case errors.Is(err, mail.ErrTooSoon), errors.Is(err, mail.ErrTooMany), errors.Is(err, mail.ErrBusy):
		return "mail_quota"
	case errors.Is(err, mail.ErrDisabled):
		return "mail_disabled"
	}
	return "mail_failed"
}

func (h *Handler) confirmEmail(c *gin.Context) {
	ctx := c.Request.Context()
	user, email, err := h.checkEmailToken(ctx, c.Query("t"))
	if err != nil {
		h.message(c, http.StatusBadRequest, "链接无效", "这个链接已经用过或者过期了。可以在设置里重新修改邮箱。", "/settings#account", "去设置")
		return
	}
	old := user.Email
	err = h.users.Update(ctx, user.ID, "email", email)
	if errors.Is(err, ErrTaken) {
		h.message(c, http.StatusConflict, "邮箱已被占用", email+" 已经被别的账号用了。", "/settings#account", "去设置")
		return
	}
	if err != nil {
		c.Error(err)
		h.message(c, http.StatusInternalServerError, "出错了", "修改失败，请重试。", "/settings#account", "去设置")
		return
	}
	// 通知旧邮箱，万一不是本人操作还能发现。发不出去不影响结果。
	_ = h.Mail.Send(ctx, mail.Message{To: old, Kind: "email-changed", Security: true, Subject: h.SiteName + "：登录邮箱已修改",
		Title:  "登录邮箱已修改",
		Text:   "你在" + h.SiteName + "的账号「" + user.Username + "」的登录邮箱已经改为 " + maskEmail(email) + "。\n\n如果不是你本人操作，请尽快联系我们。",
		Button: mail.Button{Label: "联系我们", URL: h.SiteURL + "/feedback"}})
	h.message(c, http.StatusOK, "邮箱已修改", "以后请用 "+email+" 登录。", "/settings#account", "回到设置")
}

// welcomeLogin 是新用户的登录地址：登录后先去设置播放源（没有订阅就不能播放）。
func welcomeLogin(email string) string {
	return "/auth/login?" + url.Values{"email": {email}, "redirect": {"/settings?welcome=1#sources"}}.Encode()
}

// maskEmail 把邮箱中间部分换成星号：evan@example.com → e***@example.com。
func maskEmail(email string) string {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" {
		return email
	}
	first := []rune(local)[0]
	return string(first) + "***@" + domain
}

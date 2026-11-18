// Package mail 负责发邮件：SMTP 发信、收件域名检查、发信频率限制。
//
// 只发和账号有关的邮件（注册密码、重置密码、确认新邮箱），不发通知和营销邮件。
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"moovie/internal/db"
)

var (
	// ErrDisabled 表示没有配置邮件服务。
	ErrDisabled = errors.New("邮件服务暂时不可用，请联系管理员")
	// ErrInvalid 表示邮箱格式不对。
	ErrInvalid = errors.New("邮箱格式不正确")
	// ErrTooSoon 表示同一个邮箱刚收过一封。
	ErrTooSoon = errors.New("邮件发得太频繁，请 1 分钟后再试")
	// ErrTooMany 表示同一个邮箱一天内收得太多。
	ErrTooMany = errors.New("这个邮箱今天收到的邮件太多了，请明天再试")
	// ErrBusy 表示全站这一小时发得太多（防止被刷）。
	ErrBusy = errors.New("邮件服务繁忙，请稍后再试")
	// ErrSend 表示邮件服务器拒收或连不上。
	ErrSend = errors.New("邮件发送失败，请稍后再试")
)

// Message 是一封纯文本邮件。Kind 用于日志和频率统计（register、reset、email 等）。
type Message struct {
	To      string
	Subject string
	Text    string
	Kind    string
	// Security 表示安全提醒（如"邮箱已修改"）：不受单个收件人的频率限制，只受全站上限，
	// 免得有人先刷满对方的次数，让提醒发不出去。
	Security bool
}

// Transport 把一封编好的邮件交给邮件服务器。
type Transport interface {
	Send(ctx context.Context, from, to string, raw []byte) error
}

// Limits 是发信频率限制。
type Limits struct {
	Gap     time.Duration // 同一个收件人两封之间至少隔多久
	PerDay  int           // 同一个收件人 24 小时内最多几封
	PerHour int           // 全站每小时最多几封
}

// DefaultLimits 是默认的发信频率限制。
var DefaultLimits = Limits{Gap: time.Minute, PerDay: 5, PerHour: 200}

// Mailer 发邮件。所有邮件都经过这里：先查收件域名，再查频率，最后交给 Transport。
type Mailer struct {
	transport Transport
	checker   *Checker
	db        db.Querier
	from      string
	limits    Limits
	log       *slog.Logger
}

// New 创建 Mailer。transport 为 nil 时 Send 一律返回 ErrDisabled。
func New(transport Transport, checker *Checker, q db.Querier, from string, limits Limits, log *slog.Logger) *Mailer {
	return &Mailer{transport: transport, checker: checker, db: q, from: from, limits: limits, log: log}
}

// Enabled 表示配置了邮件服务。
func (m *Mailer) Enabled() bool { return m != nil && m.transport != nil }

// Checker 返回收件域名检查器，给填写邮箱的入口当场检查用。
func (m *Mailer) Checker() *Checker {
	if m == nil {
		return nil
	}
	return m.checker
}

// Validate 检查用户填写的新邮箱：格式和收件域名。用在注册、改邮箱这些入口，放在发信计数之前。
func Validate(ctx context.Context, checker *Checker, address string) error {
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address || len(address) > 254 || strings.ContainsAny(address, "\r\n") {
		return ErrInvalid
	}
	if checker == nil {
		return nil
	}
	return checker.Check(ctx, address)
}

// Send 发一封邮件。收件域名收不了信时跳过并记日志，返回 ErrUndeliverable。
func (m *Mailer) Send(ctx context.Context, message Message) error {
	if !m.Enabled() {
		return ErrDisabled
	}
	if err := Validate(ctx, m.checker, message.To); err != nil {
		m.log.Warn("收件地址收不了信，跳过", "kind", message.Kind, "domain", domainOf(message.To), "err", err)
		return err
	}
	if err := m.reserve(ctx, message.To, message.Kind, message.Security); err != nil {
		m.log.Warn("发信频率超限", "kind", message.Kind, "domain", domainOf(message.To), "err", err)
		return err
	}
	raw := build(m.from, message, time.Now())
	if err := m.transport.Send(ctx, addressOf(m.from), message.To, raw); err != nil {
		m.log.Error("发信失败", "kind", message.Kind, "domain", domainOf(message.To), "err", err)
		return ErrSend
	}
	m.log.Info("已发信", "kind", message.Kind, "domain", domainOf(message.To))
	return nil
}

// reserve 检查并记下一次发信。只存收件人的哈希，不存邮箱本身。
func (m *Mailer) reserve(ctx context.Context, to, kind string, security bool) error {
	recipient := Fingerprint(to)
	var last *time.Time
	var today, hour int
	err := m.db.QueryRow(ctx, `SELECT max(created_at) FILTER (WHERE recipient = $1),
count(*) FILTER (WHERE recipient = $1), count(*) FILTER (WHERE created_at > now() - interval '1 hour')
FROM mail_log WHERE created_at > now() - interval '1 day'`, recipient).Scan(&last, &today, &hour)
	if err != nil {
		return err
	}
	switch {
	case m.limits.PerHour > 0 && hour >= m.limits.PerHour:
		return ErrBusy
	case security: // 安全提醒不看单个收件人的限制
	case m.limits.PerDay > 0 && today >= m.limits.PerDay:
		return ErrTooMany
	case last != nil && time.Since(*last) < m.limits.Gap:
		return ErrTooSoon
	}
	_, err = m.db.Exec(ctx, `INSERT INTO mail_log (recipient, kind) VALUES ($1, $2)`, recipient, kind)
	return err
}

// Prune 删除 35 天前的发信记录（后台的趋势图看最近 30 天）。
func (m *Mailer) Prune(ctx context.Context) error {
	_, err := m.db.Exec(ctx, `DELETE FROM mail_log WHERE created_at < now() - interval '35 days'`)
	return err
}

// Fingerprint 是邮箱的哈希，用于频率统计。
func Fingerprint(address string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(address))))
	return hex.EncodeToString(sum[:])
}

func domainOf(address string) string {
	if at := strings.LastIndexByte(address, '@'); at >= 0 {
		return strings.ToLower(address[at+1:])
	}
	return ""
}

func addressOf(from string) string {
	if parsed, err := mail.ParseAddress(from); err == nil {
		return parsed.Address
	}
	return from
}

// build 编码一封纯文本 UTF-8 邮件。
func build(from string, message Message, now time.Time) []byte {
	clean := func(s string) string { return strings.NewReplacer("\r", "", "\n", "").Replace(s) }
	sender := from
	if parsed, err := mail.ParseAddress(from); err == nil {
		sender = parsed.String() // 显示名会按 RFC 2047 编码
	}
	var id [12]byte
	_, _ = rand.Read(id[:])
	var b bytes.Buffer
	header := func(key, value string) { b.WriteString(key + ": " + clean(value) + "\r\n") }
	header("From", sender)
	header("To", message.To)
	header("Subject", mime.BEncoding.Encode("utf-8", message.Subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", "<"+hex.EncodeToString(id[:])+"@"+domainOf(addressOf(from))+">")
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "base64")
	header("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	body := base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(message.Text, "\n", "\r\n")))
	for len(body) > 76 {
		b.WriteString(body[:76] + "\r\n")
		body = body[76:]
	}
	b.WriteString(body + "\r\n")
	return b.Bytes()
}

// SMTP 通过 SMTP 服务器发信。端口 465 用 TLS 直连，其他端口要求 STARTTLS（本机除外）。
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
}

// Check 连接 SMTP 服务器并登录，然后断开，不发任何邮件。用来检查配置。
func (s *SMTP) Check(ctx context.Context) error {
	client, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	return client.Quit()
}

// Send 实现 Transport。
func (s *SMTP) Send(ctx context.Context, from, to string, raw []byte) error {
	client, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(raw); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// dial 连接、加密、登录。
func (s *SMTP) dial(ctx context.Context) (*smtp.Client, error) {
	address := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if s.Port == 465 {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: s.Host}}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if s.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
				client.Close()
				return nil, err
			}
		} else if s.Username != "" && s.Host != "localhost" && s.Host != "127.0.0.1" {
			client.Close()
			return nil, fmt.Errorf("SMTP 服务器 %s 不支持 STARTTLS，拒绝明文发送密码", s.Host)
		}
	}
	if s.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			client.Close()
			return nil, err
		}
	}
	return client, nil
}

// LogTransport 只把邮件写进日志，开发环境没配 SMTP 时用。
type LogTransport struct{ Log *slog.Logger }

// Send 实现 Transport。
func (t LogTransport) Send(_ context.Context, _, to string, raw []byte) error {
	text := string(raw)
	if i := strings.Index(text, "\r\n\r\n"); i >= 0 {
		if decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(text[i+4:], "\r\n", "")); err == nil {
			text = text[:i] + "\n\n" + string(decoded)
		}
	}
	t.Log.Info("开发环境：邮件没有真的发出，内容如下", "to", to, "mail", text)
	return nil
}

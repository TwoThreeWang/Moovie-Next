package mail_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"moovie/internal/db/testdb"
	"moovie/internal/mail"
	"moovie/internal/mail/mailtest"
)

func TestChecker(t *testing.T) {
	ctx := context.Background()
	resolver := mailtest.Default()
	checker := mail.NewChecker(resolver)
	cases := map[string]error{
		"a@example.com":      nil,
		"a@EXAMPLE.org":      nil,
		"a@nomx.test":        mail.ErrUndeliverable, // 域名存在但没有 MX
		"a@nullmx.test":      mail.ErrUndeliverable, // Null MX
		"a@missing.test":     mail.ErrUndeliverable, // 域名不存在
		"a@localhost":        mail.ErrUndeliverable, // 单段域名
		"a@[127.0.0.1]":      mail.ErrUndeliverable,
		"a@timeout.test":     nil, // 超时放行
		"a@例子.测试":            nil, // 中文域名放行
		"no-at-sign.example": mail.ErrUndeliverable,
	}
	for address, want := range cases {
		if err := checker.Check(ctx, address); !errors.Is(err, want) && err != want {
			t.Errorf("%s: 期望 %v，实际 %v", address, want, err)
		}
	}

	// 有结论的结果缓存，超时不缓存
	before := resolver.Calls
	checker.Check(ctx, "b@example.com")
	checker.Check(ctx, "b@missing.test")
	if resolver.Calls != before {
		t.Errorf("有结论的结果应该缓存，多查了 %d 次", resolver.Calls-before)
	}
	checker.Check(ctx, "b@timeout.test")
	if resolver.Calls != before+1 {
		t.Error("超时的结果不应该缓存")
	}
	// 单段域名不查 DNS
	before = resolver.Calls
	checker.Check(ctx, "a@intranet")
	if resolver.Calls != before {
		t.Error("单段域名应该直接拒绝，不查 DNS")
	}
}

func TestValidate(t *testing.T) {
	ctx := context.Background()
	checker := mail.NewChecker(mailtest.Default())
	for _, bad := range []string{"", "bad", "a b@example.com", "Evan <a@example.com>", "a@example.com\r\nBcc: x@example.com"} {
		if err := mail.Validate(ctx, checker, bad); !errors.Is(err, mail.ErrInvalid) {
			t.Errorf("%q 应该是格式错误，实际 %v", bad, err)
		}
	}
	if err := mail.Validate(ctx, checker, "a@nomx.test"); !errors.Is(err, mail.ErrUndeliverable) {
		t.Errorf("收不了信的域名应该被拒绝: %v", err)
	}
}

func TestMailerLimitsAndEncoding(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	recorder := &mailtest.Recorder{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mailer := mail.New(recorder, mail.NewChecker(mailtest.Default()), pool, "影牛 <noreply@example.com>",
		mail.Site{Name: "影牛", URL: "https://moovie.test"}, mail.Limits{Gap: time.Hour, PerDay: 2, PerHour: 3}, logger)

	message := mail.Message{To: "a@example.com", Subject: "你的密码", Title: "欢迎", Text: "密码：<abc>\n第二行",
		Button: mail.Button{Label: "登录", URL: "https://moovie.test/login?a=1&b=2"}, Note: "不是你就忽略", Kind: "register"}
	if err := mailer.Send(ctx, message); err != nil {
		t.Fatal(err)
	}
	got, ok := recorder.Last("a@example.com")
	want := "欢迎\n\n密码：<abc>\n第二行\n\n登录：\nhttps://moovie.test/login?a=1&b=2\n\n不是你就忽略\n"
	if !ok || got.Subject != "你的密码" || got.Text != want {
		t.Errorf("纯文本内容不对: %+v", got)
	}
	for _, part := range []string{"<b style=\"font-size:17px;color:#e2605d\">影牛</b>", ">欢迎</h2>", "<p>密码：&lt;abc&gt;<br>第二行</p>",
		`href="https://moovie.test/login?a=1&amp;b=2"`, ">登录</a>", "<p>不是你就忽略</p>", `href="https://moovie.test" style="color:#999">moovie.test</a>`} {
		if !strings.Contains(got.HTML, part) {
			t.Errorf("HTML 里缺少 %q:\n%s", part, got.HTML)
		}
	}
	if err := mailer.Send(ctx, message); !errors.Is(err, mail.ErrTooSoon) {
		t.Errorf("间隔太短应该被拒绝: %v", err)
	}

	// 收不了信的域名在发信层被跳过，不占用次数
	if err := mailer.Send(ctx, mail.Message{To: "a@nomx.test", Subject: "x", Text: "x"}); !errors.Is(err, mail.ErrUndeliverable) {
		t.Errorf("收不了信的域名应该跳过: %v", err)
	}
	if recorder.Count() != 1 {
		t.Errorf("应该只发出 1 封，实际 %d", recorder.Count())
	}

	// 全站每小时上限
	mailer.Send(ctx, mail.Message{To: "b@example.com", Subject: "x", Text: "x"})
	mailer.Send(ctx, mail.Message{To: "c@example.com", Subject: "x", Text: "x"})
	if err := mailer.Send(ctx, mail.Message{To: "d@example.com", Subject: "x", Text: "x"}); !errors.Is(err, mail.ErrBusy) {
		t.Errorf("超过每小时上限应该被拒绝: %v", err)
	}

	// 记录里不出现邮箱本身
	var plain int
	pool.QueryRow(ctx, `SELECT count(*) FROM mail_log WHERE recipient LIKE '%@%'`).Scan(&plain)
	if plain != 0 {
		t.Error("发信记录不应该保存邮箱原文")
	}

	// 每天上限：换一个间隔为 0 的发信器
	daily := mail.New(recorder, mail.NewChecker(mailtest.Default()), pool, "noreply@example.com", mail.Site{}, mail.Limits{PerDay: 2}, logger)
	daily.Send(ctx, mail.Message{To: "e@example.com", Subject: "x", Text: "x"})
	daily.Send(ctx, mail.Message{To: "e@example.com", Subject: "x", Text: "x"})
	if err := daily.Send(ctx, mail.Message{To: "e@example.com", Subject: "x", Text: "x"}); !errors.Is(err, mail.ErrTooMany) {
		t.Errorf("超过每天上限应该被拒绝: %v", err)
	}

	if err := (*mail.Mailer)(nil).Send(ctx, message); !errors.Is(err, mail.ErrDisabled) {
		t.Errorf("没有配置时应该返回 ErrDisabled: %v", err)
	}
	if strings.Contains(got.Text, "\r") {
		t.Error("正文换行应该能还原")
	}
}

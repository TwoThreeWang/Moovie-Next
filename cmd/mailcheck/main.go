// mailcheck 用来在上线前检查发信：
//
//	go run ./cmd/mailcheck -dial             只连接 SMTP 服务器并登录，不发信：检查地址、端口、账号密码对不对
//	go run ./cmd/mailcheck -to 你的邮箱      按 .env 里的 SMTP 配置发一封测试邮件（走完整的发信流程）
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"moovie/internal/config"
	"moovie/internal/db"
	"moovie/internal/mail"
)

func main() {
	to := flag.String("to", "", "收件人：发一封测试邮件")
	dial := flag.Bool("dial", false, "只连接并登录 SMTP 服务器，不发信")
	flag.Parse()
	if *to == "" && !*dial {
		flag.Usage()
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fail(err)
	}
	ctx := context.Background()
	if *dial {
		if cfg.SMTPHost == "" {
			fail(errors.New("没有配置 SMTP_HOST"))
		}
		transport := &mail.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword}
		start := time.Now()
		if err := transport.Check(ctx); err != nil {
			fail(fmt.Errorf("连接 %s:%d 失败: %w", cfg.SMTPHost, cfg.SMTPPort, err))
		}
		fmt.Printf("SMTP 连接、加密、登录都成功（%s:%d，%s）。发件人：%s\n", cfg.SMTPHost, cfg.SMTPPort, time.Since(start).Round(time.Millisecond), cfg.MailFrom)
		if *to == "" {
			return
		}
	}
	pool, err := db.OpenApp(ctx, cfg.DatabaseURL, cfg.DBSchema, 2)
	if err != nil {
		fail(err)
	}
	defer pool.Close()
	checker := mail.NewChecker(nil)

	if *to != "" {
		if cfg.SMTPHost == "" {
			fail(errors.New("没有配置 SMTP_HOST"))
		}
		if err := mail.Validate(ctx, checker, *to); err != nil {
			fail(err)
		}
		transport := &mail.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword}
		from := cfg.MailFrom
		if from == "" {
			fail(errors.New("请设置 SMTP_FROM，例如 \"影牛 <hello@你的域名>\""))
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		mailer := mail.New(transport, checker, pool, from, mail.Site{Name: cfg.SiteName, URL: cfg.SiteURL}, mail.DefaultLimits, logger)
		start := time.Now()
		err := mailer.Send(ctx, mail.Message{To: *to, Kind: "test", Subject: cfg.SiteName + "：测试邮件", Title: "测试邮件",
			Text: "能看到它，说明 SMTP 配置是对的。请再看一下：\n" +
				"1. 它是不是在收件箱里（而不是垃圾邮件）；\n2. 邮件头里 SPF、DKIM、DMARC 是否都是 pass；\n3. HTML 和纯文本两个版本是否都显示正常。"})
		if err != nil {
			fail(err)
		}
		fmt.Printf("已发送到 %s（%s）。请去收件箱确认，并检查是否进了垃圾邮件。\n", *to, time.Since(start).Round(time.Millisecond))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "失败:", err)
	os.Exit(1)
}

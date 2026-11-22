// Package mailtest 提供测试用的假邮件服务器和假 DNS。
package mailtest

import (
	"context"
	"net"
	"strings"
	"sync"

	"moovie/internal/mail"
)

// Mail 是收到的一封邮件（已解码）。
type Mail struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Recorder 把邮件存在内存里。
type Recorder struct {
	mu   sync.Mutex
	sent []Mail
}

// Send 实现 mail.Transport。
func (r *Recorder) Send(_ context.Context, _, to string, raw []byte) error {
	subject, text, html, err := mail.Decode(raw)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, Mail{To: to, Subject: subject, Text: text, HTML: html})
	return nil
}

// Last 返回发给某个地址的最后一封。
func (r *Recorder) Last(to string) (Mail, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.sent) - 1; i >= 0; i-- {
		if strings.EqualFold(r.sent[i].To, to) {
			return r.sent[i], true
		}
	}
	return Mail{}, false
}

// Count 返回一共发了几封。
func (r *Recorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

// Resolver 是假 DNS：Domains 里有的域名返回对应的 MX 记录；
// 值为 nil 表示域名存在但没有 MX；"timeout" 开头的域名模拟超时；其他域名都不存在。
type Resolver struct {
	Domains map[string][]string

	mu    sync.Mutex
	Calls int
}

// LookupMX 实现 mail.Resolver。
func (r *Resolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	r.mu.Lock()
	r.Calls++
	r.mu.Unlock()
	if strings.HasPrefix(name, "timeout") {
		return nil, &net.DNSError{Err: "i/o timeout", Name: name, IsTimeout: true, IsTemporary: true}
	}
	hosts, ok := r.Domains[name]
	if !ok || hosts == nil {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	records := make([]*net.MX, 0, len(hosts))
	for _, host := range hosts {
		records = append(records, &net.MX{Host: host, Pref: 10})
	}
	return records, nil
}

// Default 返回一个常用的假 DNS：example.com、example.org 能收信。
func Default() *Resolver {
	return &Resolver{Domains: map[string][]string{
		"example.com": {"mx.example.com."},
		"example.org": {"mx.example.org."},
		"nomx.test":   nil,
		"nullmx.test": {"."},
	}}
}

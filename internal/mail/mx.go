package mail

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"
)

// ErrUndeliverable 表示收件域名收不了信：域名不存在、没有 MX 记录、声明了 Null MX，或者是单段域名。
var ErrUndeliverable = errors.New("该邮箱域名无法接收邮件，请检查是否拼写错误")

// Resolver 是查 MX 记录的能力，net.Resolver 实现了它；测试里换成假的。
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

const (
	mxCacheTTL = time.Hour
	mxTimeout  = 3 * time.Second
	mxCacheMax = 10000
)

type mxEntry struct {
	ok      bool
	expires time.Time
}

// Checker 检查一个邮箱的域名能不能收信。
//
// 只认 MX 记录（不按 RFC 5321 退回 A 记录）：正经收信的域名都有 MX，没有的多半是拼错了。
// 查询超时或网络出错时放行：宁可多发一封退信，也不挡住正常用户。
// 有结论的结果缓存 1 小时，临时故障不缓存。
type Checker struct {
	resolver Resolver
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]mxEntry
}

// NewChecker 创建 Checker。resolver 为 nil 时用系统的 DNS。
func NewChecker(resolver Resolver) *Checker {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &Checker{resolver: resolver, now: time.Now, cache: map[string]mxEntry{}}
}

// Check 检查邮箱的域名。收不了信时返回 ErrUndeliverable，其他情况（包括查不出结论）返回 nil。
func (c *Checker) Check(ctx context.Context, address string) error {
	at := strings.LastIndexByte(address, '@')
	if at < 0 {
		return ErrUndeliverable
	}
	domain := strings.TrimSuffix(strings.ToLower(address[at+1:]), ".")
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, "[") {
		return ErrUndeliverable // 单段域名（localhost）和 IP 地址都不收
	}
	for i := 0; i < len(domain); i++ {
		if domain[i] >= 0x80 {
			return nil // 中文域名需要先转成 punycode，标准库做不了，放行
		}
	}
	if ok, cached := c.cached(domain); cached {
		return verdict(ok)
	}
	ctx, cancel := context.WithTimeout(ctx, mxTimeout)
	defer cancel()
	records, err := c.resolver.LookupMX(ctx, domain)
	ok, definite := classify(records, err)
	if !definite {
		return nil
	}
	c.store(domain, ok)
	return verdict(ok)
}

func verdict(ok bool) error {
	if ok {
		return nil
	}
	return ErrUndeliverable
}

// classify 判断查询结果。definite 为 false 表示查不出结论（超时、网络错误），不缓存、放行。
func classify(records []*net.MX, err error) (ok, definite bool) {
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return false, true // 域名不存在，或者存在但没有 MX
		}
		return false, false
	}
	for _, record := range records {
		if host := strings.TrimSuffix(record.Host, "."); host != "" {
			return true, true
		}
	}
	// 没有记录，或者只有 "."（Null MX，RFC 7505：这个域名明确不收信）
	return false, true
}

func (c *Checker) cached(domain string) (ok, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, found := c.cache[domain]
	if !found || c.now().After(entry.expires) {
		return false, false
	}
	return entry.ok, true
}

func (c *Checker) store(domain string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.cache) >= mxCacheMax {
		for key, entry := range c.cache {
			if now.After(entry.expires) {
				delete(c.cache, key)
			}
		}
		if len(c.cache) >= mxCacheMax {
			c.cache = map[string]mxEntry{}
		}
	}
	c.cache[domain] = mxEntry{ok: ok, expires: now.Add(mxCacheTTL)}
}

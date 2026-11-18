package httpx

import (
	"testing"
	"time"
)

func TestLimiterRefillsOverTime(t *testing.T) {
	limiter := NewLimiter(2, time.Minute)
	now := time.Unix(1000, 0)
	limiter.now = func() time.Time { return now }
	if !limiter.Allow("a") || !limiter.Allow("a") {
		t.Fatal("前两次应该放行")
	}
	if limiter.Allow("a") {
		t.Error("第三次应该被拒绝")
	}
	if !limiter.Allow("b") {
		t.Error("不同的 key 互不影响")
	}
	now = now.Add(31 * time.Second) // 半分钟恢复一个令牌
	if !limiter.Allow("a") {
		t.Error("过了半分钟应该恢复一次")
	}
	if limiter.Allow("a") {
		t.Error("只恢复了一次，再来应该被拒绝")
	}
}

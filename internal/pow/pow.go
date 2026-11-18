// Package pow 是登录和注册表单的人机校验：工作量证明（Proof of Work）。
//
// 服务器签发一个挑战，浏览器找一个十进制数 nonce，使 SHA-256(挑战 + ":" + nonce) 的前 bits 位都是 0。
// 正常用户在填表的时候浏览器已经在后台算完了，感觉不到；批量注册、撞库的脚本每提交一次都要付出同样的算力。
//
// 挑战的格式：用途.签发毫秒.难度.随机串.签名。签名密钥在进程启动时随机生成，
// 重启后旧挑战全部失效；用过的挑战记在内存里，不能重复使用。多实例部署时要换成共享密钥和共享存储。
package pow

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 表单字段名。
const (
	TokenField = "pow_token"
	NonceField = "pow_nonce"
)

const (
	// DefaultBits 是默认难度：期望约 100 万次哈希，电脑浏览器约 0.2 秒，中端手机约 1 秒。
	DefaultBits = 20
	// TTL 是挑战的有效期，覆盖用户慢慢填表的时间。
	TTL = 30 * time.Minute
	// MinSolveTime 是签发到提交的最短间隔，更快的视为脚本。
	MinSolveTime = 2 * time.Second
	maxUsed      = 200_000
)

// 校验失败的原因。对用户统一显示"安全校验失败"，区分开只是为了测试。
var (
	ErrMissing   = errors.New("pow: 缺少挑战或答案")
	ErrMalformed = errors.New("pow: 挑战格式不对")
	ErrSignature = errors.New("pow: 签名不对")
	ErrScope     = errors.New("pow: 用途不对")
	ErrExpired   = errors.New("pow: 挑战过期")
	ErrTooFast   = errors.New("pow: 提交太快")
	ErrUnsolved  = errors.New("pow: 答案不对")
	ErrReplayed  = errors.New("pow: 挑战已经用过")
	ErrBusy      = errors.New("pow: 未完成的挑战太多")
)

// Issuer 签发和校验挑战，并发安全。
type Issuer struct {
	secret  []byte
	bits    int
	minWait time.Duration
	now     func() time.Time

	mu   sync.Mutex
	used map[string]time.Time
}

// New 创建签发器。bits 是难度（1–28）；minWait 是签发到提交的最短间隔。
func New(bits int, minWait time.Duration) *Issuer {
	secret := make([]byte, 32)
	rand.Read(secret)
	return &Issuer{secret: secret, bits: min(28, max(1, bits)), minWait: minWait, now: time.Now, used: map[string]time.Time{}}
}

// Bits 返回当前难度。
func (i *Issuer) Bits() int { return i.bits }

func (i *Issuer) sign(payload string) string {
	mac := hmac.New(sha256.New, i.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// Issue 签发一个挑战。scope 区分用途（login、register），一个用途的挑战不能拿到另一个用途去用。
func (i *Issuer) Issue(scope string) string {
	salt := make([]byte, 12)
	rand.Read(salt)
	payload := scope + "." + strconv.FormatInt(i.now().UnixMilli(), 10) + "." + strconv.Itoa(i.bits) + "." + hex.EncodeToString(salt)
	return payload + "." + i.sign(payload)
}

// Challenge 返回挑战里要参与哈希的部分（去掉签名）。
func Challenge(token string) string {
	if index := strings.LastIndexByte(token, '.'); index > 0 {
		return token[:index]
	}
	return token
}

// LeadingZeroBits 返回哈希开头有多少个 0 比特。
func LeadingZeroBits(sum []byte) int {
	count := 0
	for _, b := range sum {
		if b == 0 {
			count += 8
			continue
		}
		for mask := byte(0x80); mask > 0 && b&mask == 0; mask >>= 1 {
			count++
		}
		break
	}
	return count
}

// Solved 判断 nonce 是不是挑战的答案。
func Solved(challenge, nonce string, bits int) bool {
	sum := sha256.Sum256([]byte(challenge + ":" + nonce))
	return LeadingZeroBits(sum[:]) >= bits
}

// Verify 校验一次提交。通过后这个挑战就作废了。
func (i *Issuer) Verify(scope, token, nonce string) error {
	if token == "" || nonce == "" {
		return ErrMissing
	}
	if len(nonce) > 16 || strings.Trim(nonce, "0123456789") != "" {
		return ErrMalformed
	}
	parts := strings.Split(token, ".")
	if len(parts) != 5 {
		return ErrMalformed
	}
	payload := strings.Join(parts[:4], ".")
	if !hmac.Equal([]byte(parts[4]), []byte(i.sign(payload))) {
		return ErrSignature
	}
	if parts[0] != scope {
		return ErrScope
	}
	issuedMs, err1 := strconv.ParseInt(parts[1], 10, 64)
	bits, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || bits < i.bits {
		return ErrMalformed
	}
	age := i.now().Sub(time.UnixMilli(issuedMs))
	if age > TTL {
		return ErrExpired
	}
	if age < i.minWait {
		return ErrTooFast
	}
	if !Solved(payload, nonce, bits) {
		return ErrUnsolved
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	now := i.now()
	if len(i.used) >= maxUsed {
		for key, expires := range i.used {
			if now.After(expires) {
				delete(i.used, key)
			}
		}
		if len(i.used) >= maxUsed {
			return ErrBusy
		}
	}
	if _, used := i.used[parts[3]]; used {
		return ErrReplayed
	}
	i.used[parts[3]] = time.UnixMilli(issuedMs).Add(TTL)
	return nil
}

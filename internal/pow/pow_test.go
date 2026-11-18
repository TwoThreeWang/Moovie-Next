package pow

import (
	"strconv"
	"testing"
	"time"
)

func solve(challenge string, bits int) string {
	for nonce := 0; ; nonce++ {
		if Solved(challenge, strconv.Itoa(nonce), bits) {
			return strconv.Itoa(nonce)
		}
	}
}

func TestLeadingZeroBits(t *testing.T) {
	cases := []struct {
		sum  []byte
		want int
	}{{[]byte{0x80}, 0}, {[]byte{0x40}, 1}, {[]byte{0x01}, 7}, {[]byte{0x00, 0xff}, 8}, {[]byte{0x00, 0x00, 0x10}, 19}, {[]byte{0, 0}, 16}}
	for _, c := range cases {
		if got := LeadingZeroBits(c.sum); got != c.want {
			t.Errorf("LeadingZeroBits(%x) = %d，期望 %d", c.sum, got, c.want)
		}
	}
}

// 这一组数据也写在前端的测试里，保证前后端算法一致。
func TestKnownVector(t *testing.T) {
	if !Solved("register.1700000000000.12.00112233445566778899aabb", "451", 12) {
		t.Error("已知的答案应该通过")
	}
	if Solved("register.1700000000000.12.00112233445566778899aabb", "2798", 12) {
		t.Error("错误的答案不应该通过")
	}
}

func TestIssueAndVerify(t *testing.T) {
	issuer := New(10, 2*time.Second)
	now := time.Now()
	issuer.now = func() time.Time { return now }
	token := issuer.Issue("register")
	nonce := solve(Challenge(token), 10)

	if err := issuer.Verify("register", token, nonce); err != ErrTooFast {
		t.Errorf("签发后马上提交应该算太快，实际 %v", err)
	}
	now = now.Add(3 * time.Second)
	if err := issuer.Verify("login", token, nonce); err != ErrScope {
		t.Errorf("注册的挑战不能用来登录，实际 %v", err)
	}
	if err := issuer.Verify("register", token, "0"); err != ErrUnsolved && nonce != "0" {
		t.Errorf("错误的答案应该被拒绝，实际 %v", err)
	}
	if err := issuer.Verify("register", token, nonce); err != nil {
		t.Fatalf("正确的答案应该通过: %v", err)
	}
	if err := issuer.Verify("register", token, nonce); err != ErrReplayed {
		t.Errorf("同一个挑战不能用两次，实际 %v", err)
	}

	tampered := issuer.Issue("register")
	tampered = tampered[:len(tampered)-1] + "0"
	if err := issuer.Verify("register", tampered, "1"); err != ErrSignature && err != ErrUnsolved {
		t.Errorf("改过的挑战应该被拒绝，实际 %v", err)
	}
	if err := New(10, 0).Verify("register", issuer.Issue("register"), "1"); err != ErrSignature {
		t.Errorf("别的签发器（比如重启前）签的挑战应该被拒绝，实际 %v", err)
	}
	old := issuer.Issue("register")
	now = now.Add(31 * time.Minute)
	if err := issuer.Verify("register", old, solve(Challenge(old), 10)); err != ErrExpired {
		t.Errorf("过期的挑战应该被拒绝，实际 %v", err)
	}
	for _, bad := range []struct{ token, nonce string }{{"", "1"}, {"a.b", ""}, {"a.b.c", "1"}, {issuer.Issue("register"), "-1"}, {issuer.Issue("register"), "12345678901234567"}} {
		if err := issuer.Verify("register", bad.token, bad.nonce); err == nil {
			t.Errorf("格式不对的提交应该被拒绝: %q %q", bad.token, bad.nonce)
		}
	}
	// 难度比现在要求的低的挑战（比如调高难度之前签发的）不接受
	easy := New(4, 0)
	easy.secret = issuer.secret
	easyToken := easy.Issue("register")
	if err := issuer.Verify("register", easyToken, solve(Challenge(easyToken), 4)); err != ErrMalformed {
		t.Errorf("难度不够的挑战应该被拒绝，实际 %v", err)
	}
}

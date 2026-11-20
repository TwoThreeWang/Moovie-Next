package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestPasswordChangeRequiresAndCommitsEncryptedConfiguration(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	salt := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16))
	blob := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 64))
	if r := s.json(http.MethodPut, "/api/keyring", map[string]any{"salt": salt, "blob": blob, "iterations": 310000, "base": 0}); r.status != 200 {
		t.Fatal(r.status, r.body)
	}
	change := func(ring map[string]any) reply {
		values := url.Values{"_csrf": {s.csrf(s.client)}, "current": {"password-123"}, "password": {"new-password-456"}}
		if ring != nil {
			wire, _ := json.Marshal(ring)
			values.Set("keyring", string(wire))
		}
		return s.do(s.client, http.MethodPost, "/settings/password", strings.NewReader(values.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"})
	}
	if r := change(nil); r.status != 400 || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("缺少密文不能先改密码: %d %s", r.status, r.body)
	}
	stale := map[string]any{"salt": salt, "blob": blob, "iterations": 310000, "base": 0}
	if r := change(stale); r.status != 409 {
		t.Fatalf("同步版本冲突应拒绝整笔修改: %d %s", r.status, r.body)
	}
	user, err := s.app.Users.ByEmail(context.Background(), "evan@example.com")
	if err != nil || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte("password-123")) != nil {
		t.Fatal("失败后原密码必须仍然有效", err)
	}
	nextSalt := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 16))
	nextBlob := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{10}, 64))
	next := map[string]any{"salt": nextSalt, "blob": nextBlob, "iterations": 310000, "base": 1}
	r := change(next)
	if r.status != 200 || !strings.Contains(r.body, `"keyring_version":2`) {
		t.Fatalf("修改应返回事务提交确认: %d %s", r.status, r.body)
	}
	user, err = s.app.Users.ByEmail(context.Background(), "evan@example.com")
	if err != nil || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte("new-password-456")) != nil {
		t.Fatal("新密码必须有效", err)
	}
	got := s.get("/api/keyring")
	if got.status != 200 || !strings.Contains(got.body, nextBlob) || !strings.Contains(got.body, nextSalt) || !strings.Contains(got.body, `"version":2`) {
		t.Fatalf("当前设备保持登录，并读到同一笔提交的新密文: %d %s", got.status, got.body)
	}
}

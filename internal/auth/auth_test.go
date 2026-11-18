package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"moovie/internal/auth"
	"moovie/internal/db/testdb"
)

func TestStoreCreateAndLookup(t *testing.T) {
	store := auth.NewStore(testdb.Pool(t))
	ctx := context.Background()
	first, err := store.Create(ctx, " Evan@Example.com ", "evan", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if first.Role != "admin" || first.Email != "evan@example.com" {
		t.Errorf("第一个账号应该是管理员，邮箱应该转成小写: %+v", first)
	}
	second, err := store.Create(ctx, "b@example.com", "bob", "hash")
	if err != nil || second.Role != "user" {
		t.Fatalf("第二个账号应该是普通用户: %+v err=%v", second, err)
	}
	if _, err := store.Create(ctx, "EVAN@example.com", "other", "hash"); !errors.Is(err, auth.ErrTaken) {
		t.Errorf("重复邮箱应该返回 ErrTaken，实际 %v", err)
	}
	if _, err := store.Create(ctx, "c@example.com", "evan", "hash"); !errors.Is(err, auth.ErrTaken) {
		t.Errorf("重复用户名应该返回 ErrTaken，实际 %v", err)
	}
	if found, err := store.ByEmail(ctx, "EVAN@EXAMPLE.COM"); err != nil || found.ID != first.ID {
		t.Errorf("按邮箱查找应该不区分大小写: %v", err)
	}
	if _, err := store.ByID(ctx, 9999); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("不存在的账号应该返回 ErrNotFound，实际 %v", err)
	}
}

func TestStoreUpdateAndPassword(t *testing.T) {
	store := auth.NewStore(testdb.Pool(t))
	ctx := context.Background()
	user, _ := store.Create(ctx, "a@example.com", "alice", "old")
	store.Create(ctx, "b@example.com", "bob", "x")

	if err := store.Update(ctx, user.ID, "username", "bob"); !errors.Is(err, auth.ErrTaken) {
		t.Errorf("改成别人的用户名应该返回 ErrTaken，实际 %v", err)
	}
	if err := store.Update(ctx, user.ID, "password_hash", "x"); err == nil {
		t.Error("不在白名单里的列不应该能通过 Update 修改")
	}
	if err := store.Update(ctx, user.ID, "is_public", true); err != nil {
		t.Fatal(err)
	}
	epoch, err := store.SetPassword(ctx, user.ID, "new")
	if err != nil || epoch != 1 {
		t.Fatalf("改密码应该让密码版本加一: %d %v", epoch, err)
	}
	updated, _ := store.ByID(ctx, user.ID)
	if updated.PasswordHash != "new" || !updated.IsPublic || updated.SessionEpoch != 1 {
		t.Errorf("更新没有生效: %+v", updated)
	}
	users, total, err := store.List(ctx, "ALI", 10, 0)
	if err != nil || total != 1 || len(users) != 1 || users[0].Username != "alice" {
		t.Errorf("搜索应该不区分大小写: %d %v", total, err)
	}
}

func TestSessionTokens(t *testing.T) {
	sessions := auth.NewSessions("secret", time.Hour, nil)
	token := sessions.Token(42, 3)
	userID, epoch, ok := sessions.Parse(token)
	if !ok || userID != 42 || epoch != 3 {
		t.Fatalf("凭证应该能解析回来: %d %d %v", userID, epoch, ok)
	}
	if _, _, ok := sessions.Parse(token + "x"); ok {
		t.Error("被改过的凭证不应该通过校验")
	}
	if _, _, ok := auth.NewSessions("other", time.Hour, nil).Parse(token); ok {
		t.Error("用别的密钥签的凭证不应该通过校验")
	}
	if _, _, ok := auth.NewSessions("secret", -time.Minute, nil).Parse(auth.NewSessions("secret", -time.Minute, nil).Token(1, 0)); ok {
		t.Error("过期的凭证不应该通过校验")
	}
	for _, bad := range []string{"", "abc", "1.2.3", "a.b.c.d"} {
		if _, _, ok := sessions.Parse(bad); ok {
			t.Errorf("格式不对的凭证不应该通过校验: %q", bad)
		}
	}
}

func TestSafeRedirect(t *testing.T) {
	cases := map[string]string{
		"/m/12?x=1":            "/m/12?x=1",
		"":                     "/",
		"https://evil.example": "/",
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"/auth/login":          "/", // 登录后不应该又回到登录页
	}
	for input, want := range cases {
		if got := auth.SafeRedirect(input); got != want {
			t.Errorf("SafeRedirect(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestValidAvatar(t *testing.T) {
	for _, s := range append([]string{"😀", "👍🏽", "👨‍👩‍👧", "🏳️‍🌈", "🇨🇳", "1️⃣", "#️⃣", "❤️", "☕", "🏴󠁧󠁢󠁳󠁣󠁴󠁿", "🫠"}, auth.Avatars...) {
		if !auth.ValidAvatar(s) {
			t.Errorf("%q 应该是合法头像", s)
		}
	}
	for _, s := range []string{"", "a", "中", "1", "😀😀", "😀a", "🇨", "‍😀", "🏽", "<b>", "😀 ", "🇨🇳🇺🇸"} {
		if auth.ValidAvatar(s) {
			t.Errorf("%q 不应该是合法头像", s)
		}
	}
}

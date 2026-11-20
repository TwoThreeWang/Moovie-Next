package auth_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"moovie/internal/auth"
	"moovie/internal/db/testdb"
)

func TestPasswordAndKeyringCommitTogether(t *testing.T) {
	pool := testdb.Pool(t)
	store := auth.NewStore(pool)
	ctx := context.Background()
	user, err := store.Create(ctx, "atomic@example.com", "atomic", "old")
	if err != nil {
		t.Fatal(err)
	}
	ring := auth.PasswordKeyring{Salt: bytes.Repeat([]byte{1}, 16), Blob: bytes.Repeat([]byte{2}, 64), Iterations: 310000}
	epoch, version, err := store.SetPasswordAndKeyring(ctx, user.ID, user.SessionEpoch, "new", &ring)
	if err != nil || epoch != user.SessionEpoch+1 || version != 1 {
		t.Fatalf("首次提交失败: %d %d %v", epoch, version, err)
	}
	ring.Base = 1
	ring.Blob = bytes.Repeat([]byte{3}, 64)
	if _, _, err = store.SetPasswordAndKeyring(ctx, user.ID, user.SessionEpoch, "stale-session", &ring); !errors.Is(err, auth.ErrPasswordConflict) {
		t.Fatalf("旧登录版本不能改密码: %v", err)
	}
	ring.Base = 0
	if _, _, err = store.SetPasswordAndKeyring(ctx, user.ID, epoch, "stale-ring", &ring); !errors.Is(err, auth.ErrPasswordConflict) {
		t.Fatalf("旧同步版本不能改密码: %v", err)
	}
	if _, _, err = store.SetPasswordAndKeyring(ctx, user.ID, epoch, "missing", nil); !errors.Is(err, auth.ErrKeyringRequired) {
		t.Fatalf("不能只改密码不更新已有密文: %v", err)
	}
	// 模拟密文写入之后密码更新失败，检查整笔事务确实回滚。
	if _, err = pool.Exec(ctx, `ALTER TABLE users ADD CONSTRAINT reject_test_password CHECK(password_hash <> 'rejected')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `ALTER TABLE users DROP CONSTRAINT reject_test_password`); err != nil {
			t.Error(err)
		}
	})
	ring.Base = 1
	if _, _, err = store.SetPasswordAndKeyring(ctx, user.ID, epoch, "rejected", &ring); err == nil {
		t.Fatal("模拟数据库错误应失败")
	}
	found, err := store.ByID(ctx, user.ID)
	if err != nil || found.PasswordHash != "new" || found.SessionEpoch != epoch {
		t.Fatalf("失败不能改变密码或登录版本: %+v %v", found, err)
	}
	var stored []byte
	var storedVersion int64
	if err = pool.QueryRow(ctx, `SELECT blob,version FROM keyrings WHERE user_id=$1`, user.ID).Scan(&stored, &storedVersion); err != nil {
		t.Fatal(err)
	}
	if storedVersion != 1 || !bytes.Equal(stored, bytes.Repeat([]byte{2}, 64)) {
		t.Fatal("密码写入失败时新密文必须一起回滚")
	}
	nextEpoch, nextVersion, err := store.SetPasswordAndKeyring(ctx, user.ID, epoch, "newer", &ring)
	if err != nil || nextEpoch != epoch+1 || nextVersion != 2 {
		t.Fatalf("重新提交应成功: %d %d %v", nextEpoch, nextVersion, err)
	}
}

func TestConcurrentPasswordChangesOnlyCommitOnePair(t *testing.T) {
	pool := testdb.Pool(t)
	store := auth.NewStore(pool)
	ctx := context.Background()
	user, err := store.Create(ctx, "parallel@example.com", "parallel", "old")
	if err != nil {
		t.Fatal(err)
	}
	ring := auth.PasswordKeyring{Salt: bytes.Repeat([]byte{1}, 16), Blob: bytes.Repeat([]byte{1}, 64), Iterations: 310000}
	epoch, _, err := store.SetPasswordAndKeyring(ctx, user.ID, user.SessionEpoch, "first", &ring)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, item := range []struct {
		hash  string
		value byte
	}{{"winner-two", 2}, {"winner-three", 3}} {
		go func(hash string, value byte) {
			<-start
			k := ring
			k.Base = 1
			k.Blob = bytes.Repeat([]byte{value}, 64)
			_, _, err := store.SetPasswordAndKeyring(ctx, user.ID, epoch, hash, &k)
			results <- err
		}(item.hash, item.value)
	}
	close(start)
	wins := 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else if !errors.Is(err, auth.ErrPasswordConflict) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("只能完整提交一组密码和密文，实际 %d", wins)
	}
	found, err := store.ByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var blob []byte
	var version int64
	if err = pool.QueryRow(ctx, `SELECT blob,version FROM keyrings WHERE user_id=$1`, user.ID).Scan(&blob, &version); err != nil {
		t.Fatal(err)
	}
	expected := byte(2)
	if found.PasswordHash == "winner-three" {
		expected = 3
	}
	if version != 2 || found.SessionEpoch != epoch+1 || !bytes.Equal(blob, bytes.Repeat([]byte{expected}, 64)) {
		t.Fatal("不能出现一个请求的密码搭配另一个请求的密文")
	}
}

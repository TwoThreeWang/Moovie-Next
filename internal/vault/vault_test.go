package vault_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"moovie/internal/db/testdb"
	"moovie/internal/vault"
)

func rid(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestPutDetectsConflicts(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1, 2)
	store := vault.NewStore(pool)
	ctx := context.Background()

	v1, _, err := store.Put(ctx, 1, rid(1), []byte("cipher-1"), 0)
	if err != nil || v1 == 0 {
		t.Fatalf("新记录应该写入: %d %v", v1, err)
	}
	// 另一台设备不知道记录已存在，也按新记录提交：冲突，并拿到当前内容。
	_, current, err := store.Put(ctx, 1, rid(1), []byte("cipher-other"), 0)
	if !errors.Is(err, vault.ErrConflict) || current == nil || string(current.Blob) != "cipher-1" || current.Version != v1 {
		t.Fatalf("应该冲突并返回当前记录: %+v err=%v", current, err)
	}
	v2, _, err := store.Put(ctx, 1, rid(1), []byte("cipher-2"), v1)
	if err != nil || v2 <= v1 {
		t.Fatalf("带着正确的版本应该能更新: %d %v", v2, err)
	}
	// 删除：blob 为空。
	v3, _, err := store.Put(ctx, 1, rid(1), nil, v2)
	if err != nil {
		t.Fatal(err)
	}
	records, version, err := store.Since(ctx, 1, 0, 100)
	if err != nil || len(records) != 1 || records[0].Blob != nil || version != v3 {
		t.Fatalf("同步时应该看到一条已删除的记录: %+v version=%d err=%v", records, version, err)
	}
	// 用户之间互相隔离。
	if records, _, _ := store.Since(ctx, 2, 0, 100); len(records) != 0 {
		t.Error("别的用户不应该看到这条记录")
	}
	if _, _, err := store.Put(ctx, 2, rid(1), []byte("x"), 0); err != nil {
		t.Errorf("不同用户可以用相同的记录 ID: %v", err)
	}
}

func TestPutLimits(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	store := vault.NewStore(pool)
	ctx := context.Background()
	if _, _, err := store.Put(ctx, 1, []byte("short"), []byte("x"), 0); err == nil {
		t.Error("记录 ID 不是 32 字节应该被拒绝")
	}
	if _, _, err := store.Put(ctx, 1, rid(1), make([]byte, vault.MaxBlobBytes+1), 0); err == nil {
		t.Error("超长的密文应该被拒绝")
	}
	// 直接把记录数填到上限，再写一条新的应该被拒绝，但更新已有的不受影响。
	if _, err := pool.Exec(ctx, `INSERT INTO private_history (user_id, rid, blob)
SELECT 1, sha256(g::text::bytea), 'x'::bytea FROM generate_series(1, $1) g`, vault.MaxRecords); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Put(ctx, 1, rid(9), []byte("x"), 0); !errors.Is(err, vault.ErrFull) {
		t.Errorf("达到上限后应该返回 ErrFull，实际 %v", err)
	}
}

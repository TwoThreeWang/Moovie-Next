package library_test

import (
	"context"
	"errors"
	"testing"

	"moovie/internal/db/testdb"
	"moovie/internal/library"
)

func TestSaveListAndRemove(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1, 2)
	testdb.Media(t, pool, 10, 11, 12)
	store := library.NewStore(pool)
	ctx := context.Background()

	if _, err := store.Save(ctx, 1, 10, library.StatusWish, 5, "想看时不该有评分和短评"); err != nil {
		t.Fatal(err)
	}
	entry, found, err := store.Get(ctx, 1, 10)
	if err != nil || !found || entry.Rating != 0 || entry.Comment != "" {
		t.Fatalf("想看应该清掉评分和短评: %+v %v %v", entry, found, err)
	}
	// 同一部作品再保存是更新，不是新增。
	if _, err := store.Save(ctx, 1, 10, library.StatusWatched, 4, "  不错  "); err != nil {
		t.Fatal(err)
	}
	entry, _, _ = store.Get(ctx, 1, 10)
	if entry.Status != library.StatusWatched || entry.Rating != 4 || entry.Comment != "不错" {
		t.Errorf("更新没有生效: %+v", entry)
	}
	store.Save(ctx, 1, 11, library.StatusWish, 0, "")
	store.Save(ctx, 2, 12, library.StatusWatched, 5, "")

	wish, watched, err := store.Counts(ctx, 1)
	if err != nil || wish != 1 || watched != 1 {
		t.Errorf("数量不对: wish=%d watched=%d err=%v", wish, watched, err)
	}
	items, err := store.List(ctx, 1, library.StatusWatched, 10, 0)
	if err != nil || len(items) != 1 || items[0].Card.ID != 10 || items[0].Card.Title != "作品10" {
		t.Errorf("看过列表不对: %+v err=%v", items, err)
	}
	statuses, err := store.StatusMap(ctx, 1, []int64{10, 11, 12})
	if err != nil || statuses[10] != "watched" || statuses[11] != "wish" || statuses[12] != "" {
		t.Errorf("状态表不对: %v err=%v", statuses, err)
	}
	if err := store.Remove(ctx, 1, 10); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.Get(ctx, 1, 10); found {
		t.Error("删除后不应该还能读到")
	}
}

func TestSaveRejectsInvalidInput(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	testdb.Media(t, pool, 10)
	store := library.NewStore(pool)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"未知状态": func() error { _, err := store.Save(ctx, 1, 10, "liked", 0, ""); return err },
		"评分太大": func() error { _, err := store.Save(ctx, 1, 10, library.StatusWatched, 6, ""); return err },
		"评分为负": func() error { _, err := store.Save(ctx, 1, 10, library.StatusWatched, -1, ""); return err },
	} {
		if err := call(); !errors.Is(err, library.ErrInvalid) {
			t.Errorf("%s：应该返回 ErrInvalid，实际 %v", name, err)
		}
	}
}

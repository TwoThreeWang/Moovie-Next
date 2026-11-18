package collections_test

import (
	"context"
	"errors"
	"testing"

	"moovie/internal/collections"
	"moovie/internal/db/testdb"
)

func TestSaveReplacesItemsAndRelations(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	testdb.Media(t, pool, 10, 11, 12)
	store := collections.NewStore(pool)
	ctx := context.Background()

	other, err := store.Save(ctx, collections.Draft{OwnerID: 1, Slug: "other", Title: "另一个片单", MediaIDs: []int64{10}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, collections.Draft{OwnerID: 1, Slug: "unrelated", Title: "不相关的片单", MediaIDs: []int64{11}}); err != nil {
		t.Fatal(err)
	}
	id, err := store.Save(ctx, collections.Draft{
		OwnerID: 1, Slug: "best-2026", Title: "2026 最佳", Featured: true,
		MediaIDs: []int64{12, 10, 99999}, Notes: map[int64]string{12: "年度第一"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 相关片单按共同收录的作品实时算：和 other 都收了作品 10；unrelated 没有共同作品，也不是精选，不出现。
	if related, err := store.RelatedTo(ctx, id); err != nil || len(related) != 1 || related[0].ID != other {
		t.Fatalf("相关片单应该是有共同作品的那个: %+v err=%v", related, err)
	}
	items, err := store.Items(ctx, id)
	if err != nil || len(items) != 2 || items[0].Card.ID != 12 || items[0].Note != "年度第一" || items[1].Card.ID != 10 {
		t.Fatalf("作品应该按提交顺序保存，不存在的作品被跳过: %+v err=%v", items, err)
	}
	// 再保存一次：整体替换。
	if _, err := store.Save(ctx, collections.Draft{ID: id, OwnerID: 1, Slug: "best-2026", Title: "2026 最佳", MediaIDs: []int64{11}}); err != nil {
		t.Fatal(err)
	}
	items, _ = store.Items(ctx, id)
	related, _ := store.RelatedTo(ctx, id)
	if len(items) != 1 || items[0].Card.ID != 11 || len(related) != 1 || related[0].Slug != "unrelated" {
		t.Errorf("再次保存应该整体替换作品，相关片单跟着变: %+v %+v", items, related)
	}
	found, err := store.BySlug(ctx, "best-2026")
	if err != nil || found.Count != 1 || found.Featured {
		t.Errorf("片单信息不对: %+v err=%v", found, err)
	}
	list, _ := store.List(ctx, 10, 0)
	if len(list) != 3 {
		t.Errorf("应该有 3 个片单，实际 %d", len(list))
	}
}

func TestSaveValidation(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	store := collections.NewStore(pool)
	ctx := context.Background()
	store.Save(ctx, collections.Draft{OwnerID: 1, Slug: "taken", Title: "已有"})
	for name, draft := range map[string]collections.Draft{
		"地址有大写": {OwnerID: 1, Slug: "Best", Title: "x"},
		"标题为空":  {OwnerID: 1, Slug: "ok-slug", Title: "  "},
		"地址重复":  {OwnerID: 1, Slug: "taken", Title: "x"},
	} {
		if _, err := store.Save(ctx, draft); !errors.Is(err, collections.ErrInvalid) {
			t.Errorf("%s：应该返回 ErrInvalid，实际 %v", name, err)
		}
	}
	if _, err := store.Save(ctx, collections.Draft{ID: 9999, OwnerID: 1, Slug: "ghost", Title: "x"}); !errors.Is(err, collections.ErrNotFound) {
		t.Errorf("更新不存在的片单应该返回 ErrNotFound，实际 %v", err)
	}
	if _, err := store.BySlug(ctx, "nope"); !errors.Is(err, collections.ErrNotFound) {
		t.Errorf("不存在的片单应该返回 ErrNotFound，实际 %v", err)
	}
}

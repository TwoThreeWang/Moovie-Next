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
	publish := func(d collections.Draft) collections.Collection {
		t.Helper()
		d.Submit = true
		id, err := store.Save(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := store.ByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Review(ctx, id, 1, pending.UpdatedAt, "approve", ""); err != nil {
			t.Fatal(err)
		}
		c, err := store.ByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	other := publish(collections.Draft{OwnerID: 1, Slug: "other", Title: "另一个片单", MediaIDs: []int64{10}})
	publish(collections.Draft{OwnerID: 1, Slug: "unrelated", Title: "不相关的片单", MediaIDs: []int64{11}})
	c := publish(collections.Draft{
		OwnerID: 1, Slug: "best-2026", Title: "2026 最佳",
		MediaIDs: []int64{12, 10}, Notes: map[int64]string{12: "年度第一"},
	})
	if related, err := store.RelatedTo(ctx, c.ID); err != nil || len(related) != 1 || related[0].ID != other.ID {
		t.Fatalf("相关片单应该是有共同作品的那个: %+v err=%v", related, err)
	}
	_, items, err := store.VisibleDetail(ctx, c.Slug)
	if err != nil || len(items) != 2 || items[0].Card.ID != 12 || items[0].Note != "年度第一" {
		t.Fatalf("作品应该按提交顺序保存: %+v err=%v", items, err)
	}
	updated := publish(collections.Draft{ID: c.ID, OwnerID: 1, Base: c.UpdatedAt, Title: "2026 最佳", MediaIDs: []int64{11}})
	found, items, err := store.VisibleDetail(ctx, c.Slug)
	related, _ := store.RelatedTo(ctx, c.ID)
	if err != nil || len(items) != 1 || items[0].Card.ID != 11 || len(related) != 1 || related[0].Slug != "unrelated" {
		t.Fatalf("再次保存应该整体替换作品，相关片单跟着变: %+v %+v %v", items, related, err)
	}
	if updated.ID != c.ID || found.Count != 1 || found.Slug != c.Slug {
		t.Errorf("原地址应该保持可用: %+v", found)
	}
}

func TestSaveValidation(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	testdb.Media(t, pool, 10)
	store := collections.NewStore(pool)
	ctx := context.Background()
	store.Save(ctx, collections.Draft{OwnerID: 1, Slug: "taken", Title: "已有", MediaIDs: []int64{10}})
	for name, draft := range map[string]collections.Draft{
		"地址有大写": {OwnerID: 1, Slug: "Best", Title: "x", MediaIDs: []int64{10}},
		"标题为空":  {OwnerID: 1, Slug: "ok-slug", Title: "  ", MediaIDs: []int64{10}},
		"地址重复":  {OwnerID: 1, Slug: "taken", Title: "x", MediaIDs: []int64{10}},
	} {
		if _, err := store.Save(ctx, draft); !errors.Is(err, collections.ErrInvalid) {
			t.Errorf("%s：应该返回 ErrInvalid，实际 %v", name, err)
		}
	}
	if _, err := store.Save(ctx, collections.Draft{ID: 9999, OwnerID: 1, Slug: "ghost", Title: "x", MediaIDs: []int64{10}}); !errors.Is(err, collections.ErrConflict) {
		t.Errorf("更新不存在的片单应该返回 ErrConflict，实际 %v", err)
	}
	if _, _, err := store.VisibleDetail(ctx, "nope"); !errors.Is(err, collections.ErrNotFound) {
		t.Errorf("不存在的片单应该返回 ErrNotFound，实际 %v", err)
	}
}

func TestCollectionCoverMetadata(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	testdb.Media(t, pool, 10, 11)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE media SET poster = 'https://img1.doubanio.com/' || id || '.jpg'`); err != nil {
		t.Fatal(err)
	}
	store := collections.NewStore(pool)
	id, err := store.Save(ctx, collections.Draft{OwnerID: 1, Slug: "cover-metadata", Title: "片单", MediaIDs: []int64{11, 10}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.ByID(ctx, id)
	if err != nil || len(got.Covers) != 2 {
		t.Fatalf("读取组合封面失败: %+v %v", got, err)
	}
	if got.Covers[0].Title != "作品11" || got.Covers[0].URL != "https://img1.doubanio.com/11.jpg" || got.Covers[1].Title != "作品10" {
		t.Fatalf("封面必须保留编排顺序和每张图的片名: %+v", got.Covers)
	}
}

func TestRevisionKeepsPublishedVersion(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	testdb.User(t, pool, 2)
	testdb.Media(t, pool, 10, 11)
	store := collections.NewStore(pool)
	ctx := context.Background()
	id, err := store.Save(ctx, collections.Draft{OwnerID: 2, Slug: "rev", Title: "原版", MediaIDs: []int64{10}, Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	reload := func() collections.Collection {
		t.Helper()
		c, err := store.ByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if err := store.Review(ctx, id, 1, reload().UpdatedAt, "approve", ""); err != nil {
		t.Fatal(err)
	}
	submit := func(title string) {
		t.Helper()
		_, err := store.Save(ctx, collections.Draft{ID: id, OwnerID: 2, Base: reload().UpdatedAt, Title: title,
			MediaIDs: []int64{11, 10}, Notes: map[int64]string{11: "新加的"}, Submit: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	live := func() (collections.Collection, []collections.Item) {
		t.Helper()
		c, items, err := store.VisibleDetail(ctx, "rev")
		if err != nil {
			t.Fatal(err)
		}
		return c, items
	}

	submit("修改版")
	if c, items := live(); c.Title != "原版" || len(items) != 1 || c.Revision != collections.RevisionPending {
		t.Fatalf("修改审核期间应显示原版: %+v %+v", c, items)
	}
	if c, items, err := store.OwnerDetail(ctx, id, 2); err != nil || c.Title != "修改版" || len(items) != 2 || items[0].Note != "新加的" {
		t.Fatalf("作者应看到修改稿: %+v %+v %v", c, items, err)
	}
	if err := store.DiscardRevision(ctx, id, 2, reload().UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if c, _ := live(); c.Revision != "" || c.Title != "原版" {
		t.Fatalf("放弃修改后应只剩原版: %+v", c)
	}

	submit("修改版")
	if err := store.Review(ctx, id, 1, reload().UpdatedAt, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if c, items := live(); c.Title != "修改版" || len(items) != 2 || items[0].Card.ID != 11 || c.Revision != "" {
		t.Fatalf("通过后应换成修改版: %+v %+v", c, items)
	}

	// 撤下时合并修改稿，私密片单保留作者最新的修改。
	submit("再改一版")
	if err := store.Withdraw(ctx, id, 2, reload().UpdatedAt, false); err != nil {
		t.Fatal(err)
	}
	if c := reload(); c.Status != collections.DraftStatus || c.Title != "再改一版" || c.Revision != "" {
		t.Fatalf("撤下后应保留最新修改: %+v", c)
	}
}

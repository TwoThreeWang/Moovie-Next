package ads_test

import (
	"context"
	"errors"
	"testing"

	"moovie/internal/ads"
	"moovie/internal/db/testdb"
)

func TestNormalize(t *testing.T) {
	ok := func(change func(*ads.Ad)) ads.Ad {
		ad := ads.Ad{Slot: ads.Sidebar, Enabled: true, Kind: "image", ImageURL: "https://ads.example.com/a.jpg"}
		change(&ad)
		return ad
	}
	valid := map[string]ads.Ad{
		"https 图片":  ok(func(*ads.Ad) {}),
		"本站路径的图片":   ok(func(a *ads.Ad) { a.ImageURL = "/static/img/ad.png" }),
		"http 点击链接": ok(func(a *ads.Ad) { a.Link = "http://shop.example.com/x?a=1" }),
		"文字广告":      ok(func(a *ads.Ad) { a.Kind, a.ImageURL, a.Text = "text", "", "看看这个" }),
		"关着的空广告":    ok(func(a *ads.Ad) { a.Enabled, a.ImageURL = false, "" }),
		"60 个字":     ok(func(a *ads.Ad) { a.Text = string(make([]rune, 0)) + repeat("字", 60) }),
		"排序 999":    ok(func(a *ads.Ad) { a.Position = 999 }),
	}
	for name, ad := range valid {
		if err := ads.Normalize(&ad); err != nil {
			t.Errorf("%s 应该通过: %v", name, err)
		}
	}
	invalid := map[string]ads.Ad{
		"http 图片会被页面的安全策略拦下": ok(func(a *ads.Ad) { a.ImageURL = "http://ads.example.com/a.jpg" }),
		"javascript 链接":      ok(func(a *ads.Ad) { a.Link = "javascript:alert(1)" }),
		"data 图片":            ok(func(a *ads.Ad) { a.ImageURL = "data:image/png;base64,xx" }),
		"// 开头的地址":           ok(func(a *ads.Ad) { a.Link = "//evil.example.com" }),
		"带账号密码的地址":           ok(func(a *ads.Ad) { a.Link = "https://user:pass@example.com" }),
		"开着的图片广告没有图":         ok(func(a *ads.Ad) { a.ImageURL = "" }),
		"开着的文字广告没有字":         ok(func(a *ads.Ad) { a.Kind, a.Text = "text", "   " }),
		"61 个字":              ok(func(a *ads.Ad) { a.Text = repeat("字", 61) }),
		"未知类型":               ok(func(a *ads.Ad) { a.Kind = "video" }),
		"未知广告位":              ok(func(a *ads.Ad) { a.Slot = "header" }),
		"负的排序":               ok(func(a *ads.Ad) { a.Position = -1 }),
	}
	for name, ad := range invalid {
		err := ads.Normalize(&ad)
		var input *ads.InputError
		if !errors.As(err, &input) {
			t.Errorf("%s 应该被拒绝，并且是可以给管理员看的错误: %v", name, err)
		}
	}
	ad := ok(func(a *ads.Ad) { a.Text, a.Link = "  一段\n  文字  ", "  https://x.example.com  " })
	ads.Normalize(&ad)
	if ad.Text != "一段 文字" || ad.Link != "https://x.example.com" {
		t.Errorf("应该去掉多余的空白: %q %q", ad.Text, ad.Link)
	}
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}

// 多条广告按排序号往下排；关着的、没内容的不显示；改动在本进程立刻生效；一个广告位最多 10 条。
func TestStore(t *testing.T) {
	store := ads.NewStore(testdb.Pool(t))
	ctx := context.Background()
	if shown := store.Current(ctx, ads.Sidebar); len(shown) != 0 {
		t.Fatalf("还没有广告: %v", shown)
	}
	second, err := store.Create(ctx, ads.Ad{Slot: ads.Sidebar, Position: 5, Enabled: true, Kind: "text", Text: "第二条"})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := store.Create(ctx, ads.Ad{Slot: ads.Sidebar, Position: 1, Enabled: true, Kind: "image", ImageURL: "https://a.example.com/1.png"})
	off, _ := store.Create(ctx, ads.Ad{Slot: ads.Sidebar, Position: 0, Enabled: false, Kind: "text", Text: "关着的"})
	shown := store.Current(ctx, ads.Sidebar)
	if len(shown) != 2 || shown[0].ID != first || shown[1].ID != second {
		t.Fatalf("应该按排序号显示开着的两条: %+v", shown)
	}
	if err := store.Update(ctx, ads.Ad{ID: off, Slot: ads.Sidebar, Position: 0, Enabled: true, Kind: "text", Text: "打开了"}); err != nil {
		t.Fatal(err)
	}
	if shown := store.Current(ctx, ads.Sidebar); len(shown) != 3 || shown[0].Text != "打开了" {
		t.Errorf("修改应该立刻生效: %+v", shown)
	}
	if err := store.Delete(ctx, ads.Sidebar, second); err != nil {
		t.Fatal(err)
	}
	if shown := store.Current(ctx, ads.Sidebar); len(shown) != 2 {
		t.Errorf("删除应该立刻生效: %+v", shown)
	}
	if err := store.Delete(ctx, ads.Sidebar, second); !errors.Is(err, ads.ErrNotFound) {
		t.Errorf("删除不存在的广告应该返回 ErrNotFound: %v", err)
	}
	if err := store.Update(ctx, ads.Ad{ID: 9999, Slot: ads.Sidebar, Kind: "text"}); !errors.Is(err, ads.ErrNotFound) {
		t.Errorf("修改不存在的广告应该返回 ErrNotFound: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := store.Create(ctx, ads.Ad{Slot: ads.Sidebar, Kind: "text", Text: "填满"}); err != nil {
			t.Fatal(err)
		}
	}
	var input *ads.InputError
	if _, err := store.Create(ctx, ads.Ad{Slot: ads.Sidebar, Kind: "text", Text: "第 11 条"}); !errors.As(err, &input) {
		t.Errorf("一个广告位最多 10 条: %v", err)
	}
}

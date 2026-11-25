package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"moovie/internal/collections"
	"moovie/internal/media"
)

func (s *site) seedPublishedCollection(slug, title string, ids []int64, notes map[int64]string) int64 {
	s.t.Helper()
	ctx := context.Background()
	id, err := s.app.Collections.Save(ctx, collections.Draft{OwnerID: 1, Slug: slug, Title: title,
		MediaIDs: ids, Notes: notes, Submit: true})
	if err != nil {
		s.t.Fatal(err)
	}
	list, err := s.app.Collections.ByID(ctx, id)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := s.app.Collections.Review(ctx, id, 1, list.UpdatedAt, "approve", ""); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.app.Pool.Exec(ctx, "UPDATE collections SET legacy_editorial = true WHERE id = $1", id); err != nil {
		s.t.Fatal(err)
	}
	return id
}

func TestCollectionSingleLinkLifecycle(t *testing.T) {
	s := newSite(t)
	s.register("editor")
	admin := s.client
	s.client = s.browser()
	s.register("author")
	author := s.client
	visitor := s.browser()
	ctx := context.Background()
	one := s.seedMedia("36000031", "雨夜谜案", media.TypeMovie, nil)
	two := s.seedMedia("36000032", "冬日来信", media.TypeTV, nil)
	mustContain(t, s.get("/me/collections/new"), "保存片单", "投稿到广场")
	mustContain(t, s.get("/me/collections/search?q="+url.QueryEscape("雨夜")), "雨夜谜案", "\"id\":\""+itoa(one)+"\"")

	post := func(path string, fields url.Values, jsonResponse bool) reply {
		headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": s.csrf(s.client)}
		if jsonResponse {
			headers["Accept"] = "application/json"
		}
		return s.do(s.client, http.MethodPost, path, strings.NewReader(fields.Encode()), headers)
	}
	status := func(r reply, want int) {
		t.Helper()
		if r.status != want {
			t.Fatalf("HTTP %d, want %d: %.700s", r.status, want, r.body)
		}
	}
	entries, _ := json.Marshal([]map[string]string{
		{"id": itoa(two), "note": "慢慢看，结尾值得等。"},
		{"id": itoa(one), "note": "留意第一场雨。"},
	})
	fields := url.Values{"id": {"0"}, "title": {"雨天片单"}, "entries": {string(entries)}, "intent": {"save"}}
	preview := post("/me/collections/preview", fields, false)
	mustContain(t, preview, "编辑预览", "雨天片单", "慢慢看，结尾值得等。", `name="robots" content="noindex,follow"`)
	if preview.header.Get("Cache-Control") != "no-store" || strings.Contains(s.get("/list").body, "雨天片单") {
		t.Fatal("预览应禁止缓存，并保持私密")
	}
	status(post("/me/collections", fields, true), http.StatusOK)
	lists, err := s.app.Collections.Owned(ctx, 2, 10, 0)
	if err != nil || len(lists) != 1 {
		t.Fatalf("查询新片单失败: %+v %v", lists, err)
	}
	list := lists[0]
	path, id := "/list/"+list.Slug, itoa(list.ID)
	profile := "/user/2"
	mine := "/me/collections/" + id
	manage := "/admin/collections/" + id
	visit := func(path string) reply { return s.do(visitor, http.MethodGet, path, nil, nil) }
	current := func() collections.Collection {
		t.Helper()
		c, err := s.app.Collections.ByID(ctx, list.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	changeVisibility := func(intent string) reply {
		return s.form(mine+"/visibility", url.Values{"intent": {intent}, "base": {current().UpdatedAt.Format(time.RFC3339Nano)}})
	}
	decision := func(intent, reason string) reply {
		return s.form(manage, url.Values{"intent": {intent}, "reason": {reason}, "base": {current().UpdatedAt.Format(time.RFC3339Nano)}})
	}
	status(visit(path), http.StatusNotFound)
	status(s.get("/admin/collections"), http.StatusForbidden)
	s.client = admin
	status(s.get(mine), http.StatusNotFound)
	status(s.get(manage), http.StatusNotFound)
	s.client = author

	status(changeVisibility("share"), http.StatusSeeOther)
	shared := visit(path)
	mustContain(t, shared, "仅链接可见", "慢慢看，结尾值得等。", `name="robots" content="noindex,follow"`)
	mustContain(t, s.get("/settings/export.json"), `"collections"`, "雨天片单", "慢慢看，结尾值得等。")
	if !strings.Contains(shared.header.Get("Cache-Control"), "no-store") || strings.Contains(visit("/list").body, "雨天片单") ||
		strings.Contains(visit("/sitemap-collections-1.xml").body, path) {
		t.Fatal("仅链接可见的片单不应被缓存或在公开入口出现")
	}
	status(visit(profile), http.StatusForbidden)
	status(changeVisibility("unshare"), http.StatusSeeOther)
	status(visit(path), http.StatusNotFound)
	status(changeVisibility("share"), http.StatusSeeOther)
	status(visit(path), http.StatusOK)

	fields.Set("id", id)
	fields.Set("base", current().UpdatedAt.Format(time.RFC3339Nano))
	fields.Set("intent", "submit")
	status(post("/me/collections", fields, true), http.StatusOK)
	status(visit(path), http.StatusNotFound)
	status(decision("approve", ""), http.StatusForbidden)
	s.client = admin
	mustContain(t, s.get("/admin/collections"), "雨天片单")
	mustContain(t, s.get(manage), "通过并发布", "慢慢看，结尾值得等。")
	status(decision("approve", ""), http.StatusSeeOther)
	public := visit(path)
	mustContain(t, public, "雨天片单", "慢慢看，结尾值得等。", "留意第一场雨。", "查看作者主页", `href="/user/2"`,
		`rel="canonical" href="http://moovie.test`+path+`"`, `property="og:image"`,
		`"@type":"CollectionPage"`, `"@type":"ItemList"`, `"@type":"BreadcrumbList"`, `"datePublished"`,
		`"author":{"@type":"Person","name":"author"`)
	if strings.Contains(public.body, `name="robots"`) || strings.Contains(shared.body, `"@type":"CollectionPage"`) {
		t.Fatal("只有公开且有作品的片单详情可以收录")
	}
	if strings.Index(public.body, "冬日来信") >= strings.Index(public.body, "雨夜谜案") {
		t.Fatal("公开页应保留片单编排顺序")
	}
	mustContain(t, visit(profile), "author", "已发布的片单", "雨天片单")
	if strings.Contains(visit(profile).body, "me-stats") {
		t.Fatal("片单公开不能公开私密观影记录")
	}
	mustContain(t, visit("/sitemap-collections-1.xml"), path)
	s.client = author
	mustContain(t, s.get("/notifications"), "通过了你的片单投稿")
	mustContain(t, s.get("/me/collections"), "撤下并保留链接", "撤下并设为私密")
	status(changeVisibility("unshare"), http.StatusConflict)

	s.client = s.browser()
	s.register("reader")
	reader := s.client
	for range 2 {
		status(s.form(mine+"/favorite", url.Values{"intent": {"save"}}), http.StatusSeeOther)
	}
	mustContain(t, s.get(path), "1 人收藏", "取消收藏")
	mustContain(t, s.get("/me/collections/favorites"), "雨天片单")
	mustContain(t, s.get("/settings/export.json"), `"collection_favorites"`, list.Slug)
	for range 2 {
		r := s.do(visitor, http.MethodPost, "/api/collections/"+list.Slug+"/view", nil, map[string]string{"X-CSRF-Token": s.csrf(visitor)})
		status(r, http.StatusNoContent)
	}
	if current().Views != 1 {
		t.Fatalf("同一访客当天应只计一次，得到 %d", current().Views)
	}

	if err := s.app.Media.SetHidden(ctx, two, true); err != nil {
		t.Fatal(err)
	}
	s.client = author
	editor := s.get(mine)
	if !strings.Contains(html.UnescapeString(editor.body), `"hidden":true`) {
		t.Fatal("隐藏作品应在编辑器中标记")
	}
	fields.Set("base", current().UpdatedAt.Format(time.RFC3339Nano))
	fields.Set("title", "雨天片单修订版")
	mustContain(t, post("/me/collections/preview", fields, false), "雨天片单修订版")
	mustContain(t, visit(path), "雨天片单")
	if strings.Contains(visit(path).body, "雨天片单修订版") {
		t.Fatal("预览不能提前公开修改")
	}
	var firstPublished time.Time
	if err := s.app.Pool.QueryRow(ctx, "SELECT published_at FROM collections WHERE id = $1", list.ID).Scan(&firstPublished); err != nil {
		t.Fatal(err)
	}
	fields.Set("intent", "save")
	status(post("/me/collections", fields, true), http.StatusConflict)
	fields.Set("intent", "submit")
	status(post("/me/collections", fields, true), http.StatusOK)
	status(post("/me/collections", fields, true), http.StatusConflict)
	// 修改审核期间公开页、站点地图和收藏夹都继续显示上一版。
	assertLiveVersion := func() {
		t.Helper()
		page := visit(path)
		mustContain(t, page, "雨天片单", "1 人收藏")
		if strings.Contains(page.body, "雨天片单修订版") {
			t.Fatal("修改通过审核前不能公开")
		}
		mustContain(t, visit("/sitemap-collections-1.xml"), path)
		s.client = reader
		favorites := s.get("/me/collections/favorites")
		mustContain(t, favorites, "雨天片单")
		if strings.Contains(favorites.body, "雨天片单修订版") {
			t.Fatal("收藏夹不能展示审阅中的内容")
		}
	}
	assertLiveVersion()
	s.client = author
	mustContain(t, s.get("/me/collections"), "修改待审核")
	mustContain(t, s.get(mine), "雨天片单修订版")
	s.client = admin
	mustContain(t, s.get("/admin/collections"), "雨天片单", ">修改</span>")
	mustContain(t, s.get(manage), "雨天片单修订版", "通过修改", "下架片单")
	reviewing, items, err := s.app.Collections.ModerationDetail(ctx, list.ID)
	if err != nil || reviewing.Title != "雨天片单修订版" || len(items) != 2 || !items[0].Hidden {
		t.Fatalf("审核页应显示修改稿并保留暂时隐藏的条目: %+v %+v %v", reviewing, items, err)
	}
	status(decision("return", "补充选片理由"), http.StatusSeeOther)
	assertLiveVersion()
	s.client = author
	mustContain(t, s.get(mine), "补充选片理由", "雨天片单修订版")
	mustContain(t, s.get("/notifications"), "退回了你的片单投稿")
	fields.Set("base", current().UpdatedAt.Format(time.RFC3339Nano))
	fields.Set("entries", `[{"id":"99999999"}]`)
	status(post("/me/collections", fields, true), http.StatusConflict)
	fields.Set("entries", string(entries))
	status(post("/me/collections", fields, true), http.StatusOK)
	s.client = admin
	status(decision("approve", ""), http.StatusSeeOther)
	var published time.Time
	if err := s.app.Pool.QueryRow(ctx, "SELECT published_at FROM collections WHERE id = $1", list.ID).Scan(&published); err != nil || !published.Equal(firstPublished) {
		t.Fatalf("重新审核后应保留首次发布时间: %v %v", published, err)
	}
	mustContain(t, visit(path), "雨天片单修订版", "1 人收藏", "1 次浏览")
	s.client = author
	status(changeVisibility("withdraw-link"), http.StatusSeeOther)
	mustContain(t, visit(path), "仅链接可见", "雨天片单修订版")
	if strings.Contains(visit("/list").body, "雨天片单修订版") {
		t.Fatal("撤下广场并保留链接后，片单不应留在广场")
	}
	status(visit(profile), http.StatusForbidden)
	status(changeVisibility("unshare"), http.StatusSeeOther)
	status(visit(path), http.StatusNotFound)

	fields.Set("base", current().UpdatedAt.Format(time.RFC3339Nano))
	status(post("/me/collections", fields, true), http.StatusOK)
	staleReview := current().UpdatedAt
	fields.Set("base", staleReview.Format(time.RFC3339Nano))
	fields.Set("title", "雨天片单最终版")
	status(post("/me/collections", fields, true), http.StatusOK)
	s.client = admin
	status(s.form(manage, url.Values{"intent": {"approve"}, "base": {staleReview.Format(time.RFC3339Nano)}}), http.StatusConflict)
	status(decision("approve", ""), http.StatusSeeOther)
	status(decision("remove", "片单需要重新整理"), http.StatusSeeOther)
	status(visit(path), http.StatusNotFound)
	s.client = author
	mustContain(t, s.get("/notifications"), "下架了你的片单", "片单需要重新整理")
}

func TestCollectionOrderingAndErrorResponses(t *testing.T) {
	s := newSite(t)
	s.register("editor")
	ctx := context.Background()
	id := s.seedMedia("36000033", "测试选片", media.TypeMovie, nil)
	mostSaved := s.seedPublishedCollection("most-saved", "收藏较多", []int64{id}, nil)
	mostViewed := s.seedPublishedCollection("most-viewed", "浏览较多", []int64{id}, nil)
	newest := s.seedPublishedCollection("newest", "最新公开片单", []int64{id}, nil)
	if _, err := s.app.Pool.Exec(ctx, "INSERT INTO users (id, email, username, password_hash) VALUES (2, 'reader@example.com', 'reader', 'test')"); err != nil {
		t.Fatal(err)
	}
	if err := s.app.Collections.SetFavorite(ctx, mostSaved, 2, true); err != nil {
		t.Fatal(err)
	}
	if err := s.app.Collections.SetFavorite(ctx, mostViewed, 2, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.app.Pool.Exec(ctx, "UPDATE collection_favorites SET created_at = now() - interval '8 days' WHERE collection_id = $1", mostViewed); err != nil {
		t.Fatal(err)
	}
	if err := s.app.Collections.RecordView(ctx, "most-viewed", 2, time.Now().UTC(), []byte("reader")); err != nil {
		t.Fatal(err)
	}
	if err := s.app.Collections.RecordView(ctx, "newest", 2, time.Now().UTC().AddDate(0, 0, -8), []byte("reader")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.app.Pool.Exec(ctx, "UPDATE collections SET updated_at = now() WHERE id = $1", mostSaved); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ path, first, second string }{
		{"/list", "最新公开片单", "收藏较多"},
		{"/list?sort=hot", "收藏较多", "浏览较多"},
		{"/list?sort=hot", "浏览较多", "最新公开片单"},
		{"/api/htmx/row/collections", "最新公开片单", "收藏较多"},
	} {
		r := s.get(entry.path)
		mustContain(t, r, entry.first, entry.second)
		if strings.Index(r.body, entry.first) > strings.Index(r.body, entry.second) {
			t.Fatalf("排序不符合预期: %s %s", entry.path, entry.first)
		}
	}
	// 取消后重新收藏算一次新的收藏：两份片单近 7 天各有一个收藏，再比浏览量。
	for _, active := range []bool{false, true} {
		if err := s.app.Collections.SetFavorite(ctx, mostViewed, 2, active); err != nil {
			t.Fatal(err)
		}
	}
	if hot := s.get("/list?sort=hot").body; strings.Index(hot, "浏览较多") > strings.Index(hot, "收藏较多") {
		t.Fatal("重新收藏应计入近 7 天热度")
	}
	if err := s.app.Collections.PruneViews(ctx); err != nil {
		t.Fatal(err)
	}
	if c, err := s.app.Collections.ByID(ctx, newest); err != nil || c.Views != 1 {
		t.Fatalf("清理历史去重记录不能影响累计浏览量: %+v %v", c, err)
	}
	missing := s.get("/list/does-not-exist")
	if missing.status != 404 || missing.header.Get("Cache-Control") != "no-store" ||
		!strings.Contains(missing.body, "浏览片单") || !strings.Contains(missing.body, `name="robots" content="noindex"`) {
		t.Fatalf("应返回真实 404 和可恢复的页面: %d", missing.status)
	}
	if r := s.get("/list?page=99"); r.status != 404 {
		t.Fatalf("不存在的分页应返回 404: %d", r.status)
	}
	if r := s.get("/api/nonexistent"); r.status != 404 || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Fatalf("API 错误应保留 JSON: %d %s", r.status, r.header.Get("Content-Type"))
	}
}

// 安卓上已安装的网页应用、国产浏览器和应用内浏览器打开新窗口时只带地址：旧版把预览表单 POST 到新标签页，
// 到了服务器变成 GET /me/collections/preview，落到 /me/collections/:id，报「400 参数不正确」。
// 现在预览在编辑页里用 fetch（multipart 表单，HX-Request）取片段，直接打开地址时给出明确提示。
func TestCollectionPreviewWithoutNewWindow(t *testing.T) {
	s := newSite(t)
	s.register("author")
	one := s.seedMedia("36000051", "雨夜谜案", media.TypeMovie, nil)

	editor := s.get("/me/collections/new")
	mustContain(t, editor, "data-preview-dialog", `type="button" data-preview`)
	if strings.Contains(editor.body, "formtarget") || strings.Contains(editor.body, "/me/collections/preview") {
		t.Fatal("预览不应再靠表单提交到新窗口")
	}

	lost := s.get("/me/collections/preview")
	if lost.status != http.StatusBadRequest || strings.Contains(lost.body, "参数不正确") {
		t.Fatalf("丢了 POST 内容的预览请求应给出明确提示: HTTP %d %.300s", lost.status, lost.body)
	}
	mustContain(t, lost, "预览内容只在片单编辑器里生成")

	entries, _ := json.Marshal([]map[string]string{{"id": itoa(one), "note": "第一场雨\r\n要留意。"}})
	preview := func(fields map[string]string) reply {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for key, value := range fields {
			_ = form.WriteField(key, value)
		}
		_ = form.Close()
		return s.do(s.client, http.MethodPost, "/me/collections/preview", &body, map[string]string{
			"Content-Type": form.FormDataContentType(), "HX-Request": "true", "X-CSRF-Token": s.csrf(s.client),
		})
	}
	ok := preview(map[string]string{"id": "0", "base": "", "title": "雨天片单", "description": "适合雨天", "entries": string(entries)})
	if ok.status != http.StatusOK || ok.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("预览片段: HTTP %d %q", ok.status, ok.header.Get("Cache-Control"))
	}
	mustContain(t, ok, "雨天片单", "适合雨天", "1 部", "雨夜谜案", "要留意。")
	if strings.Contains(ok.body, "<html") || strings.Contains(ok.body, "<main") {
		t.Fatalf("预览应只返回片段: %.300s", ok.body)
	}

	bad := preview(map[string]string{"id": "0", "title": "雨天片单", "entries": "[]"})
	if bad.status != http.StatusBadRequest || !strings.HasPrefix(bad.header.Get("Content-Type"), "text/plain") {
		t.Fatalf("预览出错时应返回纯文本，方便编辑器显示: HTTP %d %q", bad.status, bad.header.Get("Content-Type"))
	}
	mustContain(t, bad, "请在片单中加入 1 至 200 部作品。")
}

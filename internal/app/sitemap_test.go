package app_test

import (
	"context"
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
)

func TestSitemapIndexShardsAndCollectionSEO(t *testing.T) {
	s := newSite(t)
	s.register("editor")
	ctx := context.Background()
	_, err := s.app.Pool.Exec(ctx, `
INSERT INTO media (id, title, media_type, hidden, metadata_status, updated_at) VALUES
(10000, '边界电影', 'movie', false, 'complete', '2025-01-03T10:00:00Z'),
(10001, '下一个分片', 'tv', false, 'complete', now()),
(58001, '较高编号', 'movie', false, 'complete', now()),
(20001, '隐藏电影', 'movie', true, 'complete', now()),
(20002, '未补全电影', 'movie', false, 'partial', now()),
(20003, '短片', 'short', false, 'complete', now());

INSERT INTO collections (id, owner_user_id, slug, title, status, published_at, submitted_at, link_visible, updated_at) VALUES
(10000, 1, 'range-one', '公路电影', 'published', '2025-01-01T10:00:00Z', NULL, false, '2025-01-02T10:00:00Z'),
(10001, 1, 'range-two', '第二个分片', 'published', now(), NULL, false, now()),
(10002, 1, 'shared-only', '仅链接片单', 'draft', NULL, NULL, true, now()),
(10003, 1, 'empty-published', '空片单', 'published', now(), NULL, false, now()),
(20001, 1, 'under-review', '待审片单', 'pending', NULL, now(), false, now()),
(20002, 1, 'taken-down', '下架片单', 'returned', now(), NULL, false, now()),
(20003, 1, 'hidden-content', '只含隐藏作品', 'published', now(), NULL, false, now());

INSERT INTO collections (id, owner_user_id, slug, title, status, published_at)
SELECT id, 1, 'public-' || id, '公开片单 ' || id, 'published', now() FROM generate_series(1, 501) AS id;
INSERT INTO collection_items (collection_id, media_id, position)
SELECT id, 10000, 0 FROM collections WHERE slug NOT IN ('empty-published', 'hidden-content');
INSERT INTO collection_items (collection_id, media_id, position) VALUES (20003, 20001, 0);`)
	if err != nil {
		t.Fatal(err)
	}
	s.client = s.browser()
	loadSitemap := func(path, root, child string) map[string]string {
		t.Helper()
		response := s.get(path)
		if response.status != http.StatusOK || !strings.HasPrefix(response.header.Get("Content-Type"), "application/xml") ||
			response.header.Get("Set-Cookie") != "" {
			t.Fatalf("%s 返回 %d %v", path, response.status, response.header)
		}
		var doc struct {
			XMLName xml.Name
			Entries []struct {
				XMLName xml.Name
				Loc     string `xml:"loc"`
				LastMod string `xml:"lastmod"`
			} `xml:",any"`
		}
		if err := xml.Unmarshal([]byte(response.body), &doc); err != nil || doc.XMLName.Local != root ||
			doc.XMLName.Space != "http://www.sitemaps.org/schemas/sitemap/0.9" {
			t.Fatalf("%s XML 无效: %v %+v", path, err, doc.XMLName)
		}
		urls := make(map[string]string, len(doc.Entries))
		for _, entry := range doc.Entries {
			if entry.XMLName.Local != child || !strings.HasPrefix(entry.Loc, "http://moovie.test/") {
				t.Fatalf("%s 包含无效节点: %+v", path, entry)
			}
			url := strings.TrimPrefix(entry.Loc, "http://moovie.test")
			if _, exists := urls[url]; exists {
				t.Fatalf("%s 重复包含 %s", path, url)
			}
			urls[url] = entry.LastMod
		}
		return urls
	}
	index := loadSitemap("/sitemap.xml", "sitemapindex", "sitemap")
	for _, path := range []string{"/sitemap-pages.xml", "/sitemap-collections-1.xml", "/sitemap-collections-2.xml",
		"/sitemap-media-1.xml", "/sitemap-media-2.xml", "/sitemap-media-6.xml"} {
		if _, ok := index[path]; !ok {
			t.Errorf("站点地图索引缺少 %s", path)
		}
	}
	if len(index) != 6 {
		t.Errorf("站点地图索引包含空分片: %+v", index)
	}
	pages := loadSitemap("/sitemap-pages.xml", "urlset", "url")
	if _, ok := pages["/list"]; !ok {
		t.Error("基础页面站点地图缺少片单广场")
	}
	lists := loadSitemap("/sitemap-collections-1.xml", "urlset", "url")
	if len(lists) != 502 || lists["/list/range-one"] != "2025-01-02T10:00:00Z" {
		t.Errorf("片单分片分页或更新时间不符: %d %s", len(lists), lists["/list/range-one"])
	}
	second := loadSitemap("/sitemap-collections-2.xml", "urlset", "url")
	if len(second) != 1 || second["/list/range-two"] == "" {
		t.Errorf("片单分片收录了不应出现的 URL: %+v", second)
	}
	movies := loadSitemap("/sitemap-media-1.xml", "urlset", "url")
	if len(movies) != 1 || movies["/m/10000"] != "2025-01-03T10:00:00Z" {
		t.Errorf("作品分片边界不正确: %+v", movies)
	}
	for _, path := range []string{"/sitemap-media-3.xml", "/sitemap-collections-3.xml", "/sitemap-media-01.xml",
		"/sitemap-media-9223372036854775807.xml", "/sitemap-unknown-1.xml"} {
		if r := s.get(path); r.status != http.StatusNotFound {
			t.Errorf("%s 应返回 404，实际 %d", path, r.status)
		}
	}
	if media := loadSitemap("/sitemap-media-6.xml", "urlset", "url"); media["/m/58001"] == "" {
		t.Error("高编号作品不应被旧的总量上限排除")
	}
	mustContain(t, s.get("/list?sort=new"), `rel="canonical" href="http://moovie.test/list"`, `"@type":"CollectionPage"`)
	mustContain(t, s.get("/list?page=2"), `rel="canonical" href="http://moovie.test/list?page=2"`, "第 2 页", `"@type":"ItemList"`)
	mustContain(t, s.get("/list?sort=hot"), `name="robots" content="noindex,follow"`)
	mustContain(t, s.get("/list/empty-published"), `name="robots" content="noindex,follow"`)
	mustContain(t, s.get("/list/range-one"), "公路电影 - 片单", "边界电影", `"datePublished":"2025-01-01T10:00:00Z"`)
}

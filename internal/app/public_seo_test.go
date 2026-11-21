package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"moovie/internal/media"
)

func TestDiscoverAndCinemaPaginationSEO(t *testing.T) {
	s := newSite(t)
	s.register("editor")
	ctx := context.Background()
	_, err := s.app.Pool.Exec(ctx, `
INSERT INTO media (id, title, year, genres, media_type, metadata_status)
SELECT id, '分页电影 ' || id, 2025, ARRAY['悬疑'], 'movie', 'complete' FROM generate_series(1, 61) AS id;
INSERT INTO user_movies (user_id, media_id, status, comment)
SELECT 1, id, 'watched', '公开短评 ' || id FROM generate_series(1, 40) AS id;`)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, s.get("/cinema?tab=following"), `content="noindex,follow"`)
	s.client = s.browser()

	first := s.get("/discover/movie")
	mustContain(t, first, `rel="canonical" href="http://moovie.test/discover/movie"`,
		`href="/discover/movie?page=2" rel="next"`, `hx-get="/api/htmx/discover?page=2&amp;type=movie"`, `href="/m/32"`)
	if strings.Count(first.body, `class="card"`) != 30 || strings.Contains(first.body, `href="/m/31"`) {
		t.Fatal("发现页只应显示 30 部作品，不应把用于判断下一页的额外作品渲染出来")
	}
	second := s.get("/discover/movie?page=2")
	mustContain(t, second, `rel="canonical" href="http://moovie.test/discover/movie?page=2"`,
		`href="/discover/movie" rel="prev"`, `href="/discover/movie?page=3" rel="next"`, `href="/m/31"`)
	if strings.Contains(second.body, "noindex") {
		t.Error("默认排序的有效分页应允许收录")
	}
	fragment := s.get("/api/htmx/discover?page=2&type=movie")
	mustContain(t, fragment, `href="/m/31"`, `href="/discover/movie?page=3" rel="next"`, `hx-get="/api/htmx/discover?page=3&amp;type=movie"`)
	last := s.get("/discover/movie?page=3")
	mustContain(t, last, `rel="canonical" href="http://moovie.test/discover/movie?page=3"`, `href="/m/1"`)
	if strings.Contains(last.body, `rel="next"`) {
		t.Error("最后一页应结束滚动加载")
	}
	filtered := s.get("/discover/movie?genre=%E6%82%AC%E7%96%91&decade=2020&sort=rating&page=2&tracking=discard")
	mustContain(t, filtered, `content="noindex,follow"`,
		`rel="canonical" href="http://moovie.test/discover/movie?decade=2020&amp;genre=%E6%82%AC%E7%96%91&amp;page=2&amp;sort=rating"`,
		`href="/discover/movie?decade=2020&amp;genre=%E6%82%AC%E7%96%91&amp;page=3&amp;sort=rating" rel="next"`)
	mustContain(t, s.get("/discover/movie?page=1&sort=hot"), `rel="canonical" href="http://moovie.test/discover/movie"`)

	mustContain(t, s.get("/cinema"), `href="/cinema?page=2"`, "公开短评 40")
	cinema := s.get("/cinema?page=2")
	mustContain(t, cinema, `rel="canonical" href="http://moovie.test/cinema?page=2"`, `href="/cinema">上一页`, "公开短评 20")
	if strings.Contains(cinema.body, `/cinema?page=3`) {
		t.Error("正好 40 条评论时影院应只提供两页")
	}
	if redirect := s.get("/cinema?tab=following&page=2"); redirect.status != http.StatusFound ||
		redirect.header.Get("Location") != "/auth/login?redirect=%2Fcinema%3Fpage%3D2%26tab%3Dfollowing" {
		t.Errorf("登录后应该回到请求的分页，实际 %d %q", redirect.status, redirect.header.Get("Location"))
	}
	for _, path := range []string{"/discover/movie?page=4", "/discover/movie?page=0", "/discover/movie?page=9223372036854775808",
		"/cinema?page=3", "/cinema?page=文字"} {
		r := s.get(path)
		if r.status != http.StatusNotFound {
			t.Errorf("%s 应返回 404，实际 %d", path, r.status)
		}
	}
}

func TestMediaSeasonStructuredData(t *testing.T) {
	s := newSite(t)
	ctx := context.Background()
	first := s.seedMedia("36000101", "雾港", media.TypeTV, media.Patch{"season": 1, "episode_count": 8})
	second := s.seedMedia("36000102", "雾港 第二季", media.TypeTV, media.Patch{"season": 2, "episode_count": 12})
	film := s.seedMedia("36000103", "回声", media.TypeMovie, nil)
	const seriesIMDb = "https://www.imdb.com/title/tt1234567/"
	const seriesTMDB = "https://www.themoviedb.org/tv/4455"
	for _, id := range []int64{first, second} {
		for provider, idValue := range map[string]string{media.ProviderIMDb: "tt1234567", media.ProviderTMDB: "tv/4455"} {
			if err := s.app.Media.SetExternalID(ctx, id, provider, idValue); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.app.Media.SetExternalID(ctx, film, media.ProviderIMDb, "tt4444444"); err != nil {
		t.Fatal(err)
	}

	type entity struct {
		Type         string   `json:"@type"`
		ID           string   `json:"@id"`
		Name         string   `json:"name"`
		Number       int      `json:"seasonNumber"`
		Episodes     int      `json:"numberOfEpisodes"`
		SameAs       []string `json:"sameAs"`
		PartOfSeries *entity  `json:"partOfSeries"`
	}
	jsonLD := regexp.MustCompile(`<script type="application/ld\+json">([^<]+)</script>`)
	read := func(id int64) entity {
		t.Helper()
		r := s.get("/m/" + itoa(id))
		match := jsonLD.FindStringSubmatch(r.body)
		if r.status != http.StatusOK || len(match) == 0 {
			t.Fatalf("作品 %d 没有结构化数据，状态 %d", id, r.status)
		}
		var schema struct {
			Graph []entity `json:"@graph"`
		}
		if err := json.Unmarshal([]byte(match[1]), &schema); err != nil || len(schema.Graph) < 2 {
			t.Fatalf("作品 %d 结构化数据无效: %v", id, err)
		}
		if schema.Graph[1].Type != "BreadcrumbList" {
			t.Errorf("作品 %d 应保留面包屑", id)
		}
		return schema.Graph[0]
	}
	for _, season := range []struct {
		id       int64
		number   int
		episodes int
		douban   string
	}{{first, 1, 8, "36000101"}, {second, 2, 12, "36000102"}} {
		work := read(season.id)
		if work.Type != "TVSeason" || work.Number != season.number || work.Episodes != season.episodes ||
			!slices.Equal(work.SameAs, []string{"https://movie.douban.com/subject/" + season.douban + "/"}) {
			t.Errorf("单季实体不正确: %+v", work)
		}
		parent := work.PartOfSeries
		if parent == nil || parent.Type != "TVSeries" || parent.Name != "雾港" || parent.ID != seriesIMDb ||
			!slices.Equal(parent.SameAs, []string{seriesIMDb, seriesTMDB}) {
			t.Errorf("单季应关联同一个整剧实体: %+v", parent)
		}
	}
	if work := read(film); work.Type != "Movie" || work.PartOfSeries != nil || !slices.Contains(work.SameAs, "https://www.imdb.com/title/tt4444444/") {
		t.Errorf("电影应保留自身的 IMDb 对应条目: %+v", work)
	}
}

// 每个页面的 <head>：标题统一带站点名，公开页面有描述、关键词、canonical、Open Graph 和 Twitter Card，私有页面 noindex。
func TestHeadMeta(t *testing.T) {
	s := newSite(t)
	show := s.seedMedia("36000201", "雾港", media.TypeTV, media.Patch{"actors": []string{"陈默"}})

	home := s.get("/")
	mustContain(t, home, "<title>Moovie 影牛 - 发现你的下一部电影</title>", `<meta name="description" content="Moovie 影牛：发现你的下一部电影。`,
		`<meta name="keywords" content="电影推荐,`, `,Moovie 影牛">`, `<link rel="canonical" href="http://moovie.test/">`,
		`<meta property="og:title" content="Moovie 影牛 - 发现你的下一部电影">`, `<meta property="og:site_name" content="Moovie 影牛">`,
		`<meta name="twitter:card" content="summary">`, `<meta property="og:image" content="http://moovie.test/static/img/icon-512.png">`)

	detail := s.get("/m/" + itoa(show))
	mustContain(t, detail, "<title>雾港（2026） - 剧集简介、演员表、短评 - Moovie 影牛</title>",
		`<meta name="keywords" content="雾港,雾港 2026,雾港剧集,剧集,悬疑,陈默,Moovie 影牛">`,
		`<meta property="og:title" content="雾港（2026） - 剧集简介、演员表、短评">`, `<meta property="og:type" content="video.tv_show">`,
		`<meta name="twitter:card" content="summary_large_image">`, "台风季的港城")

	for path, title := range map[string]string{
		"/discover/movie":     "热门电影推荐 - 按类型、年代和评分筛选 - Moovie 影牛",
		"/discover":           "发现好片 - 热门电影、剧集、综艺和动漫推荐 - Moovie 影牛",
		"/search":             "搜索电影、剧集、综艺和动漫 - Moovie 影牛",
		"/search?q=%E9%9B%BE": "“雾”的搜索结果 - Moovie 影牛",
		"/list":               "片单广场 - 电影与剧集主题片单 - Moovie 影牛",
		"/cinema":             "影院 - 影迷最新短评与评分 - Moovie 影牛",
		"/trends":             "影视热搜榜 - 大家都在搜的电影和剧集 - Moovie 影牛",
		"/about":              "关于 - Moovie 影牛",
	} {
		r := s.get(path)
		mustContain(t, r, "<title>"+title+"</title>", `<meta name="description" content="`, `<meta name="keywords" content="`,
			`<link rel="canonical"`, `<meta property="og:description"`, `<meta name="twitter:title"`)
	}

	login := s.get("/auth/login")
	mustContain(t, login, "<title>登录 - Moovie 影牛</title>", `<meta name="robots" content="noindex">`, `<meta name="description" content="Moovie 影牛：`)
	for _, unwanted := range []string{"og:title", `rel="canonical"`} {
		if strings.Contains(login.body, unwanted) {
			t.Errorf("登录页不应该有 %q", unwanted)
		}
	}
	s.register("viewer")
	for _, path := range []string{"/me", "/settings", "/notifications", "/feedback"} {
		mustContain(t, s.get(path), `<meta name="robots" content="noindex">`)
	}
	mustContain(t, s.get("/"), "<title>Moovie 影牛 - 发现你的下一部电影</title>")
}

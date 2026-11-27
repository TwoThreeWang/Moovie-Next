package ingest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"moovie/internal/db/testdb"
	"moovie/internal/ingest"
	"moovie/internal/media"
	"moovie/internal/providers"
)

// fakeHongguo 是假的红果官网：详情页、榜单、分类、站点地图都按官网的结构返回。
type fakeHongguo struct {
	mu       sync.Mutex
	series   map[string]map[string]any // series_id → seriesDetail；不在里面的返回"没有这部剧"
	social   map[string]map[string]any
	ranks    map[string][]map[string]any // 榜单名（如 hot-drama）→ 第 1 页
	sitemaps map[string][]string         // 子站点地图路径 → series_id
	requests []string
}

func newFakeHongguo(t *testing.T) (*fakeHongguo, *providers.Hongguo) {
	fake := &fakeHongguo{series: map[string]map[string]any{}, social: map[string]map[string]any{}, ranks: map[string][]map[string]any{}, sitemaps: map[string][]string{}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	client := providers.NewHongguo(server.Client())
	client.Base, client.Pace = server.URL, providers.NewPacer(time.Millisecond)
	return fake, client
}

func (f *fakeHongguo) page(w http.ResponseWriter, route string, data any) {
	body, _ := json.Marshal(map[string]any{"loaderData": map[string]any{route: data}})
	w.Write([]byte(`<html><body><script>_ROUTER_DATA = ` + string(body) + `</script></body></html>`))
}

func (f *fakeHongguo) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.RequestURI())
	path := r.URL.Path
	switch {
	case path == "/detail":
		id := r.URL.Query().Get("series_id")
		detail, ok := f.series[id]
		if !ok {
			f.page(w, "detail_page", map[string]any{"seriesDetail": nil})
			return
		}
		f.page(w, "detail_page", map[string]any{"seriesDetail": detail, "seriesSocialInfo": f.social[id]})
	case strings.HasPrefix(path, "/rank/"):
		kind := strings.TrimPrefix(path, "/rank/")
		list := f.ranks[kind]
		if r.URL.Query().Get("page") != "" {
			list = nil
		}
		f.page(w, "rank_"+kind+"/page", map[string]any{"content": map[string]any{"rankList": list}})
	case strings.HasPrefix(path, "/category/"):
		f.page(w, "category_$", map[string]any{"recommendList": []any{}, "pagination": map[string]any{"totalPages": 1}})
	case path == "/sitemap/hongguoduanju/index.xml":
		var b strings.Builder
		for name := range f.sitemaps {
			if name != "/seo-important.xml" {
				b.WriteString("<sitemap><loc>http://" + r.Host + name + "</loc></sitemap>")
			}
		}
		w.Write([]byte("<sitemapindex>" + b.String() + "</sitemapindex>"))
	default:
		ids, ok := f.sitemaps[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		etag := `"` + strings.Join(ids, "-") + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		var b strings.Builder
		for _, id := range ids {
			b.WriteString("<url><loc>https://hongguoduanju.com/detail?series_id=" + id + "</loc></url>")
		}
		w.Write([]byte("<urlset>" + b.String() + "</urlset>"))
	}
}

func (f *fakeHongguo) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, request := range f.requests {
		if strings.HasPrefix(request, prefix) {
			n++
		}
	}
	return n
}

func detail(id, title string, episodes int, ended bool, tags ...string) map[string]any {
	text := "全" + itoa(episodes) + "集"
	if !ended {
		text = "更新至" + itoa(episodes) + "集"
	}
	return map[string]any{
		"series_id": id, "series_name": title, "series_intro": title + "的简介", "tags": tags,
		"series_cover":       "https://p3-novel.byteimg.com/novel-pic/" + id + "~tplv-shrink:640:0.image",
		"episode_right_text": text, "first_visible_time": "1794845285",
		"series_episode_info": map[string]any{"episode_cnt": episodes, "episode_total_cnt": episodes},
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func newShortService(t *testing.T) (*ingest.Service, *fakeHongguo) {
	service, _ := newService(t)
	fake, client := newFakeHongguo(t)
	service.Hongguo, service.ShortSync = client, true
	return service, fake
}

func TestShortImportAndRefresh(t *testing.T) {
	service, fake := newShortService(t)
	ctx := context.Background()
	series := &providers.HongguoSeries{
		ID: "7706890402913078297", Title: "帐中香第二季", Intro: "简介", Tags: []string{"恋爱", "古代"}, Episodes: 135, Total: 135, Ended: true,
		OnlineAt: time.Date(2026, 11, 16, 16, 8, 5, 0, time.UTC), GroupID: "7687910242989919256", Rating: 9, Heat: 70974984,
		Cover: "https://p6-novel.byteimg.com/novel-pic/abc~tplv-shrink:640:0.image",
	}
	if err := service.ImportShort(ctx, ingest.ShortRecord{ID: series.ID, Form: "漫剧", Series: series}); err != nil {
		t.Fatal(err)
	}
	if err := service.ImportShort(ctx, ingest.ShortRecord{ID: "7700000000000000099", Missing: true}); err != nil {
		t.Fatal(err)
	}
	// 导入两次结果一样：按 series_id 更新，不会多出一行。
	if err := service.ImportShort(ctx, ingest.ShortRecord{ID: series.ID, Series: series}); err != nil {
		t.Fatal(err)
	}
	id, created, err := service.Media.EnsureHongguo(ctx, series.ID)
	if err != nil || created {
		t.Fatalf("导入过的短剧应该已经在作品库里: created=%v err=%v", created, err)
	}
	m, _ := service.Media.Get(ctx, id)
	if m.Type != media.TypeShort || m.Title != "帐中香第二季" || m.Season != 2 || m.Year != 2026 || m.EpisodeCount != 135 ||
		m.SeriesStatus != "ended" || m.SeriesKey != "hg:7687910242989919256" || m.RatingHongguo != 9 || m.Heat != 70974984 ||
		m.MetadataStatus != "complete" || m.Form() != "漫剧" || strings.Join(m.Topics(), ",") != "恋爱,古代" {
		t.Errorf("导入的资料不对（第二次导入没带剧种时应该保留第一次的）: %+v", m)
	}
	missingID, _, _ := service.Media.EnsureHongguo(ctx, "7700000000000000099")
	missing, _ := service.Media.Get(ctx, missingID)
	if !missing.Hidden || missing.MetadataStatus != "not_found" {
		t.Errorf("不存在的剧应该隐藏并标记 not_found: %+v", missing)
	}

	// 线上刷新：集数变了；红果上下架了的隐藏。打开作品页时排的是红果的任务，不是豆瓣的。
	fake.series[series.ID] = detail(series.ID, "帐中香第二季", 140, true, "恋爱")
	if err := service.EnqueueRefresh(ctx, id, ingest.PriorityViewing); err != nil {
		t.Fatal(err)
	}
	if job, ok, _ := service.Queue.Latest(ctx, ingest.KindShortRefresh, itoa64(id)); !ok || job.Status != "queued" {
		t.Fatalf("短剧应该排 %s 任务: %+v", ingest.KindShortRefresh, job)
	}
	if err := service.RefreshShort(ctx, id); err != nil {
		t.Fatal(err)
	}
	m, _ = service.Media.Get(ctx, id)
	if m.EpisodeCount != 140 || m.Form() != "漫剧" || m.RatingHongguo != 9 {
		t.Errorf("刷新后集数应该更新，剧种和红果这次没给的评分保留: %+v", m)
	}
	delete(fake.series, series.ID)
	if err := service.RefreshShort(ctx, id); err != nil {
		t.Fatal(err)
	}
	if m, _ = service.Media.Get(ctx, id); !m.Hidden || m.MetadataStatus != "not_found" {
		t.Errorf("红果上没有了应该隐藏: %+v", m)
	}
}

func itoa64(id int64) string { return strconv.FormatInt(id, 10) }

func TestShortListsBuildChartsAndForms(t *testing.T) {
	service, fake := newShortService(t)
	ctx := context.Background()
	fake.ranks["hot-drama"] = []map[string]any{
		{"seriesId": "7705824595416190014", "title": "穿越八零", "tags": []string{"AI剧", "剧情"}},
		{"seriesId": "7705824595416190015", "title": "预约的", "reservationOnly": true},
	}
	fake.ranks["new-drama"] = []map[string]any{{"seriesId": "7704532495357725721", "title": "持械入宋：第十一季"}}
	fake.ranks["hot-comic-drama"] = []map[string]any{{"seriesId": "7704532495357725721", "title": "持械入宋：第十一季"}}
	fake.sitemaps["/seo-important.xml"] = []string{"7621200213801176126", "7705824595416190014"}
	if err := service.RefreshShortLists(ctx); err != nil {
		t.Fatal(err)
	}
	// 资料还没抓到的作品没有片名，榜单照样记着，但先不显示。
	var listed int
	testdb.Pool(t).QueryRow(ctx, `SELECT count(*) FROM chart_items WHERE kind IN ('short_hot', 'short_new')`).Scan(&listed)
	if hot, _ := service.Media.Chart(ctx, ingest.ChartShortHot, 10); listed != 2 || len(hot) != 0 {
		t.Fatalf("两个榜单各记一部（跳过只能预约的剧），资料没抓到前不显示: listed=%d shown=%d", listed, len(hot))
	}
	id, _, _ := service.Media.EnsureHongguo(ctx, "7704532495357725721")
	if m, _ := service.Media.Get(ctx, id); m.Form() != "漫剧" {
		t.Errorf("分剧种榜单上的剧应该记上剧种: %+v", m.Genres)
	}
	aiID, _, _ := service.Media.EnsureHongguo(ctx, "7705824595416190014")
	if m, _ := service.Media.Get(ctx, aiID); m.Form() != "AI剧" {
		t.Errorf("榜单标签里的剧种应该记上: %+v", m.Genres)
	}
	// 上榜的、重点剧目里的都排了刷新（同一部只排一次）。
	counts, _ := service.Queue.Counts(ctx)
	if counts["queued"] != 3 {
		t.Errorf("应该给 3 部短剧排刷新，实际 %v", counts)
	}

	// 跑一遍刷新任务，资料抓到了，片名就有了。
	fake.series["7705824595416190014"] = detail("7705824595416190014", "穿越八零，我把娘俩宠成宝", 317, true, "剧情")
	if err := service.RefreshShort(ctx, aiID); err != nil {
		t.Fatal(err)
	}
	if hot, _ := service.Media.Chart(ctx, ingest.ChartShortHot, 10); len(hot) != 1 || hot[0].Title != "穿越八零，我把娘俩宠成宝" || hot[0].Type != media.TypeShort {
		t.Errorf("榜单卡片应该显示抓到的资料: %+v", hot)
	}
}

func TestDiscoverNewShortsOnlyTakesUnknownIDs(t *testing.T) {
	service, fake := newShortService(t)
	ctx := context.Background()
	known, _, _ := service.Media.EnsureHongguo(ctx, "7600000000000000001")
	fake.sitemaps["/sitemap/hongguoduanju/index1.xml"] = []string{"7600000000000000001", "7600000000000000002"}
	fake.sitemaps["/sitemap/hongguoduanju/index2.xml"] = []string{"7700000000000000003"}
	if err := service.DiscoverNewShorts(ctx); err != nil {
		t.Fatal(err)
	}
	counts, _ := service.Queue.Counts(ctx)
	if counts["queued"] != 2 {
		t.Errorf("只有两部新剧要排刷新，实际 %v", counts)
	}
	if job, ok, _ := service.Queue.Latest(ctx, ingest.KindShortRefresh, itoa64(known)); ok {
		t.Errorf("已经有的剧不应该重新排: %+v", job)
	}
	// 第二天站点地图没变：带 ETag 请求，对方回 304，不重新下载。
	before := fake.count("/sitemap/hongguoduanju/index1.xml")
	if err := service.DiscoverNewShorts(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.count("/sitemap/hongguoduanju/index1.xml") != before+1 {
		t.Errorf("子站点地图应该再请求一次（带 ETag）")
	}
	if counts, _ = service.Queue.Counts(ctx); counts["queued"] != 2 {
		t.Errorf("没有新剧时不应该多排任务: %v", counts)
	}
}

// 短剧只出现在短剧频道：发现页的"全部"、长剧的巡检都不会碰到它们。
func TestShortStaysInItsChannel(t *testing.T) {
	service, _ := newShortService(t)
	ctx := context.Background()
	movie, _ := service.Media.Create(ctx)
	service.Media.ApplyPatch(ctx, movie, media.Patch{"title": "一部电影", "media_type": media.TypeMovie, "genres": []string{"剧情"}})
	service.Media.MarkSynced(ctx, movie, "complete")
	for index, item := range []struct {
		id, title string
		heat      int64
		tags      []string
	}{
		{"7600000000000000001", "冷门甜宠", 10, []string{"甜宠", "现代"}},
		{"7600000000000000002", "热门甜宠", 900, []string{"甜宠", "总裁"}},
		{"7600000000000000003", "热门古装", 500, []string{"古代"}},
	} {
		record := ingest.ShortRecord{ID: item.id, Series: &providers.HongguoSeries{ID: item.id, Title: item.title, Tags: item.tags, Heat: item.heat, Episodes: 60 + index, Ended: true}}
		if err := service.ImportShort(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := service.Media.Discover(ctx, media.Filter{})
	if len(all) != 1 || all[0].ID != movie {
		t.Errorf("发现页的全部里不应该有短剧: %+v", all)
	}
	shorts, _ := service.Media.Discover(ctx, media.Filter{Type: media.TypeShort})
	if len(shorts) != 3 || shorts[0].Title != "热门甜宠" || shorts[2].Title != "冷门甜宠" {
		t.Errorf("短剧频道应该按热度排: %+v", shorts)
	}
	sweet, _ := service.Media.Discover(ctx, media.Filter{Type: media.TypeShort, Genre: "甜宠", Sort: "new"})
	if len(sweet) != 2 || sweet[0].Title != "热门甜宠" {
		t.Errorf("按题材筛选、按上架顺序（series_id 倒序）排: %+v", sweet)
	}
	topics, _ := service.Media.ShortTopics(ctx, 3)
	if len(topics) == 0 || topics[0] != "甜宠" {
		t.Errorf("最常见的题材应该是甜宠: %v", topics)
	}
	id, _, _ := service.Media.EnsureHongguo(ctx, "7600000000000000001")
	m, _ := service.Media.Get(ctx, id)
	similar, _ := service.Media.Similar(ctx, m, 5)
	if len(similar) != 1 || similar[0].Title != "热门甜宠" {
		t.Errorf("短剧的相似推荐只在短剧里按题材找: %+v", similar)
	}
	stale, _ := service.Media.Stale(ctx, ingest.KindRefresh, 50)
	for _, staleID := range stale {
		if staleID != movie {
			t.Errorf("豆瓣的巡检不应该挑到短剧: %v", stale)
		}
	}
	semantic, _ := service.Media.NeedsSemantic(ctx, ingest.KindSemantic, ingest.KindEnrich, 50)
	for _, semanticID := range semantic {
		if semanticID != movie {
			t.Errorf("短剧不写 AI 描述: %v", semantic)
		}
	}
	// 搜索：同样匹配时长剧在前。
	service.Media.ApplyPatch(ctx, movie, media.Patch{"title": "热门甜宠"})
	found, _ := service.Media.Search(ctx, "热门甜宠", 5)
	if len(found) < 2 || found[0].ID != movie || found[1].Type != media.TypeShort {
		t.Errorf("同名时长剧应该排在短剧前面: %+v", found)
	}
}

// 连载中的短剧每天重新抓，已完结的不再抓。
func TestStaleShort(t *testing.T) {
	service, _ := newShortService(t)
	ctx := context.Background()
	service.ImportShort(ctx, ingest.ShortRecord{ID: "7600000000000000001", Series: &providers.HongguoSeries{ID: "7600000000000000001", Title: "完结的", Episodes: 80, Ended: true}})
	service.ImportShort(ctx, ingest.ShortRecord{ID: "7600000000000000002", Series: &providers.HongguoSeries{ID: "7600000000000000002", Title: "连载的", Episodes: 30}})
	fresh, _, _ := service.Media.EnsureHongguo(ctx, "7600000000000000003") // 只有 ID，还没抓
	ids, _ := service.Media.StaleShort(ctx, ingest.KindShortRefresh, 10)
	if len(ids) != 1 || ids[0] != fresh {
		t.Errorf("刚抓过的不用再抓，只有没抓过的: %v", ids)
	}
	testdb.Pool(t).Exec(ctx, `UPDATE media SET metadata_synced_at = now() - interval '2 days' WHERE hongguo_id <> ''`)
	ids, _ = service.Media.StaleShort(ctx, ingest.KindShortRefresh, 10)
	if len(ids) != 2 {
		t.Errorf("连载中的和没抓过的要抓，已完结的不抓: %v", ids)
	}
}

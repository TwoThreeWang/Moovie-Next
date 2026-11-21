package media_test

import (
	"context"
	"testing"
	"time"

	"moovie/internal/db/testdb"
	"moovie/internal/media"
)

func seed(t *testing.T, store *media.Store, doubanID, title string, year int, mediaType string) int64 {
	t.Helper()
	patch := media.Patch{}
	patch.Set("title", title)
	patch.Set("year", year)
	patch.Set("media_type", mediaType)
	id, _, err := store.EnsureDouban(context.Background(), doubanID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyPatch(context.Background(), id, media.ProviderDouban, patch); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEnsureDoubanIsIdempotent(t *testing.T) {
	store := media.NewStore(testdb.Pool(t))
	ctx := context.Background()
	seedPatch := media.Patch{}
	seedPatch.Set("title", "雾港")
	first, created, err := store.EnsureDouban(ctx, "30000001", seedPatch)
	if err != nil || !created {
		t.Fatalf("第一次应该新建: id=%d created=%v err=%v", first, created, err)
	}
	second, created, err := store.EnsureDouban(ctx, "30000001", seedPatch)
	if err != nil || created || second != first {
		t.Fatalf("第二次应该返回同一行: id=%d created=%v err=%v", second, created, err)
	}
	if _, _, err := store.EnsureDouban(ctx, "abc", nil); err == nil {
		t.Error("不合法的豆瓣 ID 应该报错")
	}
	m, err := store.Get(ctx, first)
	if err != nil || m.Title != "雾港" || m.SeriesKey != "雾港" {
		t.Errorf("seed 应该写入片名和 series_key: %+v err=%v", m, err)
	}
}

// 字段合并的规则：豆瓣是主要来源，给的值总是写入；TMDB 是辅助来源，只补空着的字段，
// TMDB 独有的字段（剧照、TMDB 评分、连载状态）直接写。
func TestApplyPatchMergeRule(t *testing.T) {
	store := media.NewStore(testdb.Pool(t))
	ctx := context.Background()
	id, _, err := store.EnsureDouban(ctx, "30000002", nil)
	if err != nil {
		t.Fatal(err)
	}
	tmdb := media.Patch{}
	tmdb.Set("title", "Fog Harbor")
	tmdb.Set("summary", "TMDB 的简介")
	tmdb.Set("backdrops", []string{"https://image.tmdb.org/t/p/original/a.jpg"})
	if err := store.ApplyPatch(ctx, id, media.ProviderTMDB, tmdb); err != nil {
		t.Fatal(err)
	}
	douban := media.Patch{}
	douban.Set("title", "雾港")
	douban.Set("rating_douban", 8.9)
	if err := store.ApplyPatch(ctx, id, media.ProviderDouban, douban); err != nil {
		t.Fatal(err)
	}
	// TMDB 再来一次：片名和简介都已经有值，不会被改；剧照和连载状态是 TMDB 独有的，直接更新。
	again := media.Patch{}
	again.Set("title", "Fog Harbor 2")
	again.Set("summary", "TMDB 的新简介")
	again.Set("year", 2024)
	again.Set("backdrops", []string{"https://image.tmdb.org/t/p/original/b.jpg", "https://image.tmdb.org/t/p/original/c.jpg"})
	again.Set("series_status", "ended")
	if err := store.ApplyPatch(ctx, id, media.ProviderTMDB, again); err != nil {
		t.Fatal(err)
	}
	m, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "雾港" {
		t.Errorf("片名应该保持豆瓣的值，实际 %q", m.Title)
	}
	if m.Summary != "TMDB 的简介" || m.Year != 2024 {
		t.Errorf("TMDB 只补空着的字段：简介保持第一次的值，年份补上。实际 %q %d", m.Summary, m.Year)
	}
	if m.RatingDouban < 8.89 || len(m.Backdrops) != 2 || m.SeriesStatus != "ended" {
		t.Errorf("评分、剧照、连载状态没有写入: %+v", m)
	}
	// 豆瓣给了简介：覆盖 TMDB 的
	if err := store.ApplyPatch(ctx, id, media.ProviderDouban, media.Patch{"summary": "豆瓣的简介"}); err != nil {
		t.Fatal(err)
	}
	if m, _ := store.Get(ctx, id); m.Summary != "豆瓣的简介" || m.Title != "雾港" {
		t.Errorf("豆瓣给的值应该总是写入: %q", m.Summary)
	}
	if err := store.ApplyPatch(ctx, id, "resource-site", media.Patch{"title": "x"}); err == nil {
		t.Error("未知来源应该被拒绝")
	}
	if err := store.ApplyPatch(ctx, 999999, media.ProviderDouban, douban); err != media.ErrNotFound {
		t.Errorf("不存在的作品应该返回 ErrNotFound，实际 %v", err)
	}
}

func TestSearchFindsByAliasAndOrdersExactFirst(t *testing.T) {
	store := media.NewStore(testdb.Pool(t))
	ctx := context.Background()
	exact := seed(t, store, "30000010", "雾港", 2026, media.TypeTV)
	seed(t, store, "30000011", "雾港往事", 2020, media.TypeMovie)
	other := seed(t, store, "30000012", "山海食堂", 2025, media.TypeTV)
	if err := store.AddAliases(ctx, other, "Mountain Sea Diner"); err != nil {
		t.Fatal(err)
	}
	hidden := seed(t, store, "30000013", "雾港禁片", 2019, media.TypeMovie)
	if err := store.SetHidden(ctx, hidden, true); err != nil {
		t.Fatal(err)
	}

	cards, err := store.Search(ctx, "雾港", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 || cards[0].ID != exact {
		t.Fatalf("应该找到两部，完全匹配的排第一；实际 %+v", cards)
	}
	cards, err = store.Search(ctx, "mountain sea", 10)
	if err != nil || len(cards) != 1 || cards[0].ID != other {
		t.Fatalf("应该能按别名搜到: %+v err=%v", cards, err)
	}
	if cards, _ := store.Search(ctx, "  ", 10); len(cards) != 0 {
		t.Error("空关键词应该返回空结果")
	}
}

func TestResolve(t *testing.T) {
	store := media.NewStore(testdb.Pool(t))
	ctx := context.Background()
	show := seed(t, store, "30000020", "雾港 第二季", 2026, media.TypeTV)
	movie2010 := seed(t, store, "30000021", "回声", 2010, media.TypeMovie)
	movie2024 := seed(t, store, "30000022", "回声", 2024, media.TypeMovie)
	hidden := seed(t, store, "30000023", "禁片", 2020, media.TypeMovie)
	store.SetHidden(ctx, hidden, true)

	results, err := store.Resolve(ctx, []media.Candidate{
		{DoubanID: "30000020"},                   // 0 豆瓣 ID 精确匹配
		{Title: "雾港第二季", Year: 2026, Type: "tv"}, // 1 片名规范化后匹配
		{Title: "回声", Year: 2024, Type: "movie"}, // 2 用年份区分同名作品
		{Title: "回声"},                            // 3 同名且没有年份：需要用户选
		{Title: "回声", Year: 1990},                // 4 年份差太多：当作没有
		{Title: "雾港 第二季", Type: "movie"},         // 5 类型不符
		{Title: "不存在的片子"},                        // 6
		{DoubanID: "30000023"},                   // 7 隐藏的作品不参与匹配
		{DoubanID: "0", Title: "回声", Year: 2011}, // 8 豆瓣 ID 是 "0" 时退回片名匹配，年份允许差一年
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		status string
		id     int64
	}{
		{"matched", show}, {"matched", show}, {"matched", movie2024}, {"ambiguous", 0},
		{"unknown", 0}, {"unknown", 0}, {"unknown", 0}, {"unknown", 0}, {"matched", movie2010},
	}
	for index, expected := range want {
		got := results[index]
		if got.Status != expected.status {
			t.Errorf("第 %d 条：状态 %q，期望 %q", index, got.Status, expected.status)
			continue
		}
		if expected.id != 0 && (got.Media == nil || got.Media.ID != expected.id) {
			t.Errorf("第 %d 条：匹配到的作品不对：%+v", index, got.Media)
		}
	}
	if len(results[3].Options) != 2 {
		t.Errorf("同名作品应该返回两个候选，实际 %d", len(results[3].Options))
	}
}

func TestChartSeriesUnitsAndAiring(t *testing.T) {
	pool := testdb.Pool(t)
	store := media.NewStore(pool)
	ctx := context.Background()
	s1 := seed(t, store, "30000030", "雾港", 2024, media.TypeTV)
	s2 := seed(t, store, "30000031", "雾港 第二季", 2026, media.TypeTV)
	film := seed(t, store, "30000032", "雾港", 2020, media.TypeMovie)
	store.ApplyPatch(ctx, s2, media.ProviderDouban, media.Patch{"season": 2})

	if err := store.ReplaceChart(ctx, "tv", []int64{s2, s1}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceChart(ctx, "tv", []int64{s1, s2}); err != nil {
		t.Fatal(err)
	}
	chart, err := store.Chart(ctx, "tv", 10)
	if err != nil || len(chart) != 2 || chart[0].ID != s1 {
		t.Fatalf("榜单应该被整批替换成新的顺序: %+v err=%v", chart, err)
	}

	first, _ := store.Get(ctx, s1)
	series, err := store.Series(ctx, first)
	if err != nil || len(series) != 1 || series[0].ID != s2 {
		t.Fatalf("同系列应该只有第二季（同名电影不算）: %+v err=%v film=%d", series, err, film)
	}

	today := media.Today()
	units := []media.Unit{{Episode: 1, Title: "雾起"}, {Episode: 2, Title: "旧码头", AirDate: &today}, {Episode: 3, AirDate: &today}, {Episode: 4, AirDate: &today}}
	if err := store.ReplaceUnits(ctx, s2, 2, units); err != nil {
		t.Fatal(err)
	}
	got, err := store.Units(ctx, s2)
	if err != nil || len(got) != 4 || got[1].Title != "旧码头" || got[1].AirDate == nil {
		t.Fatalf("分集没有正确保存: %+v err=%v", got, err)
	}

	testdb.User(t, pool, 1, 2)
	if _, err := pool.Exec(ctx, `INSERT INTO user_movies (user_id, media_id, status) VALUES (1, $1, 'wish')`, s2); err != nil {
		t.Fatal(err)
	}
	airing, err := store.AiringOn(ctx, 1, time.Now(), 10)
	if err != nil || len(airing) != 1 || airing[0].Label() != "更新第 2–4 集" {
		t.Fatalf("用户 1 想看这部剧，今天更新了第 2–4 集，应该合成一张卡片: %+v err=%v", airing, err)
	}
	if label := (media.Airing{First: 21, Last: 21}).Label(); label != "更新第 21 集" {
		t.Errorf("只更新一集时: %q", label)
	}
	if airing, _ := store.AiringOn(ctx, 2, time.Now(), 10); len(airing) != 0 {
		t.Error("用户 2 没有在追，不应该出现")
	}
}

func TestBlockedKeywordsAndProbe(t *testing.T) {
	pool := testdb.Pool(t)
	store := media.NewStore(pool)
	ctx := context.Background()
	blocked := seed(t, store, "30000040", "某某禁片", 2020, media.TypeMovie)
	kept := seed(t, store, "30000041", "山海食堂", 2025, media.TypeTV)
	if _, err := pool.Exec(ctx, `INSERT INTO blocked_keywords (kind, keyword) VALUES ('copyright', '禁片')`); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyBlockedKeywords(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if m, _ := store.Get(ctx, blocked); !m.Hidden {
		t.Error("片名命中屏蔽词的作品应该被隐藏")
	}
	if m, _ := store.Get(ctx, kept); m.Hidden {
		t.Error("没命中的作品不应该被隐藏")
	}
}

func TestSimilarFallsBackToGenres(t *testing.T) {
	store := media.NewStore(testdb.Pool(t))
	ctx := context.Background()
	base := seed(t, store, "30000050", "雾港", 2026, media.TypeTV)
	close1 := seed(t, store, "30000051", "白夜", 2025, media.TypeTV)
	far := seed(t, store, "30000052", "山海食堂", 2025, media.TypeTV)
	store.ApplyPatch(ctx, base, media.ProviderDouban, media.Patch{"genres": []string{"悬疑", "犯罪"}})
	store.ApplyPatch(ctx, close1, media.ProviderDouban, media.Patch{"genres": []string{"悬疑", "犯罪", "剧情"}})
	store.ApplyPatch(ctx, far, media.ProviderDouban, media.Patch{"genres": []string{"美食"}})
	m, _ := store.Get(ctx, base)
	cards, err := store.Similar(ctx, m, 10)
	if err != nil || len(cards) != 1 || cards[0].ID != close1 {
		t.Fatalf("没有向量时应该按类型重合找到《白夜》: %+v err=%v", cards, err)
	}

	// 有向量时按向量距离排序。
	vector := func(x float32) []float32 {
		v := make([]float32, 768)
		v[0], v[1] = x, 1-x
		return v
	}
	store.SetEmbedding(ctx, base, "", vector(1))
	store.SetEmbedding(ctx, close1, "", vector(0.1))
	store.SetEmbedding(ctx, far, "", vector(0.9))
	m, _ = store.Get(ctx, base)
	cards, err = store.Similar(ctx, m, 10)
	if err != nil || len(cards) != 2 || cards[0].ID != far {
		t.Fatalf("有向量时应该按向量距离，最近的是《山海食堂》: %+v err=%v", cards, err)
	}
}

// 同一部剧的不同季是不同的作品，但在 TMDB 和 IMDb 里共用一个 ID：两季都要能记下来。
func TestExternalIDsSharedAcrossSeasons(t *testing.T) {
	store := media.NewStore(testdb.Pool(t))
	ctx := context.Background()
	first := seed(t, store, "30000060", "雾港", 2024, media.TypeTV)
	second := seed(t, store, "30000061", "雾港 第二季", 2026, media.TypeTV)
	for _, id := range []int64{first, second} {
		if err := store.SetExternalID(ctx, id, media.ProviderTMDB, "tv/4455"); err != nil {
			t.Fatal(err)
		}
		if err := store.SetExternalID(ctx, id, media.ProviderIMDb, "tt1234567"); err != nil {
			t.Fatal(err)
		}
	}
	// 已有 ID 时不被覆盖
	store.SetExternalID(ctx, second, media.ProviderTMDB, "tv/9999")
	for _, id := range []int64{first, second} {
		m, err := store.Get(ctx, id)
		if err != nil || m.TMDBID != "tv/4455" || m.IMDbID != "tt1234567" || m.DoubanID == "" {
			t.Errorf("作品 %d 的外部 ID 不对: %+v err=%v", id, m, err)
		}
	}
}

// 发现页的"最热"：热门榜上的按名次排在前面，其余按 TMDB 近期关注度，再按累计评分人数。
// "最新"：按上映日期；没上映的不列；没有日期的按年份排在那一年的最后。
func TestDiscoverSortsByRecentHeatAndReleaseDate(t *testing.T) {
	pool := testdb.Pool(t)
	store := media.NewStore(pool)
	ctx := context.Background()
	now := time.Now()
	day := func(days int) *time.Time { d := now.AddDate(0, 0, days); return &d }
	add := func(doubanID, title string, year int, released *time.Time, popularity float64, votes int) int64 {
		id := seed(t, store, doubanID, title, year, media.TypeMovie)
		if _, err := pool.Exec(ctx, `UPDATE media SET release_date = $2, popularity_tmdb = $3, votes_tmdb = $4 WHERE id = $1`,
			id, released, popularity, votes); err != nil {
			t.Fatal(err)
		}
		return id
	}
	thisYear := now.Year()
	classic := add("40000001", "老经典", 1994, nil, 3, 30000)
	trending := add("40000002", "最近很火", thisYear, day(-40), 900, 200)
	charted := add("40000003", "榜上第一", thisYear, day(-3), 10, 50)
	quiet := add("40000004", "冷门新片", thisYear, day(-1), 1, 5)
	unreleased := add("40000005", "还没上映", thisYear, day(30), 500, 0)
	yearOnly := add("40000006", "只有年份", thisYear-1, nil, 0, 0) // 去年：排在今年所有有日期的后面，不受今天是几月影响
	nextYear := add("40000007", "明年的片", thisYear+1, nil, 0, 0)
	if err := store.ReplaceChart(ctx, media.TypeMovie, []int64{charted}); err != nil {
		t.Fatal(err)
	}
	titles := func(sort string) []int64 {
		cards, err := store.Discover(ctx, media.Filter{Type: media.TypeMovie, Sort: sort})
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]int64, 0, len(cards))
		for _, card := range cards {
			ids = append(ids, card.ID)
		}
		return ids
	}
	if got, want := titles("hot"), []int64{charted, trending, unreleased, classic, quiet, nextYear, yearOnly}; !equalIDs(got, want) {
		t.Errorf("最热的顺序不对:\n got %v\nwant %v", got, want)
	}
	if got := titles(""); !equalIDs(got, titles("hot")) {
		t.Errorf("没给排序时应该和最热一样: %v", got)
	}
	if got, want := titles("new"), []int64{quiet, charted, trending, yearOnly, classic}; !equalIDs(got, want) {
		t.Errorf("最新的顺序不对（没上映的和明年的不该出现）:\n got %v\nwant %v", got, want)
	}
	// 全部类型时也能按榜单排（几类的榜单名次放在一起比）
	if cards, err := store.Discover(ctx, media.Filter{Sort: "hot"}); err != nil || len(cards) == 0 || cards[0].ID != charted {
		t.Errorf("全部类型的最热应该以榜上的开头: %v %v", cards, err)
	}
}

// 补日期的进度：查过的不再挑中；日期和关注度为空时保留原值。批量更新关注度按 TMDB 编号对上各季。
func TestReleaseAndPopularityUpdates(t *testing.T) {
	pool := testdb.Pool(t)
	store := media.NewStore(pool)
	ctx := context.Background()
	movie := seed(t, store, "41000001", "电影", 2020, media.TypeMovie)
	season1 := seed(t, store, "41000002", "某剧", 2021, media.TypeTV)
	season2 := seed(t, store, "41000003", "某剧 第二季", 2023, media.TypeTV)
	noTMDB := seed(t, store, "41000004", "没有 TMDB", 2020, media.TypeMovie)
	pool.Exec(ctx, `UPDATE media SET tmdb_id = 'movie/603' WHERE id = $1`, movie)
	pool.Exec(ctx, `UPDATE media SET tmdb_id = 'tv/77' WHERE id = ANY($1)`, []int64{season1, season2})

	pending, err := store.NeedsRelease(ctx, "media.release", 10)
	if err != nil || !equalIDs(pending, []int64{movie, season1, season2}) {
		t.Fatalf("应该挑出有 TMDB 编号、没查过的: %v %v", pending, err)
	}
	released := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := store.SetRelease(ctx, movie, &released, 12.5); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRelease(ctx, movie, nil, 0); err != nil {
		t.Fatal(err)
	}
	var date time.Time
	var popularity float64
	pool.QueryRow(ctx, `SELECT release_date, popularity_tmdb FROM media WHERE id = $1`, movie).Scan(&date, &popularity)
	if !date.Equal(released) || popularity != 12.5 {
		t.Errorf("空值不该清掉已有的日期和关注度: %v %v", date, popularity)
	}
	if pending, _ := store.NeedsRelease(ctx, "media.release", 10); !equalIDs(pending, []int64{season1, season2}) {
		t.Errorf("查过的不该再挑中: %v", pending)
	}

	index, err := store.TMDBIndex(ctx)
	if err != nil || len(index["tv/77"]) != 2 || len(index["movie/603"]) != 1 || len(index) != 2 {
		t.Fatalf("TMDB 编号索引不对: %v %v", index, err)
	}
	if err := store.SetPopularity(ctx, []int64{season1, season2, movie}, []float64{80, 80, 7}); err != nil {
		t.Fatal(err)
	}
	var values []float64
	pool.QueryRow(ctx, `SELECT array_agg(popularity_tmdb::float8 ORDER BY id) FROM media WHERE id = ANY($1)`,
		[]int64{movie, season1, season2, noTMDB}).Scan(&values)
	if len(values) != 4 || values[0] != 7 || values[1] != 80 || values[2] != 80 || values[3] != 0 {
		t.Errorf("批量更新关注度不对: %v", values)
	}
}

func equalIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

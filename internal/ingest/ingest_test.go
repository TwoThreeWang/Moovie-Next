package ingest_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"moovie/internal/db/testdb"
	"moovie/internal/ingest"
	"moovie/internal/jobs"
	"moovie/internal/media"
	"moovie/internal/providers"
	"moovie/internal/providers/providerstest"
)

func newService(t *testing.T) (*ingest.Service, *providerstest.Fake) {
	pool := testdb.Pool(t)
	fake := providerstest.New(t)
	return &ingest.Service{
		Media: media.NewStore(pool), Queue: jobs.NewQueue(pool), Douban: fake.Douban(), Wikidata: fake.WikidataClient(),
		TMDB: fake.TMDBClient(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, fake
}

func seedShow(fake *providerstest.Fake) {
	fake.Subjects["tv:36000001"] = map[string]any{
		"id": "36000001", "title": "雾港 第二季", "aka": []string{"雾港2"}, "year": "2026", "intro": "豆瓣的简介",
		"genres": []string{"悬疑", "犯罪"}, "countries": []string{"中国大陆"}, "episodes_count": 12, "rating": map[string]any{"value": 8.9},
		"pic":       map[string]any{"large": "https://img9.doubanio.com/view/photo/l_ratio_poster/public/p1.jpg"},
		"directors": []map[string]any{{"name": "陈默"}}, "actors": []map[string]any{{"name": "周冬雨"}},
	}
	fake.Wikidata["36000001"] = []map[string]string{{"item": "http://www.wikidata.org/entity/Q123", "imdb": "tt1234567", "tmdbTV": "4455", "label": "Fog Harbor"}}
	fake.TMDB["/3/tv/4455"] = map[string]any{
		"name": "雾港", "original_name": "Fog Harbor", "overview": "TMDB 的简介", "poster_path": "/tmdb-poster.jpg",
		"vote_average": 8.1, "vote_count": 321, "episode_run_time": []int{45}, "status": "Returning Series",
		"first_air_date": "2024-03-01", "popularity": 66.6,
		"seasons": []map[string]any{{"season_number": 1, "episode_count": 12}, {"season_number": 2, "episode_count": 10, "air_date": "2026-10-08"}},
		"images":  map[string]any{"backdrops": []map[string]any{{"file_path": "/b1.jpg"}}},
	}
	fake.TMDB["/3/tv/4455/season/2"] = map[string]any{"episodes": []map[string]any{
		{"episode_number": 1, "name": "雾起", "air_date": "2026-10-08", "runtime": 46}, {"episode_number": 2, "name": "旧码头", "air_date": "2026-10-15"},
	}}
}

// 完整的采集流程：豆瓣是主要来源，Wikidata 提供外部 ID，TMDB 补充剧照和分集。
func TestRefreshMergesAllSources(t *testing.T) {
	service, fake := newService(t)
	seedShow(fake)
	ctx := context.Background()
	id, err := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000001", Title: "雾港 第二季", Kind: "tv"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Refresh(ctx, id, jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	if err := service.Enrich(ctx, id, jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	m, err := service.Media.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "雾港 第二季" || m.Type != media.TypeTV || m.Season != 2 || m.Year != 2026 || m.MetadataStatus != "complete" {
		t.Errorf("豆瓣的基本信息没有写入: %+v", m)
	}
	// 豆瓣给过的字段不被 TMDB 覆盖；豆瓣没有的字段用 TMDB 的。
	if m.Summary != "豆瓣的简介" || m.Poster != "https://img9.doubanio.com/view/photo/l_ratio_poster/public/p1.jpg" {
		t.Errorf("豆瓣的简介和海报不应该被 TMDB 覆盖: %q %q", m.Summary, m.Poster)
	}
	if m.EpisodeCount != 10 {
		t.Errorf("确认同一季后，集数应该使用 TMDB 的 10，实际 %d", m.EpisodeCount)
	}
	if m.OriginalTitle != "Fog Harbor" || len(m.Backdrops) != 1 || m.RatingTMDB < 8 || m.DurationMinutes != 45 || m.SeriesStatus != "ongoing" {
		t.Errorf("TMDB 的补充信息没有写入: %+v", m)
	}
	if m.IMDbID != "tt1234567" || m.TMDBID != "tv/4455" || m.WikidataID != "Q123" || m.DoubanID != "36000001" {
		t.Errorf("外部 ID 不对: %+v", m)
	}
	units, _ := service.Media.Units(ctx, id)
	if len(units) != 2 || units[0].Season != 2 || units[0].Title != "雾起" || units[0].AirDate == nil {
		t.Errorf("应该写入第二季的分集: %+v", units)
	}
	// 别名来自豆瓣的又名、Wikidata 的标签和 TMDB 的片名，都能用来匹配。
	for _, title := range []string{"雾港2", "Fog Harbor", "雾港 第二季"} {
		results, _ := service.Media.Resolve(ctx, []media.Candidate{{Title: title}})
		if results[0].Status != "matched" {
			t.Errorf("应该能用别名 %q 匹配到作品", title)
		}
	}
}

func TestRefreshMarksMissingDoubanSubject(t *testing.T) {
	service, fake := newService(t)
	ctx := context.Background()
	id, err := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36009999"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Refresh(ctx, id, jobs.Job{}); err != nil {
		t.Fatalf("豆瓣没有这个条目不算任务失败: %v", err)
	}
	m, _ := service.Media.Get(ctx, id)
	if m.MetadataStatus != "not_found" || !m.Hidden {
		t.Errorf("应该标记为 not_found 并隐藏: %+v", m)
	}
	if stale, _ := service.Media.Stale(ctx, ingest.KindRefresh, 10); len(stale) != 0 {
		t.Errorf("not_found 的作品不应该再被安排同步: %v", stale)
	}
	// 再从任何入口送来这个 ID，都不会再排任务去请求豆瓣。
	testdb.Pool(t).Exec(ctx, `DELETE FROM jobs`)
	if again, _ := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36009999"}, "", ingest.PriorityViewing); again != id {
		t.Fatal("应该还是原来那一行")
	}
	if counts, _ := service.Queue.Counts(ctx); counts["queued"] != 0 {
		t.Errorf("豆瓣确认没有的 ID 不应该再排队: %v", counts)
	}
	// 后来豆瓣上又有了：自动的任务不会再去问，管理员手动刷新（Force）才会，问到了就恢复显示。
	fake.Subjects["tv:36009999"] = map[string]any{"id": "36009999", "type": "tv", "title": "迟到的剧"}
	if err := service.Refresh(ctx, id, jobs.Job{}); !errors.Is(err, jobs.ErrSkip) {
		t.Fatalf("确认没有的条目，自动的任务不应该再请求豆瓣: %v", err)
	}
	if err := service.Refresh(ctx, id, jobs.Job{Payload: []byte(`{"force":true}`)}); err != nil {
		t.Fatal(err)
	}
	if m, _ := service.Media.Get(ctx, id); m.Hidden || m.MetadataStatus != "complete" || m.Title != "迟到的剧" {
		t.Errorf("豆瓣又有了这个条目，应该恢复显示: %+v", m)
	}
}

func TestRefreshWorksWithoutOptionalSources(t *testing.T) {
	service, fake := newService(t)
	seedShow(fake)
	service.Wikidata, service.TMDB = nil, &providers.TMDB{}
	ctx := context.Background()
	id, _ := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000001"}, "", 0)
	if err := service.Refresh(ctx, id, jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	if err := service.Enrich(ctx, id, jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	m, _ := service.Media.Get(ctx, id)
	if m.Title != "雾港 第二季" || m.MetadataStatus != "complete" || len(m.Backdrops) != 0 || m.EnrichedAt == nil {
		t.Errorf("只有豆瓣时也应该完成同步: %+v", m)
	}
}

func TestEnsureDoubanQueuesRefreshOnce(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	first, err := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000002", Title: "回声", Year: 2024, Kind: "movie"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000002", Title: "回声"}, "", 0)
	if first != second {
		t.Error("同一个豆瓣 ID 应该对应同一部作品")
	}
	counts, _ := service.Queue.Counts(ctx)
	_, refresh, _ := service.Queue.Latest(ctx, ingest.KindRefresh, strconv.FormatInt(first, 10))
	_, enrich, _ := service.Queue.Latest(ctx, ingest.KindEnrich, strconv.FormatInt(first, 10))
	if counts["queued"] != 2 || !refresh || !enrich {
		t.Errorf("只有新建时才排队：建档和豆瓣详情各一个，实际 %v", counts)
	}
	m, _ := service.Media.Get(ctx, first)
	if m.Title != "回声" || m.Year != 2024 || m.Type != media.TypeMovie {
		t.Errorf("新建时应该先填入已知的基本信息: %+v", m)
	}
}

// 榜单全都因为豆瓣暂停而失败时，错误里还认得出限流和要等多久：任务队列推迟到恢复，不算一次失败。
func TestRefreshChartsKeepsThrottleErrors(t *testing.T) {
	service, _ := newService(t)
	service.Douban.Pace = providers.NewPacer(time.Millisecond)
	service.Douban.Pace.Flagged()
	err := service.RefreshCharts(context.Background())
	var paused *providers.PausedError
	if !errors.Is(err, providers.ErrThrottled) || !errors.As(err, &paused) || paused.RetryAfter() < time.Hour {
		t.Fatalf("榜单的错误里应该还认得出限流和要等多久: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "所有榜单都刷新失败: movie: ") {
		t.Errorf("错误文字应该和原来一样: %s", err)
	}
}

// 只凭豆瓣 ID 收进来的条目不知道是电影还是剧集：电影详情暂停着时，是剧集的照样能从剧集详情读到。
func TestRefreshUnknownTypeDoesNotWaitForMovies(t *testing.T) {
	service, fake := newService(t)
	seedShow(fake)
	service.Douban.MoviePace, service.Douban.TVPace = providers.NewPacer(time.Millisecond), providers.NewPacer(time.Millisecond)
	service.Douban.MoviePace.Flagged()
	ctx := context.Background()
	id, err := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000001"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Refresh(ctx, id, jobs.Job{}); err != nil {
		t.Fatalf("电影详情暂停时，不知道类型的剧集应该照样能刷新: %v", err)
	}
	if m, _ := service.Media.Get(ctx, id); m.Title != "雾港 第二季" || m.Type != media.TypeTV {
		t.Errorf("应该读到剧集的资料: %+v", m)
	}
}

func TestRefreshKnownMovieWaitsForMovies(t *testing.T) {
	service, fake := newService(t)
	service.Douban.MoviePace = providers.NewPacer(time.Millisecond)
	service.Douban.MoviePace.Flagged()
	ctx := context.Background()
	id, err := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000003", Title: "新电影", Kind: "movie"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Refresh(ctx, id, jobs.Job{}); !errors.Is(err, providers.ErrThrottled) {
		t.Fatalf("明确是电影时应等待电影接口恢复: %v", err)
	}
	if fake.Count("/rexxar/api/v2/tv/36000003") != 0 {
		t.Fatalf("电影接口暂停时不应误查剧集接口: %v", fake.Requests)
	}
}

func TestRefreshChartsAndDiscover(t *testing.T) {
	service, fake := newService(t)
	fake.Popular["hot_gaia"] = []map[string]any{{"id": "1000001", "title": "电影一", "year": "2026"}, {"id": "1000002", "title": "电影二"}}
	fake.Popular["tv_animation"] = []map[string]any{{"id": "3000001", "title": "动漫一"}}
	ctx := context.Background()
	// 剧集榜和综艺榜的接口失败，电影榜和动漫榜成功：整体不算失败。
	if err := service.RefreshCharts(ctx); err != nil {
		t.Fatal(err)
	}
	movies, _ := service.Media.Chart(ctx, media.TypeMovie, 10)
	anime, _ := service.Media.Chart(ctx, media.TypeAnime, 10)
	if len(movies) != 2 || movies[0].Title != "电影一" || len(anime) != 1 || anime[0].Type != media.TypeAnime {
		t.Fatalf("榜单不对: %+v %+v", movies, anime)
	}

	fake.Suggest["雾港"] = []map[string]any{{"id": "36000001", "title": "雾港", "type": "tv", "year": "2026"}}
	if err := service.Discover(ctx, "雾港"); err != nil {
		t.Fatal(err)
	}
	if cards, _ := service.Media.Search(ctx, "雾港", 10); len(cards) != 1 {
		t.Errorf("联想到的条目应该进入作品库，实际 %d 条", len(cards))
	}
	before := len(fake.Requests)
	service.Discover(ctx, "雾港")
	if len(fake.Requests) != before {
		t.Error("同一个关键词在冷却期内不应该再请求豆瓣")
	}
}

// 只为豆瓣独有、还会变的资料定期重新读详情：近两年作品的评分和简介。连载剧的集数和状态交给 TMDB，不为它们去读豆瓣。
func TestStaleOnlyPicksWorksThatStillChange(t *testing.T) {
	service, _ := newService(t)
	pool := testdb.Pool(t)
	ctx := context.Background()
	thisYear := time.Now().Year()
	add := func(doubanID, mediaType, seriesStatus, status string, year int, syncedDaysAgo float64) int64 {
		id, _, err := service.Media.EnsureDouban(ctx, doubanID, media.Patch{"title": doubanID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE media SET media_type = $2, series_status = $3, metadata_status = $4, year = $5,
metadata_synced_at = now() - make_interval(secs => $6) WHERE id = $1`, id, mediaType, seriesStatus, status, year, syncedDaysAgo*86400); err != nil {
			t.Fatal(err)
		}
		return id
	}
	never, _, _ := service.Media.EnsureDouban(ctx, "37000001", nil)
	want := map[int64]string{
		never: "从没同步过",
		add("37000003", "tv", "ongoing", "complete", thisYear, 8): "今年的连载剧，每周一次",
		add("37000004", "movie", "", "complete", thisYear, 8):     "今年的电影，每周一次",
		add("37000005", "tv", "ended", "complete", thisYear-1, 8): "去年完结的剧，评分还在变，每周一次",
	}
	skip := map[int64]string{
		add("37000002", "tv", "ongoing", "complete", 1999, 2):     "连载中的老剧：集数交给 TMDB",
		add("37000011", "tv", "ongoing", "complete", thisYear, 2): "今年的连载剧，两天前才同步过",
		add("37000012", "tv", "", "complete", 2015, 400):          "状态未知的老剧",
		add("37000013", "tv", "ended", "complete", 2015, 400):     "完结多年的剧",
		add("37000014", "movie", "", "complete", 1994, 400):       "老电影",
		add("37000015", "movie", "", "complete", thisYear, 3):     "今年的电影但三天前才同步过",
		add("37000017", "movie", "", "not_found", thisYear, 400):  "豆瓣上没有的",
	}
	// 这一周已经从列表里拿到过评分的近期作品，详情放宽到 30 天：电影、剧集都一样。
	listed := func(id int64) int64 {
		pool.Exec(ctx, `UPDATE media SET douban_listed_at = now() - interval '1 day' WHERE id = $1`, id)
		return id
	}
	skip[listed(add("37000019", "movie", "", "complete", thisYear, 8))] = "今年的电影，这周列表里拿到过评分"
	want[listed(add("37000020", "movie", "", "complete", thisYear, 31))] = "列表里拿到过评分，但详情已经一个月没抓"
	skip[listed(add("37000021", "tv", "ended", "complete", thisYear-1, 8))] = "去年完结的剧，这周列表里拿到过评分"
	stalelisted := add("37000022", "movie", "", "complete", thisYear, 8)
	pool.Exec(ctx, `UPDATE media SET douban_listed_at = now() - interval '8 days' WHERE id = $1`, stalelisted)
	want[stalelisted] = "列表里拿到评分已经是一周多以前"
	cooling := add("37000018", "movie", "", "complete", thisYear, 8)
	pool.Exec(ctx, `INSERT INTO jobs (kind, subject, status, finished_at) VALUES ($1, $2, 'failed', now())`, ingest.KindRefresh, strconv.FormatInt(cooling, 10))
	skip[cooling] = "重试用完失败了、还在冷却期的"
	stale, err := service.Media.Stale(ctx, ingest.KindRefresh, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, id := range stale {
		got[id] = true
		if reason, bad := skip[id]; bad {
			t.Errorf("不应该重新同步：%s", reason)
		}
	}
	for id, reason := range want {
		if !got[id] {
			t.Errorf("应该重新同步：%s", reason)
		}
	}
	if len(stale) == 0 || stale[0] != never {
		t.Errorf("从没同步过的应该排在最前面: %v", stale)
	}
}

// runAll 用 Worker 把队列里的任务全部跑完（失败重试的不等，拨回时间接着跑），最多跑 limit 个。
func runAll(t *testing.T, service *ingest.Service, limit int) {
	t.Helper()
	worker := jobs.NewWorker(service.Queue, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service.Register(worker)
	pool := testdb.Pool(t)
	for range limit {
		pool.Exec(context.Background(), `UPDATE jobs SET run_at = now() WHERE status = 'queued'`)
		if worked, err := worker.RunOne(context.Background()); err != nil {
			t.Fatal(err)
		} else if !worked {
			return
		}
	}
}

// 整条链：建档（Wikidata/NeoDB/TMDB）和豆瓣详情 → AI 写描述 → 算向量。每一步成功就保存，下一步失败不会让上一步重来。
func TestSemanticAndEmbeddingAreSavedSeparately(t *testing.T) {
	service, fake := newService(t)
	seedShow(fake)
	service.AI, service.Embedder = fake.AI(), fake.Embedder()
	ctx := context.Background()
	id, _ := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000001"}, "", ingest.PriorityViewing)

	// 向量模型出故障：AI 写的描述照样保存，向量任务稍后重试。
	fake.EmbedDown = true
	runAll(t, service, 4) // 豆瓣、补充资料、写描述各一次，算向量失败一次
	m, _ := service.Media.Get(ctx, id)
	if !strings.HasPrefix(m.SemanticText, "《雾港 第二季》") || m.HasEmbedding || m.SemanticHash != m.InputHash() {
		t.Fatalf("AI 的描述应该已经保存，向量还没有: %+v", m)
	}
	if len(fake.Prompts) != 1 || !strings.Contains(fake.Prompts[0], m.SemanticInput()) || !strings.Contains(fake.Prompts[0], "极高语义密度") {
		t.Fatalf("应该用老系统的提示词和资料格式调用一次 AI: %v", fake.Prompts)
	}
	if !strings.Contains(m.SemanticInput(), "原名：Fog Harbor") {
		t.Errorf("应该等 TMDB 补完原名再写描述: %s", m.SemanticInput())
	}

	// 向量恢复后重试：只算向量，不再调用 AI。
	fake.EmbedDown = false
	runAll(t, service, 20)
	m, _ = service.Media.Get(ctx, id)
	if !m.HasEmbedding || len(fake.Prompts) != 1 {
		t.Fatalf("应该只补上向量、不再调用 AI: 有向量=%v AI 调用 %d 次", m.HasEmbedding, len(fake.Prompts))
	}

	// 再同步一遍、资料没变：不调用 AI，也不重算向量。
	embeds := fake.Count("/v1/embeddings")
	service.RefreshNow(ctx, id)
	runAll(t, service, 20)
	if len(fake.Prompts) != 1 || fake.Count("/v1/embeddings") != embeds {
		t.Errorf("资料没变不应该重写描述、重算向量: AI %d 次，向量 %d → %d 次", len(fake.Prompts), embeds, fake.Count("/v1/embeddings"))
	}

	// 豆瓣的简介改了：重写描述，旧向量作废重算。
	fake.Subjects["tv:36000001"]["intro"] = "改过的简介"
	service.RefreshNow(ctx, id)
	runAll(t, service, 20)
	m, _ = service.Media.Get(ctx, id)
	if len(fake.Prompts) != 2 || !strings.Contains(m.SemanticText, "第 2 次") || !m.HasEmbedding || fake.Count("/v1/embeddings") != embeds+1 {
		t.Errorf("资料变了应该重写描述并重算向量: AI %d 次 %q", len(fake.Prompts), m.SemanticText)
	}
}

// Wikidata、TMDB 出错只重试这一步，不会再去请求豆瓣；重试用完照样往下写描述，过了冷却期由巡检再补。
func TestEnrichFailureRetriesWithoutDouban(t *testing.T) {
	service, fake := newService(t)
	seedShow(fake)
	service.AI = fake.AI()
	pool := testdb.Pool(t)
	ctx := context.Background()
	fake.TMDB["/3/tv/4455"] = "坏掉的响应" // TMDB 一直出错（不是"没有这个条目"）
	id, _ := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000001"}, "", 0)
	runAll(t, service, 30)

	if n := fake.Count("/rexxar/api/v2/tv/36000001"); n != 1 {
		t.Errorf("TMDB 出错不应该让豆瓣再被请求，实际请求了 %d 次", n)
	}
	var attempts int
	pool.QueryRow(ctx, `SELECT attempts FROM jobs WHERE kind = $1 AND status = 'failed'`, ingest.KindEnrich).Scan(&attempts)
	if attempts != jobs.MaxAttempts {
		t.Errorf("补充资料应该重试到用完 %d 次，实际 %d 次", jobs.MaxAttempts, attempts)
	}
	m, _ := service.Media.Get(ctx, id)
	if m.EnrichedAt != nil || m.SemanticText == "" || m.IMDbID != "tt1234567" {
		t.Errorf("TMDB 没补上，但 Wikidata 的 ID 应该已经保存，描述也应该照样写了: %+v", m)
	}
	if picked, _ := service.Media.NeedsEnrich(ctx, ingest.KindEnrich, 10); len(picked) != 0 {
		t.Errorf("冷却期内巡检不应该再挑它: %v", picked)
	}
	pool.Exec(ctx, `DELETE FROM jobs WHERE status = 'failed'`) // 失败记录保留 14 天，过期后……
	if picked, _ := service.Media.NeedsEnrich(ctx, ingest.KindEnrich, 10); len(picked) != 1 {
		t.Errorf("……冷却期过了，巡检应该再补一次: %v", picked)
	}
}

// 巡检把每一步漏掉的补上。
func TestSweepFillsGaps(t *testing.T) {
	service, fake := newService(t)
	seedShow(fake)
	service.AI, service.Embedder = fake.AI(), fake.Embedder()
	pool := testdb.Pool(t)
	ctx := context.Background()
	id, _ := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000001"}, "", 0)
	runAll(t, service, 20)
	// 假装描述和向量丢了（比如那时还没配 AI）。
	pool.Exec(ctx, `UPDATE media SET semantic_text = '', semantic_hash = '', embedding = NULL WHERE id = $1`, id)
	pool.Exec(ctx, `DELETE FROM jobs`)
	if err := service.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	runAll(t, service, 20)
	if m, _ := service.Media.Get(ctx, id); m.SemanticText == "" || !m.HasEmbedding {
		t.Errorf("巡检应该补上描述和向量: %+v", m)
	}
}

// 热门榜综合三个来源：同时出现在豆瓣和 TMDB 里的排到前面；本站在看的人够多才计入。
func TestChartsBlendSources(t *testing.T) {
	service, fake := newService(t)
	pool := testdb.Pool(t)
	ctx := context.Background()
	var douban []map[string]any
	for i := 1; i <= 10; i++ {
		douban = append(douban, map[string]any{"id": strconv.Itoa(1000000 + i), "title": "豆瓣第" + strconv.Itoa(i), "type": "movie"})
	}
	fake.Popular["hot_gaia"] = douban
	fake.TMDB["/3/trending/movie/week"] = map[string]any{"results": []map[string]any{{"id": 900}, {"id": 901}}}
	movie := func(doubanID, title string) int64 {
		id, _, err := service.Media.EnsureDouban(ctx, doubanID, media.Patch{"title": title, "media_type": media.TypeMovie})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	tmdbOnly := movie("2000001", "只在 TMDB 趋势里")
	siteHot := movie("2000002", "本站很多人在看")
	pool.Exec(ctx, `UPDATE media SET tmdb_id = 'movie/900' WHERE id = $1`, tmdbOnly)
	watch := func(users int) {
		pool.Exec(ctx, `DELETE FROM playback_positions; DELETE FROM watch_days`)
		for user := int64(1); user <= int64(users); user++ {
			testdb.User(t, pool, user)
			pool.Exec(ctx, `INSERT INTO playback_positions (user_id, media_id, activity_at) VALUES ($1, $2, now())`, user, siteHot)
			pool.Exec(ctx, `INSERT INTO watch_days (user_id, day) VALUES ($1, current_date)`, user)
		}
	}
	chart := func() []string {
		if err := service.RefreshCharts(ctx); err != nil {
			t.Fatal(err)
		}
		cards, _ := service.Media.Chart(ctx, media.TypeMovie, 50)
		titles := []string{}
		for _, card := range cards {
			titles = append(titles, card.Title)
		}
		return titles
	}

	// 本站只有 5 个人：本站的数据不计入。豆瓣第 10 名同时在 TMDB 趋势第 2 名，排到第一。
	watch(5)
	fake.Popular["hot_gaia"] = douban
	first := chart()
	pool.Exec(ctx, `UPDATE media SET tmdb_id = 'movie/901' WHERE douban_id = '1000010'`)
	titles := chart()
	if titles[0] != "豆瓣第10" || titles[1] != "豆瓣第1" || titles[len(titles)-1] != "只在 TMDB 趋势里" || strings.Contains(strings.Join(titles, ","), "本站很多人在看") {
		t.Fatalf("本站人少时只综合豆瓣和 TMDB: %v（第一次：%v）", titles, first)
	}

	// 本站有 25 个人在看：计入。只在本站热播的排在豆瓣那些之后、只在 TMDB 里的之前。
	watch(25)
	titles = chart()
	joined := strings.Join(titles, ",")
	if !strings.Contains(joined, "豆瓣第9,本站很多人在看,只在 TMDB 趋势里") {
		t.Errorf("本站人够多时应该计入本站热播: %v", titles)
	}

	// 豆瓣取不到：这一类保持原样，不只靠另外两个来源重排。
	delete(fake.Popular, "hot_gaia")
	service.RefreshCharts(ctx)
	if after, _ := service.Media.Chart(ctx, media.TypeMovie, 50); len(after) != len(titles) || after[0].Title != titles[0] {
		t.Errorf("豆瓣取不到时榜单应该保持原样: %d 条", len(after))
	}
}

// 补资料时一并记下这一季的首播日期和 TMDB 近期关注度。
func TestEnrichRecordsSeasonReleaseDate(t *testing.T) {
	service, fake := newService(t)
	seedShow(fake)
	pool := testdb.Pool(t)
	ctx := context.Background()
	id, err := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000001", Title: "雾港 第二季", Kind: "tv"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Enrich(ctx, id, jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	m, err := service.Media.Get(ctx, id)
	if err != nil || m.Year != 2026 {
		t.Fatalf("尚未同步豆瓣时，应按第二季首播时间补年份: %+v, %v", m, err)
	}
	var released string
	var popularity float64
	var checked bool
	if err := pool.QueryRow(ctx, `SELECT release_date::text, popularity_tmdb, release_checked FROM media WHERE id = $1`, id).
		Scan(&released, &popularity, &checked); err != nil {
		t.Fatal(err)
	}
	if released != "2026-10-08" || popularity < 66 || popularity > 67 || !checked {
		t.Errorf("第二季应该记下这一季的首播日期: %s %v %v", released, popularity, checked)
	}
}

// 补日期的任务：按已知的 TMDB 编号读一次详情；TMDB 上没有的也记为查过；巡检会把没查过的排进队列。
func TestReleaseBackfill(t *testing.T) {
	service, fake := newService(t)
	pool := testdb.Pool(t)
	ctx := context.Background()
	fake.TMDB["/3/movie/603"] = map[string]any{"title": "黑客帝国", "release_date": "1999-03-31", "popularity": 41.2}
	add := func(doubanID, tmdbID string) int64 {
		id, _, err := service.Media.EnsureDouban(ctx, doubanID, media.Patch{"title": doubanID})
		if err != nil {
			t.Fatal(err)
		}
		pool.Exec(ctx, `UPDATE media SET tmdb_id = $2, media_type = 'movie' WHERE id = $1`, id, tmdbID)
		return id
	}
	found := add("42000001", "movie/603")
	missing := add("42000002", "movie/999")

	if err := service.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var queued int
	pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind = $1`, ingest.KindRelease).Scan(&queued)
	if queued != 2 {
		t.Errorf("巡检应该把两部没查过日期的排进队列，实际 %d", queued)
	}

	for _, id := range []int64{found, missing} {
		if err := service.Release(ctx, id, jobs.Job{}); err != nil {
			t.Fatal(err)
		}
	}
	var released *string
	var checked bool
	pool.QueryRow(ctx, `SELECT release_date::text, release_checked FROM media WHERE id = $1`, found).Scan(&released, &checked)
	if released == nil || *released != "1999-03-31" || !checked {
		t.Errorf("应该补上上映日期: %v %v", released, checked)
	}
	pool.QueryRow(ctx, `SELECT release_date::text, release_checked FROM media WHERE id = $1`, missing).Scan(&released, &checked)
	if released != nil || !checked {
		t.Errorf("TMDB 上没有的应该记为查过、日期留空: %v %v", released, checked)
	}
	if pending, _ := service.Media.NeedsRelease(ctx, ingest.KindRelease, 10); len(pending) != 0 {
		t.Errorf("都查过了，不该再挑中: %v", pending)
	}
}

// 每天的关注度：当天的导出文件还没生成时用前一天的；只更新作品库里有的 TMDB 条目，剧集的几季都更新。
func TestRefreshPopularityFromExports(t *testing.T) {
	service, fake := newService(t)
	pool := testdb.Pool(t)
	ctx := context.Background()
	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	name := func(prefix string) string {
		return prefix + "_" + yesterday.Format("01_02_2006") + ".json.gz"
	}
	fake.Exports[name("movie_ids")] = []map[string]any{{"id": 603, "popularity": 41.2}, {"id": 1, "popularity": 999}}
	fake.Exports[name("tv_series_ids")] = []map[string]any{{"id": 77, "popularity": 9.5}}
	ids := map[string]int64{}
	for doubanID, tmdbID := range map[string]string{"43000001": "movie/603", "43000002": "tv/77", "43000003": "tv/77"} {
		id, _, err := service.Media.EnsureDouban(ctx, doubanID, media.Patch{"title": doubanID})
		if err != nil {
			t.Fatal(err)
		}
		pool.Exec(ctx, `UPDATE media SET tmdb_id = $2 WHERE id = $1`, id, tmdbID)
		ids[doubanID] = id
	}
	if err := service.RefreshPopularity(ctx); err != nil {
		t.Fatal(err)
	}
	for doubanID, want := range map[string]float64{"43000001": 41.2, "43000002": 9.5, "43000003": 9.5} {
		var got float64
		pool.QueryRow(ctx, `SELECT popularity_tmdb::float8 FROM media WHERE id = $1`, ids[doubanID]).Scan(&got)
		if got < want-0.01 || got > want+0.01 {
			t.Errorf("%s 的关注度应该是 %v，实际 %v", doubanID, want, got)
		}
	}
	delete(fake.Exports, name("tv_series_ids"))
	if err := service.RefreshPopularity(ctx); err == nil {
		t.Error("连前一天的文件都没有时应该报错，下次再试")
	}
}

// 榜单、标记列表里已有的作品：评分直接更新，年份和海报只补空着的。
func TestListingUpdatesExistingWorks(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	id, _, err := service.Media.EnsureDouban(ctx, "36000001", media.Patch{"title": "雾港", "poster": "https://img9.doubanio.com/l.jpg", "rating_douban": 7.5})
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000001", Title: "雾港", Rating: 8.6, Year: 2026, Poster: "https://img9.doubanio.com/s.jpg"}, "", ingest.PriorityDiscover)
	if err != nil || again != id {
		t.Fatalf("应该找到已有的作品: %d %v", again, err)
	}
	m, err := service.Media.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.RatingDouban < 8.59 || m.RatingDouban > 8.61 || m.Year != 2026 || m.Poster != "https://img9.doubanio.com/l.jpg" {
		t.Errorf("评分应该更新、年份补上、海报不被列表里的小图覆盖: %+v", m)
	}
	// 列表里没有评分（比如网页版热门榜的新片）时不清掉已有的评分。
	if _, err := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000001", Title: "雾港"}, "", ingest.PriorityDiscover); err != nil {
		t.Fatal(err)
	}
	if m, _ := service.Media.Get(ctx, id); m.RatingDouban < 8.59 {
		t.Errorf("没有评分的列表不该清掉已有的评分: %v", m.RatingDouban)
	}
}

// 详情接口拒绝了这个条目：有人在等的改用详情网页，后台批量的照常失败、稍后重试。
func TestRefreshFallsBackToPageWhenSomeoneWaits(t *testing.T) {
	service, fake := newService(t)
	service.Wikidata, service.TMDB = nil, nil
	page, err := os.ReadFile("../providers/testdata/subject_tv.html")
	if err != nil {
		t.Fatal(err)
	}
	fake.Refuse["35588177"] = true
	fake.Pages["35588177"] = string(page)
	ctx := context.Background()
	id, err := service.EnsureDouban(ctx, providers.DoubanItem{ID: "35588177", Title: "漫长的季节", Kind: "tv"}, "", ingest.PriorityImport)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Refresh(ctx, id, jobs.Job{Priority: ingest.PriorityRoutine}); err == nil {
		t.Fatal("后台任务不该用详情网页兜底")
	}
	if fake.Count("/subject/35588177/") != 0 {
		t.Error("后台任务不该打开详情网页")
	}
	if err := service.Refresh(ctx, id, jobs.Job{Priority: ingest.PriorityViewing}); err != nil {
		t.Fatal(err)
	}
	m, err := service.Media.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "漫长的季节" || m.EpisodeCount != 12 || m.RatingDouban < 9.39 || m.Year != 2023 {
		t.Errorf("应该用详情网页的资料: %+v", m)
	}
}

// 豆瓣详情一直拿不到（接口全拒绝）时，新作品照样建档：Wikidata 给片名和编号，NeoDB 给中文简介、类型和演职员，
// TMDB 给剧照和其余空着的字段；几个来源按 豆瓣 > NeoDB > TMDB 取舍，NeoDB 的评分不入库。
// 豆瓣详情后来到了，覆盖建档的资料，NeoDB 不再被请求。
func TestBuildsWithoutDouban(t *testing.T) {
	service, fake := newService(t)
	service.NeoDB, service.AI = fake.NeoDBClient(), fake.AI()
	ctx := context.Background()
	fake.Refuse["36100001"] = true
	fake.Wikidata["36100001"] = []map[string]string{
		{"item": "http://www.wikidata.org/entity/Q900", "imdb": "tt9000001", "tmdbMovie": "9001", "label": "Silent Witness", "lang": "en"},
		{"item": "http://www.wikidata.org/entity/Q900", "label": "无声的证词", "lang": "zh-cn"},
	}
	neodbEntry := func(doubanID, description string) map[string]any {
		return map[string]any{
			"type": "Movie", "title": "Silent Witness", "orig_title": "Silent Witness", "rating": 9.9, "description": "An English brief",
			"localized_title":       []map[string]string{{"lang": "zh-cn", "text": "沉默的证人"}, {"lang": "zh-tw", "text": "沉默證人"}},
			"localized_description": []map[string]string{{"lang": "en", "text": "An English brief"}, {"lang": "zh-cn", "text": description}},
			"genre":                 []string{"drama", "犯罪", "unknown"}, "director": []string{"导演乙"}, "actor": []map[string]string{{"name": "演员丙", "role": ""}},
			"external_resources": []map[string]string{{"url": "https://movie.douban.com/subject/" + doubanID + "/"},
				{"url": "https://www.imdb.com/title/tt9000001/"}, {"url": "https://www.themoviedb.org/movie/9001"}},
		}
	}
	// 同名的另一部（豆瓣链接不同）排在前面，不能认错。
	fake.NeoDB["无声的证词"] = []map[string]any{neodbEntry("36199999", "同名的另一部"), neodbEntry("36100001", "NeoDB 的中文简介")}
	fake.TMDB["/3/movie/9001"] = map[string]any{
		"title": "无声的证词", "original_title": "Silent Witness", "overview": "TMDB 的简介", "poster_path": "/p.jpg",
		"vote_average": 7.5, "vote_count": 100, "runtime": 101, "release_date": "2025-05-01",
		"images": map[string]any{"backdrops": []map[string]any{{"file_path": "/b.jpg"}}},
	}
	// 只凭豆瓣 ID 收进来的老链接：什么资料都没有
	id, err := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36100001"}, "", ingest.PriorityDiscover)
	if err != nil {
		t.Fatal(err)
	}
	runAll(t, service, 30)
	m, _ := service.Media.Get(ctx, id)
	if m.Title != "沉默的证人" || m.Summary != "NeoDB 的中文简介" || strings.Join(m.Genres, ",") != "剧情,犯罪" ||
		strings.Join(m.Directors, ",") != "导演乙" || strings.Join(m.Actors, ",") != "演员丙" {
		t.Errorf("Wikidata、NeoDB 的资料没有写对: %+v", m)
	}
	if len(m.Backdrops) != 1 || m.RatingTMDB < 7 || m.DurationMinutes != 101 || m.TMDBID != "movie/9001" || m.Type != media.TypeMovie {
		t.Errorf("TMDB 的资料没有写入: %+v", m)
	}
	if m.RatingDouban != 0 || m.MetadataStatus != "partial" || m.EnrichedAt == nil || m.Fetching() || m.Hidden {
		t.Errorf("豆瓣没同步也应该建好档、正常展示，NeoDB 的评分不入库: %+v", m)
	}
	if results, _ := service.Media.Resolve(ctx, []media.Candidate{{Title: "沉默證人"}}); results[0].Status != "matched" {
		t.Error("NeoDB 的其他中文片名应该成为别名")
	}
	if len(fake.Prompts) != 0 {
		t.Error("豆瓣没同步、建档又不久的作品先不写语义描述")
	}
	if fake.Count("/subject/36100001/") != 0 {
		t.Error("没人在等的作品不应该去读豆瓣详情网页")
	}
	searches := fake.Count("/api/catalog/search")

	// 豆瓣恢复了：管理员手动刷新，豆瓣的资料覆盖建档的资料，接着写描述。
	delete(fake.Refuse, "36100001")
	fake.Subjects["36100001"] = map[string]any{"id": "36100001", "type": "movie", "title": "无声的证词", "intro": "豆瓣的简介",
		"genres": []string{"剧情", "悬疑"}, "year": "2025", "rating": map[string]any{"value": 8.1}}
	if err := service.RefreshNow(ctx, id); err != nil {
		t.Fatal(err)
	}
	runAll(t, service, 30)
	m, _ = service.Media.Get(ctx, id)
	if m.Summary != "豆瓣的简介" || strings.Join(m.Genres, ",") != "剧情,悬疑" || m.RatingDouban < 8 || m.MetadataStatus != "complete" {
		t.Errorf("豆瓣的资料应该覆盖建档的资料: %+v", m)
	}
	if len(fake.Prompts) != 1 || m.SemanticText == "" {
		t.Errorf("豆瓣同步以后应该写语义描述: %d", len(fake.Prompts))
	}
	if fake.Count("/api/catalog/search") != searches {
		t.Error("豆瓣同步过以后不应该再去问 NeoDB")
	}
}

// 打开作品页：访客和爬虫只排建档，不排豆瓣详情；登录用户打开没同步过豆瓣的，豆瓣详情提到最前面。
func TestViewedOnlyQueuesDoubanForSignedInUsers(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	id, _, _ := service.Media.EnsureDouban(ctx, "36100002", media.Patch{"title": "没补的片子"})
	m, _ := service.Media.Get(ctx, id)
	subject := strconv.FormatInt(id, 10)
	if err := service.Viewed(ctx, m, false); err != nil {
		t.Fatal(err)
	}
	if job, found, _ := service.Queue.Latest(ctx, ingest.KindEnrich, subject); !found || job.Priority != ingest.PriorityViewing {
		t.Errorf("访客打开没建档的作品应该马上排建档: %+v", job)
	}
	if _, found, _ := service.Queue.Latest(ctx, ingest.KindRefresh, subject); found {
		t.Error("访客打开作品页不应该排豆瓣详情")
	}
	if err := service.Viewed(ctx, m, true); err != nil {
		t.Fatal(err)
	}
	if job, found, _ := service.Queue.Latest(ctx, ingest.KindRefresh, subject); !found || job.Priority != ingest.PriorityViewing {
		t.Errorf("登录用户打开应该优先去读豆瓣: %+v", job)
	}
	// 建过档、豆瓣也同步过的，谁打开都不再排任务。
	testdb.Pool(t).Exec(ctx, `DELETE FROM jobs`)
	service.Media.MarkSynced(ctx, id, "complete")
	service.Media.MarkEnriched(ctx, id, time.Now())
	m, _ = service.Media.Get(ctx, id)
	service.Viewed(ctx, m, true)
	if counts, _ := service.Queue.Counts(ctx); counts["queued"] != 0 {
		t.Errorf("资料齐了不应该再排任务: %v", counts)
	}
}

// 排了很久的豆瓣任务，真要请求之前再确认一次：已经不需要了（比如插件交来了资料）就不去请求。
func TestRefreshSkipsWhenNoLongerDue(t *testing.T) {
	service, fake := newService(t)
	seedShow(fake)
	ctx := context.Background()
	id, _ := service.EnsureDouban(ctx, providers.DoubanItem{ID: "36000001", Title: "雾港 第二季", Kind: "tv"}, "", 0)
	testdb.Pool(t).Exec(ctx, `UPDATE media SET year = 2015 WHERE id = $1`, id)
	service.Media.MarkSynced(ctx, id, "complete")
	if err := service.Refresh(ctx, id, jobs.Job{}); !errors.Is(err, jobs.ErrSkip) {
		t.Errorf("已经同步过、不用再同步的作品应该跳过: %v", err)
	}
	if fake.Count("/rexxar/api/v2/tv/36000001") != 0 {
		t.Error("跳过时不应该请求豆瓣")
	}
}

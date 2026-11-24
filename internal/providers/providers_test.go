package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"moovie/internal/providers"
	"moovie/internal/providers/providerstest"
)

func TestDoubanSubjectTriesMovieThenTV(t *testing.T) {
	fake := providerstest.New(t)
	fake.Subjects["tv:36000001"] = map[string]any{
		"id": "36000001", "title": "雾港 第二季", "original_title": "Fog Harbor", "aka": []string{"雾港2"},
		"year": "2026", "intro": " 台风季的港城 ", "genres": []string{"悬疑", "犯罪"}, "countries": []string{"中国大陆"},
		"durations": []string{"45分钟"}, "episodes_count": 12, "rating": map[string]any{"value": 8.9},
		"pic":       map[string]any{"large": "https://img9.doubanio.com/view/photo/l_ratio_poster/public/p1.jpg"},
		"directors": []map[string]any{{"name": "陈默"}}, "actors": []map[string]any{{"name": "周冬雨"}, {"name": "张译"}},
	}
	subject, err := fake.Douban().Subject(context.Background(), "36000001", "")
	if err != nil {
		t.Fatal(err)
	}
	if subject.Kind != "tv" || subject.Title != "雾港 第二季" || subject.Year != 2026 || subject.DurationMinutes != 45 ||
		subject.EpisodesCount != 12 || subject.Intro != "台风季的港城" || len(subject.Actors) != 2 || subject.Rating != 8.9 {
		t.Errorf("详情解析不对: %+v", subject)
	}
	if _, err := fake.Douban().Subject(context.Background(), "36009999", ""); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("两个接口都没有时应该返回 ErrNotFound，实际 %v", err)
	}
}

func TestDoubanSuggestKeepsOnlyTitles(t *testing.T) {
	fake := providerstest.New(t)
	fake.Suggest["雾港"] = []map[string]any{
		{"id": "36000001", "title": "雾港", "type": "tv", "year": "2026", "img": "https://img1.doubanio.com/a.jpg"},
		{"id": "1000001", "title": "某演员", "type": "celebrity"},
	}
	items, err := fake.Douban().Suggest(context.Background(), "雾港")
	if err != nil || len(items) != 1 || items[0].Year != 2026 || items[0].Kind != "tv" {
		t.Fatalf("联想结果应该只保留影视条目: %+v err=%v", items, err)
	}
}

func TestDoubanPopularMergesListsAndSurvivesPartialFailure(t *testing.T) {
	fake := providerstest.New(t)
	fake.Popular["tv_domestic"] = []map[string]any{{"id": "1000001", "title": "国产剧一"}, {"id": "1000002", "title": "国产剧二"}}
	fake.Popular["tv_american"] = []map[string]any{{"id": "2000001", "title": "美剧一"}, {"id": "1000001", "title": "重复的"}}
	items, err := fake.Douban().Popular(context.Background(), "tv")
	if err != nil || len(items) != 3 || items[0].ID != "1000001" || items[1].ID != "2000001" {
		t.Fatalf("两个榜单应该交替合并并去重: %+v err=%v", items, err)
	}
	// 综艺榜没有配置数据，假服务器返回 500：整个榜单失败。
	if _, err := fake.Douban().Popular(context.Background(), "show"); err == nil {
		t.Error("所有接口都失败时应该报错")
	}
	if _, err := fake.Douban().Popular(context.Background(), "music"); err == nil {
		t.Error("不支持的榜单类型应该报错")
	}
}

func TestDoubanInterests(t *testing.T) {
	fake := providerstest.New(t)
	fake.Interests["evan/done"] = []map[string]any{
		{"comment": " 好看 ", "create_time": "2026-10-03 21:00:00", "rating": map[string]any{"value": 4},
			"subject": map[string]any{"id": "36000001", "title": "雾港", "type": "tv", "year": "2026"}},
		{"subject": map[string]any{"id": 36000002, "title": "回声", "type": "movie"}},
	}
	interests, total, err := fake.Douban().Interests(context.Background(), "evan", "done", 0, 50)
	if err != nil || total != 2 || len(interests) != 2 {
		t.Fatalf("应该读到 2 条标记: %d %d %v", total, len(interests), err)
	}
	if interests[0].Rating != 4 || interests[0].Comment != "好看" || interests[0].CreatedAt.Year() != 2026 || interests[1].Item.ID != "36000002" {
		t.Errorf("标记解析不对: %+v", interests)
	}
	if _, _, err := fake.Douban().Interests(context.Background(), "nobody", "done", 0, 50); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("不存在的用户应该返回 ErrNotFound，实际 %v", err)
	}
}

func TestWikidataByDoubanID(t *testing.T) {
	fake := providerstest.New(t)
	fake.Wikidata["36000001"] = []map[string]string{
		{"item": "http://www.wikidata.org/entity/Q123", "imdb": "tt1234567", "tmdbTV": "4455", "label": "Fog Harbor"},
		{"item": "http://www.wikidata.org/entity/Q123", "imdb": "tt1234567", "tmdbTV": "4455", "label": "雾港"},
	}
	entity, err := fake.WikidataClient().ByDoubanID(context.Background(), "36000001")
	if err != nil || entity.QID != "Q123" || entity.IMDbID != "tt1234567" || entity.TMDBTVID != "4455" || len(entity.Labels) != 2 {
		t.Fatalf("Wikidata 解析不对: %+v err=%v", entity, err)
	}
	if _, err := fake.WikidataClient().ByDoubanID(context.Background(), "36009999"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("没有对应条目时应该返回 ErrNotFound，实际 %v", err)
	}
	if _, err := fake.WikidataClient().ByDoubanID(context.Background(), `1" . ?x ?y ?z`); err == nil {
		t.Error("不是纯数字的 ID 不能拼进查询语句")
	}
}

func TestTMDBDetailsAndSeason(t *testing.T) {
	fake := providerstest.New(t)
	fake.TMDB["/3/find/tt1234567"] = map[string]any{"tv_results": []map[string]any{{"id": 4455}}}
	fake.TMDB["/3/tv/4455"] = map[string]any{
		"name": "雾港", "original_name": "Fog Harbor", "overview": "简介", "poster_path": "/p.jpg", "vote_average": 8.1, "vote_count": 321,
		"episode_run_time": []int{45}, "status": "Returning Series", "first_air_date": "2024-03-01", "popularity": 88.5,
		"seasons":      []map[string]any{{"season_number": 1, "episode_count": 12}, {"season_number": 2, "episode_count": 10, "air_date": "2026-10-08"}},
		"images":       map[string]any{"backdrops": []map[string]any{{"file_path": "/b1.jpg"}, {"file_path": "/b2.jpg"}}},
		"external_ids": map[string]any{"imdb_id": "tt1234567"},
	}
	fake.TMDB["/3/tv/4455/season/2"] = map[string]any{"episodes": []map[string]any{
		{"episode_number": 1, "name": "雾起", "air_date": "2026-10-08", "runtime": 46}, {"episode_number": 2, "name": "旧码头", "air_date": ""},
	}}
	tmdb := fake.TMDBClient()
	ctx := context.Background()
	id, kind, err := tmdb.FindByIMDb(ctx, "tt1234567")
	if err != nil || id != 4455 || kind != "tv" {
		t.Fatalf("FindByIMDb 不对: %d %s %v", id, kind, err)
	}
	details, err := tmdb.Details(ctx, "tv", 4455)
	if err != nil || details.Title != "雾港" || details.Runtime != 45 || details.Ended || details.IMDbID != "tt1234567" ||
		len(details.Backdrops) != 2 || details.Backdrops[0] != "https://image.tmdb.org/t/p/original/b1.jpg" || len(details.Seasons) != 2 {
		t.Fatalf("详情解析不对: %+v err=%v", details, err)
	}
	date := func(value *time.Time) string {
		if value == nil {
			return "nil"
		}
		return value.Format("2006-01-02")
	}
	if details.Popularity != 88.5 || date(details.ReleaseOf(1)) != "2024-03-01" || date(details.ReleaseOf(2)) != "2026-10-08" || date(details.ReleaseOf(3)) != "nil" {
		t.Errorf("剧集的日期和关注度不对: popularity=%v 第一季=%s 第二季=%s 第三季=%s",
			details.Popularity, date(details.ReleaseOf(1)), date(details.ReleaseOf(2)), date(details.ReleaseOf(3)))
	}
	fake.TMDB["/3/movie/603"] = map[string]any{"title": "黑客帝国", "release_date": "1999-03-31", "popularity": 41.2}
	if movie, err := tmdb.Details(ctx, "movie", 603); err != nil || date(movie.ReleaseOf(1)) != "1999-03-31" || movie.Popularity != 41.2 {
		t.Errorf("电影的上映日期不对: %+v %v", movie, err)
	}
	fake.TMDB["/3/movie/604"] = map[string]any{"title": "没日期", "release_date": ""}
	if movie, err := tmdb.Details(ctx, "movie", 604); err != nil || movie.ReleaseOf(1) != nil {
		t.Errorf("没有日期时应该是 nil: %+v %v", movie, err)
	}
	episodes, err := tmdb.Season(ctx, 4455, 2)
	if err != nil || len(episodes) != 2 || episodes[0].AirDate == nil || episodes[1].AirDate != nil || episodes[0].Runtime != 46 {
		t.Fatalf("分集解析不对: %+v err=%v", episodes, err)
	}
	if _, _, err := tmdb.FindByIMDb(ctx, "tt0000000"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("找不到时应该返回 ErrNotFound，实际 %v", err)
	}
	if (&providers.TMDB{}).Enabled() || !tmdb.Enabled() {
		t.Error("Enabled 应该取决于是否配置了 Token")
	}
}

// OpenAI 格式：/chat/completions 和 /embeddings，带上 Bearer 密钥；向量维度不对、送进去的字太多都要处理。
func TestOpenAIChatAndEmbed(t *testing.T) {
	dimensions := providers.EmbeddingDimensions
	var input string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/v1/chat/completions":
			messages := body["messages"].([]any)
			reply := messages[0].(map[string]any)["content"].(string) + "|" + messages[1].(map[string]any)["content"].(string)
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "  " + reply + "  "}}}})
		case "/v1/embeddings":
			input = body["input"].(string)
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"embedding": make([]float32, dimensions)}}})
		}
	}))
	defer server.Close()
	client := &providers.OpenAI{HTTP: server.Client(), BaseURL: server.URL + "/v1", Key: "k", Model: "m"}
	if reply, err := client.Chat(context.Background(), "系统", "用户"); err != nil || reply != "系统|用户" {
		t.Fatalf("对话结果不对: %q %v", reply, err)
	}
	if vector, err := client.Embed(context.Background(), strings.Repeat("影", 600)); err != nil || len(vector) != 768 || len([]rune(input)) != 450 {
		t.Fatalf("应该得到 768 维向量、最多送 450 个字: %d %d %v", len(vector), len([]rune(input)), err)
	}
	dimensions = 384
	if _, err := client.Embed(context.Background(), "雾港"); err == nil {
		t.Error("维度不对的向量应该被拒绝")
	}
	if (&providers.OpenAI{BaseURL: "x"}).Enabled() || (&providers.OpenAI{Model: "m"}).Enabled() {
		t.Error("地址和模型缺一个都应该是关闭的")
	}
}

func TestDanmuFetch(t *testing.T) {
	var matched string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/match":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			matched = body["fileName"]
			json.NewEncoder(w).Encode(map[string]any{"isMatched": true, "matches": []map[string]any{{"episodeId": 77}}})
		case "/api/v2/comment/77":
			json.NewEncoder(w).Encode(map[string]any{"comments": []map[string]any{
				{"p": "12.5,1,16711680,x", "m": "滚动"}, {"p": "3,5,255,x", "m": "顶部"}, {"p": "4,4,99999999,x", "m": "底部"},
				{"p": "bad", "m": "格式不对"}, {"p": "1,1,1", "m": "  "},
			}})
		}
	}))
	defer server.Close()
	danmu := &providers.Danmu{HTTP: server.Client(), Base: server.URL}
	items, err := danmu.Fetch(context.Background(), "雾港", 2, 3)
	if err != nil || matched != "雾港 S02E03" || len(items) != 3 {
		t.Fatalf("弹幕解析不对: matched=%q %+v err=%v", matched, items, err)
	}
	if items[0].Color != "#FF0000" || items[1].Mode != 1 || items[2].Mode != 2 || items[2].Color != "#FFFFFF" {
		t.Errorf("模式和颜色映射不对: %+v", items)
	}
}

func TestPacerSpacesRequests(t *testing.T) {
	pacer := providers.NewPacer(40 * time.Millisecond)
	start := time.Now()
	for range 3 {
		if err := pacer.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Errorf("三次请求之间应该至少间隔两个周期，实际只过了 %v", elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pacer.Wait(context.Background())
	if err := pacer.Wait(ctx); err == nil {
		t.Error("ctx 取消后等待应该返回错误")
	}
}

func TestPacerBacksOffWhenThrottled(t *testing.T) {
	p := providers.NewPacer(time.Second)
	p.Throttled()
	if interval, paused := p.Status(); interval != 2*time.Second || !paused.IsZero() {
		t.Errorf("被限流一次应该放慢一倍: %s %v", interval, paused)
	}
	p.Throttled()
	p.Throttled()
	if _, paused := p.Status(); paused.Before(time.Now().Add(9 * time.Minute)) {
		t.Errorf("连续被限流 3 次应该暂停 10 分钟: %v", paused)
	}
	if err := p.Wait(context.Background()); !errors.Is(err, providers.ErrThrottled) {
		t.Errorf("暂停期间应该直接返回限流，而不是干等: %v", err)
	}
	for range 30 {
		p.Succeeded()
	}
	if interval, _ := p.Status(); interval != time.Second {
		t.Errorf("成功多次后应该回到原来的间隔: %s", interval)
	}
	for range 10 {
		p.Throttled()
	}
	if interval, _ := p.Status(); interval != time.Minute {
		t.Errorf("间隔最多 1 分钟: %s", interval)
	}
}

// 已经知道是剧集时先试剧集接口，只发一次请求；用电影接口查到剧集时（豆瓣会跳转过去），类型以返回的内容为准。
func TestDoubanSubjectUsesKnownTypeAndReturnedType(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/rexxar/api/v2/movie/26302614": // 真实的豆瓣在这里返回 301，跳到下面的剧集接口
			http.Redirect(w, r, "/rexxar/api/v2/tv/26302614", http.StatusMovedPermanently)
		case "/rexxar/api/v2/tv/26302614":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"26302614","type":"tv","title":"请回答1988","episodes_count":20}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	douban := providers.NewDouban(server.Client())
	douban.MobileBase, douban.Pace = server.URL, nil

	subject, err := douban.Subject(context.Background(), "26302614", "tv")
	if err != nil || subject.Kind != "tv" || len(paths) != 1 {
		t.Fatalf("已知是剧集时应该一次请求就拿到: %+v err=%v 请求=%v", subject, err, paths)
	}
	subject, err = douban.Subject(context.Background(), "26302614", "movie")
	if err != nil || subject.Kind != "tv" || subject.EpisodesCount != 20 {
		t.Errorf("从电影接口跳转过来的剧集，类型应该是 tv: %+v err=%v", subject, err)
	}
}

// 豆瓣有时用 400 + subject_ip_rate_limit 表示限流，也要按限流处理。
func TestDoubanRateLimitIn400(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"request": "GET \/v2\/movie\/36877245", "msg": "subject_ip_rate_limit", "code": 1309}`))
	}))
	defer server.Close()
	douban := providers.NewDouban(server.Client())
	douban.MobileBase, douban.Pace = server.URL, providers.NewPacer(10*time.Millisecond)
	if _, err := douban.Subject(context.Background(), "36877245", "movie"); !errors.Is(err, providers.ErrThrottled) {
		t.Fatalf("400 + rate_limit 应该识别为限流: %v", err)
	}
	if interval, _ := douban.Pace.Status(); interval <= 10*time.Millisecond {
		t.Errorf("被限流后应该放慢: %s", interval)
	}
}

// 只有个别条目返回 403、谁都能打开的那部电影能正常打开：是这个条目打不开，不是限流，不能让整条通道停下。
// 那部电影也返回 403 才是真的被拒绝了。
func TestDouban403OnlyThrottlesWhenEverythingIsRefused(t *testing.T) {
	refuseAll := false
	canary := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/1292052") {
			canary++
			if !refuseAll {
				w.Write([]byte(`{"id": "1292052", "title": "肖申克的救赎", "type": "movie"}`))
				return
			}
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()
	douban := providers.NewDouban(server.Client())
	douban.MobileBase, douban.Pace = server.URL, providers.NewPacer(10*time.Millisecond)

	_, err := douban.Subject(context.Background(), "36000001", "movie")
	if err == nil || errors.Is(err, providers.ErrThrottled) {
		t.Fatalf("个别条目的 403 不应该算限流: %v", err)
	}
	if interval, paused := douban.Pace.Status(); interval != 10*time.Millisecond || !paused.IsZero() {
		t.Errorf("个别条目的 403 不应该让请求放慢: %s %v", interval, paused)
	}
	douban.Subject(context.Background(), "36000002", "movie")
	if canary != 1 {
		t.Errorf("一分钟内只该检查一次那部电影，实际 %d 次", canary)
	}

	refuseAll = true
	douban = providers.NewDouban(server.Client())
	douban.MobileBase, douban.Pace = server.URL, providers.NewPacer(10*time.Millisecond)
	if _, err := douban.Subject(context.Background(), "36000001", "movie"); !errors.Is(err, providers.ErrThrottled) {
		t.Fatalf("那部电影也打不开，应该算限流: %v", err)
	}
	if interval, _ := douban.Pace.Status(); interval <= 10*time.Millisecond {
		t.Errorf("被限流后应该放慢: %s", interval)
	}
}

func TestDoubanThrottleSlowsDown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()
	douban := providers.NewDouban(server.Client())
	douban.MobileBase, douban.WebBase = server.URL, server.URL
	douban.Pace = providers.NewPacer(10 * time.Millisecond)
	if _, err := douban.Subject(context.Background(), "1292052", ""); !errors.Is(err, providers.ErrThrottled) {
		t.Fatalf("429 应该识别为限流: %v", err)
	}
	if interval, _ := douban.Pace.Status(); interval <= 10*time.Millisecond {
		t.Errorf("被限流后应该放慢: %s", interval)
	}
}

// 每日导出文件：按日期拼文件名，解压后逐行读取，坏行跳过；当天的文件还没有时返回 ErrNotFound。
func TestTMDBPopularityExport(t *testing.T) {
	fake := providerstest.New(t)
	fake.Exports["movie_ids_11_02_2026.json.gz"] = []map[string]any{{"id": 603, "popularity": 41.2, "adult": false}, {"id": 604, "popularity": 0.6}}
	fake.Exports["tv_series_ids_11_02_2026.json.gz"] = []map[string]any{{"id": 77, "popularity": 9.5}}
	tmdb := fake.TMDBClient()
	ctx := context.Background()
	day := time.Date(2026, 11, 2, 23, 0, 0, 0, time.FixedZone("UTC+8", 8*3600)) // UTC 是 11 月 2 日
	got := map[int]float64{}
	if err := tmdb.Popularity(ctx, "movie", day, func(id int, popularity float64) { got[id] = popularity }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[603] != 41.2 || got[604] != 0.6 {
		t.Errorf("电影导出文件读得不对: %v", got)
	}
	tv := map[int]float64{}
	if err := tmdb.Popularity(ctx, "tv", day, func(id int, popularity float64) { tv[id] = popularity }); err != nil || tv[77] != 9.5 {
		t.Errorf("剧集导出文件读得不对: %v %v", tv, err)
	}
	if err := tmdb.Popularity(ctx, "movie", day.AddDate(0, 0, 1), func(int, float64) {}); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("文件还没生成时应该返回 ErrNotFound，实际 %v", err)
	}
	if err := tmdb.Popularity(ctx, "person", day, func(int, float64) {}); err == nil {
		t.Error("不认识的类型应该报错")
	}
}

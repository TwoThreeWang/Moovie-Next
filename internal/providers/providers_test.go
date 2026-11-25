package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
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

// 个别条目返回 403 只算这个条目失败，不能让整条通道停下，也不该放慢；
// 上一次成功之后接连 3 个不同条目都被拒绝（哪怕热门条目还能打开），才是在拒绝我们。
func TestDouban403OnlyThrottlesWhenManySubjectsAreRefused(t *testing.T) {
	var mu sync.Mutex
	refused := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		for id := range refused {
			if strings.HasSuffix(r.URL.Path, "/"+id) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Write([]byte(`{"id": "` + id + `", "title": "片名", "type": "movie"}`))
	}))
	defer server.Close()
	refuse := func(ids ...string) {
		mu.Lock()
		defer mu.Unlock()
		for _, id := range ids {
			refused[id] = true
		}
	}
	douban := providers.NewDouban(server.Client())
	douban.MobileBase, douban.Pace = server.URL, providers.NewPacer(10*time.Millisecond)
	ctx := context.Background()

	// 一个坏条目夹在正常条目中间：电影、剧集两个接口都 403 也只算一个条目，不算限流
	refuse("36000001", "36000002")
	for _, id := range []string{"36000001", "1292052", "36000002", "1292052"} {
		_, err := douban.Subject(ctx, id, "movie")
		if errors.Is(err, providers.ErrThrottled) {
			t.Fatalf("个别条目的 403 不应该算限流: %s %v", id, err)
		}
	}
	if interval, paused := douban.Pace.Status(); interval != 10*time.Millisecond || !paused.IsZero() {
		t.Errorf("个别条目的 403 不应该让请求放慢: %s %v", interval, paused)
	}

	// 接连几个不同条目都被拒绝，中间没有成功：热门条目还能打开也算限流
	refuse("36000003", "36000004", "36000005")
	douban.Subject(ctx, "36000003", "movie")
	if _, err := douban.Subject(ctx, "36000004", "movie"); errors.Is(err, providers.ErrThrottled) {
		t.Fatalf("第二个被拒绝的条目还不该算限流: %v", err)
	}
	if _, err := douban.Subject(ctx, "36000005", "movie"); !errors.Is(err, providers.ErrThrottled) {
		t.Fatalf("接连 3 个不同条目被拒绝应该算限流: %v", err)
	}
	if interval, _ := douban.Pace.Status(); interval <= 10*time.Millisecond {
		t.Errorf("被限流后应该放慢: %s", interval)
	}
	// 在恢复成功之前，再被拒绝的也都算限流，不会白白用掉任务的重试次数
	if _, err := douban.Subject(ctx, "36000001", "movie"); !errors.Is(err, providers.ErrThrottled) {
		t.Fatalf("还没恢复时的 403 应该算限流: %v", err)
	}
	// 成功一次就清零
	douban.Pace = providers.NewPacer(10 * time.Millisecond)
	if _, err := douban.Subject(ctx, "1292052", "movie"); err != nil {
		t.Fatal(err)
	}
	if _, err := douban.Subject(ctx, "36000001", "movie"); errors.Is(err, providers.ErrThrottled) {
		t.Fatalf("成功之后的第一个 403 不该算限流: %v", err)
	}
}

// 豆瓣分接口限流：条目详情被拒绝时，用户的标记列表、榜单可能还能打开。
// 别的接口成功不能把条目的拒绝计数清零，否则同步用户时条目的 403 又全成了个别失败。
func TestDoubanRefusalsAreCountedPerEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/interests"):
			w.Write([]byte(`{"total": 0, "interests": []}`))
		case strings.Contains(r.URL.Path, "/hot_gaia"):
			w.Write([]byte(`{"items": [{"id": "1", "title": "片名", "type": "movie"}]}`))
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
		}
	}))
	defer server.Close()
	douban := providers.NewDouban(server.Client())
	douban.MobileBase, douban.WebBase, douban.Pace = server.URL, server.URL, providers.NewPacer(time.Millisecond)
	ctx := context.Background()
	for index, id := range []string{"36000001", "36000002", "36000003"} {
		if _, _, err := douban.Interests(ctx, "u1", "done", 0, 50); err != nil {
			t.Fatal(err)
		}
		if _, err := douban.Popular(ctx, "movie"); err != nil {
			t.Fatal(err)
		}
		_, err := douban.Subject(ctx, id, "movie")
		if throttled := errors.Is(err, providers.ErrThrottled); throttled != (index == 2) {
			t.Fatalf("第 %d 个被拒绝的条目：算限流=%v，%v", index+1, throttled, err)
		}
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

// 请求头扮成普通浏览器：手机版接口用 iPhone，网页版接口用电脑上的 Chrome；
// bid 和豆瓣发的一样是 11 位大小写字母加数字，每次都不同，还带着城市 Cookie 和中文的 Accept-Language。
func TestDoubanRequestHeaders(t *testing.T) {
	var mu sync.Mutex
	headers := map[string]http.Header{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers[r.URL.Path] = r.Header.Clone()
		mu.Unlock()
		switch r.URL.Path {
		case "/j/subject_suggest":
			w.Write([]byte(`[]`))
		case "/feed/people/u1/interests":
			w.Write([]byte(`<?xml version="1.0"?><rss><channel></channel></rss>`))
		default:
			w.Write([]byte(`{"id": "1292052", "title": "片名", "type": "movie"}`))
		}
	}))
	defer server.Close()
	douban := providers.NewDouban(server.Client())
	douban.MobileBase, douban.WebBase, douban.FeedBase, douban.Pace = server.URL, server.URL, server.URL, nil
	ctx := context.Background()
	if _, err := douban.Subject(ctx, "1292052", "movie"); err != nil {
		t.Fatal(err)
	}
	if _, err := douban.Suggest(ctx, "片名"); err != nil {
		t.Fatal(err)
	}
	if _, err := douban.RecentInterests(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	bid := regexp.MustCompile(`^ll="108288"; bid=([A-Za-z0-9]{11})$`)
	bids := map[string]bool{}
	for path, wantUA := range map[string]string{"/rexxar/api/v2/movie/1292052": "iPhone", "/j/subject_suggest": "Macintosh", "/feed/people/u1/interests": "Macintosh"} {
		header := headers[path]
		if !strings.Contains(header.Get("User-Agent"), wantUA) {
			t.Errorf("%s 的 UA 应该是 %s: %q", path, wantUA, header.Get("User-Agent"))
		}
		if !strings.HasPrefix(header.Get("Accept-Language"), "zh-CN") {
			t.Errorf("%s 应该带中文的 Accept-Language: %q", path, header.Get("Accept-Language"))
		}
		match := bid.FindStringSubmatch(header.Get("Cookie"))
		if match == nil {
			t.Errorf("%s 的 Cookie 不像浏览器的: %q", path, header.Get("Cookie"))
			continue
		}
		bids[match[1]] = true
	}
	if len(bids) != 3 {
		t.Errorf("每次请求的 bid 应该都不同: %v", bids)
	}
	if headers["/j/subject_suggest"].Get("X-Requested-With") != "XMLHttpRequest" {
		t.Error("网页版的 /j/ 接口应该像页面里的 Ajax 请求")
	}
}

// 被转到验证页、或者该是数据的地方返回了网页，都是豆瓣在拦我们：按限流算，请求放慢。
// 同一个域名里的跳转（电影接口跳到剧集接口）照常跟着走。
func TestDoubanVerificationPageCountsAsThrottle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/misc/sorry"):
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><title>禁止访问</title></html>`))
		case strings.HasSuffix(r.URL.Path, "/36000001"):
			http.Redirect(w, r, "/misc/sorry?original-url="+r.URL.Path, http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/36000002"):
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("\n<!DOCTYPE html><html><head><title>豆瓣</title></head></html>"))
		case r.URL.Path == "/feed/people/u1/interests":
			w.Write([]byte(`<!DOCTYPE html><html><title>登录豆瓣</title></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	douban := providers.NewDouban(server.Client())
	douban.MobileBase, douban.FeedBase = server.URL, server.URL
	ctx := context.Background()
	for _, id := range []string{"36000001", "36000002"} {
		douban.Pace = providers.NewPacer(10 * time.Millisecond)
		if _, err := douban.Subject(ctx, id, "movie"); !errors.Is(err, providers.ErrThrottled) {
			t.Fatalf("%s：验证页应该按限流算: %v", id, err)
		}
		if interval, _ := douban.Pace.Status(); interval <= 10*time.Millisecond {
			t.Errorf("%s：被拦下后应该放慢: %s", id, interval)
		}
	}
	if _, err := douban.RecentInterests(ctx, "u1"); !errors.Is(err, providers.ErrThrottled) {
		t.Errorf("RSS 返回网页也应该按限流算: %v", err)
	}
}

// 热门榜先用网页版接口；网页版取不到（出错或者是空的）再用手机版接口兜底。
func TestDoubanPopularPrefersWebAndFallsBackToRexxar(t *testing.T) {
	fake := providerstest.New(t)
	fake.WebPopular["movie/热门"] = []map[string]any{
		{"id": "1292052", "title": "肖申克的救赎", "rate": "9.7", "cover": "https://img2.doubanio.com/p1.jpg"},
		{"id": "1292052", "title": "重复的"},
		{"id": "36000001", "title": "新片", "rate": ""},
	}
	fake.Popular["hot_gaia"] = []map[string]any{{"id": "9999999", "title": "手机版的"}}
	items, err := fake.Douban().Popular(context.Background(), "movie")
	if err != nil || len(items) != 2 || items[0].Rating != 9.7 || items[0].Kind != "movie" || items[0].Poster == "" || items[1].Rating != 0 {
		t.Fatalf("应该用网页版的榜单并去重: %+v err=%v", items, err)
	}
	if fake.Count("/rexxar/api/v2/movie/hot_gaia") != 0 {
		t.Error("网页版取到了就不该再问手机版")
	}

	fake.WebPopular["tv/综艺"] = []map[string]any{}
	fake.Popular["tv_variety_show"] = []map[string]any{{"id": "3000001", "title": "综艺一", "type": "tv"}}
	items, err = fake.Douban().Popular(context.Background(), "show")
	if err != nil || len(items) != 1 || items[0].ID != "3000001" {
		t.Fatalf("网页版是空的时应该用手机版兜底: %+v err=%v", items, err)
	}
	// 网页版出错（假服务器对没配置的返回 500）也用手机版兜底。
	fake.Popular["tv_animation"] = []map[string]any{{"id": "4000001", "title": "动漫一"}}
	if items, err := fake.Douban().Popular(context.Background(), "anime"); err != nil || len(items) != 1 {
		t.Fatalf("网页版出错时应该用手机版兜底: %+v err=%v", items, err)
	}
}

// RSS 里只认影视条目的看过和想看；书、音乐、在看这些只留下时间。
func TestDoubanRecentInterests(t *testing.T) {
	fake := providerstest.New(t)
	fake.Feeds["evan"] = []map[string]string{
		{"title": "看过雾港", "link": "https://movie.douban.com/subject/36000001/", "pubDate": "Tue, 24 Nov 2026 13:45:12 GMT"},
		{"title": "想看回声", "link": "https://movie.douban.com/subject/36000002/", "pubDate": "Mon, 23 Nov 2026 21:00:00 +0800"},
		{"title": "在看漫长的季节", "link": "https://movie.douban.com/subject/35588177/", "pubDate": "Mon, 23 Nov 2026 20:00:00 +0800"},
		{"title": "想读三体", "link": "https://book.douban.com/subject/2567698/", "pubDate": "Sun, 22 Nov 2026 10:00:00 +0800"},
	}
	recent, err := fake.Douban().RecentInterests(context.Background(), "evan")
	if err != nil || len(recent) != 4 {
		t.Fatalf("应该读到 4 条动态: %+v err=%v", recent, err)
	}
	if recent[0].ID != "36000001" || recent[0].Status != "done" || recent[0].At.UTC().Hour() != 13 {
		t.Errorf("看过解析不对: %+v", recent[0])
	}
	if recent[1].Status != "mark" || recent[1].At.UTC().Hour() != 13 {
		t.Errorf("想看解析不对: %+v", recent[1])
	}
	if recent[2].Status != "" || recent[3].ID != "" || recent[3].Status != "" || recent[3].At.IsZero() {
		t.Errorf("在看和书只该留下时间: %+v %+v", recent[2], recent[3])
	}
	if _, err := fake.Douban().RecentInterests(context.Background(), "nobody"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("不存在的用户应该返回 ErrNotFound，实际 %v", err)
	}
}

// 详情网页有自己的节奏：详情接口被限流暂停时，网页照样能打开。没有的条目是 ErrNotFound，验证页按限流算。
func TestDoubanSubjectPageHasItsOwnPace(t *testing.T) {
	fake := providerstest.New(t)
	page, err := os.ReadFile("testdata/subject_tv.html")
	if err != nil {
		t.Fatal(err)
	}
	fake.Pages["35588177"] = string(page)
	fake.Pages["36000009"] = `<html><head><title>豆瓣</title></head><body>检测到有异常请求</body></html>`
	douban := fake.Douban()
	douban.Pace, douban.PagePace = providers.NewPacer(time.Millisecond), providers.NewPacer(time.Millisecond)
	for range 3 {
		douban.Pace.Throttled()
	}
	if _, paused := douban.Pace.Status(); paused.IsZero() {
		t.Fatal("详情接口应该已经暂停了")
	}
	ctx := context.Background()
	subject, err := douban.SubjectPage(ctx, "35588177")
	if err != nil || subject.Title != "漫长的季节" || subject.Kind != "tv" {
		t.Fatalf("接口暂停时详情网页应该照样能打开: %+v err=%v", subject, err)
	}
	if _, err := douban.SubjectPage(ctx, "36000001"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("没有的条目应该返回 ErrNotFound: %v", err)
	}
	if _, err := douban.SubjectPage(ctx, "36000009"); !errors.Is(err, providers.ErrThrottled) {
		t.Errorf("验证页应该按限流算: %v", err)
	}
	if interval, _ := douban.PagePace.Status(); interval <= time.Millisecond {
		t.Errorf("网页被拦下后网页的节奏应该放慢: %s", interval)
	}
	if _, err := douban.SubjectPage(ctx, "1292052/../x"); err == nil {
		t.Error("不是纯数字的 ID 应该被拒绝")
	}
}

// 详情接口对个别条目返回 403 时，错误里带着 ErrRefused，调用方可以改用详情网页。
func TestDoubanRefusedSubjectIsMarked(t *testing.T) {
	fake := providerstest.New(t)
	fake.Refuse["36000001"] = true
	_, err := fake.Douban().Subject(context.Background(), "36000001", "movie")
	if !errors.Is(err, providers.ErrRefused) || errors.Is(err, providers.ErrThrottled) {
		t.Errorf("个别条目的 403 应该是 ErrRefused、还不算限流: %v", err)
	}
}

// 标记列表里条目的豆瓣评分也读出来，用来顺手更新已有的作品。
func TestDoubanInterestsCarrySubjectRating(t *testing.T) {
	fake := providerstest.New(t)
	fake.Interests["evan/done"] = []map[string]any{{"rating": map[string]any{"value": 2},
		"subject": map[string]any{"id": "36000001", "title": "雾港", "type": "tv", "rating": map[string]any{"value": 8.6}}}}
	interests, _, err := fake.Douban().Interests(context.Background(), "evan", "done", 0, 50)
	if err != nil || len(interests) != 1 || interests[0].Item.Rating != 8.6 || interests[0].Rating != 2 {
		t.Fatalf("条目的豆瓣评分和用户自己打的分要分开: %+v err=%v", interests, err)
	}
}

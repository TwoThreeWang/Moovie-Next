package danmaku_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"moovie/internal/danmaku"
	"moovie/internal/db/testdb"
	"moovie/internal/media"
	"moovie/internal/providers"
)

type fakeUpstream struct {
	calls  []string
	result []providers.DanmuComment
	err    error
}

func (f *fakeUpstream) Enabled() bool { return true }
func (f *fakeUpstream) Fetch(_ context.Context, title string, season, episode int) ([]providers.DanmuComment, error) {
	f.calls = append(f.calls, title)
	return f.result, f.err
}

func TestListMergesLocalAndUpstream(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	mediaStore := media.NewStore(pool)
	ctx := context.Background()
	id, _, _ := mediaStore.EnsureDouban(ctx, "30000001", media.Patch{"title": "雾港 第二季", "season": 2, "media_type": media.TypeTV})
	upstream := &fakeUpstream{result: []providers.DanmuComment{{Time: 5, Text: "来自弹幕库", Color: "#FFFFFF"}}}
	service := danmaku.NewService(danmaku.NewStore(pool), mediaStore, upstream)
	key := danmaku.Key{MediaID: id, Season: 2, Episode: 3}

	if err := service.Send(ctx, 1, key, providers.DanmuComment{Time: 12.5, Text: " 前排 ", Color: "#ff0000"}); err != nil {
		t.Fatal(err)
	}
	if err := service.Send(ctx, 1, key, providers.DanmuComment{Time: 13, Text: "前排"}); !errors.Is(err, danmaku.ErrDuplicate) {
		t.Errorf("10 秒内重复的内容应该被拒绝，实际 %v", err)
	}
	items, err := service.List(ctx, key)
	if err != nil || len(items) != 2 || items[0].Text != "来自弹幕库" || items[1].Text != "前排" || items[1].Color != "#FF0000" {
		t.Fatalf("应该合并两处弹幕并按时间排序: %+v err=%v", items, err)
	}
	// 去第三方弹幕库查询时用去掉季数的片名。
	if len(upstream.calls) != 1 || upstream.calls[0] != "雾港" {
		t.Errorf("查询弹幕库用的片名不对: %v", upstream.calls)
	}
	// 第二次走缓存，不再请求。
	service.List(ctx, key)
	if len(upstream.calls) != 1 {
		t.Errorf("第二次应该用缓存，实际请求了 %d 次", len(upstream.calls))
	}
	// 别的集没有这条用户弹幕。
	other, _ := service.List(ctx, danmaku.Key{MediaID: id, Season: 2, Episode: 4})
	if len(other) != 1 {
		t.Errorf("第 4 集应该只有弹幕库的 1 条，实际 %d", len(other))
	}
}

func TestUpstreamMatchQueries(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	ctx := context.Background()
	mediaStore := media.NewStore(pool)
	movieTitle := "雾港 第二部"
	movie, _, err := mediaStore.EnsureDouban(ctx, "30000001", media.Patch{"title": movieTitle, "media_type": media.TypeMovie})
	if err != nil {
		t.Fatal(err)
	}
	series, _, err := mediaStore.EnsureDouban(ctx, "30000002", media.Patch{"title": "雾港 第二季", "media_type": media.TypeTV, "season": 2})
	if err != nil {
		t.Fatal(err)
	}

	queries := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/match" || json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		queries <- body["fileName"]
		_ = json.NewEncoder(w).Encode(map[string]bool{"isMatched": false})
	}))
	defer server.Close()
	upstream := &providers.Danmu{HTTP: server.Client(), Base: server.URL}
	store := danmaku.NewStore(pool)
	service := danmaku.NewService(store, mediaStore, upstream)
	movieKey := danmaku.Key{MediaID: movie, Season: 1, Episode: 1}
	if err := service.Send(ctx, 1, movieKey, providers.DanmuComment{Time: 1, Text: "电影弹幕", Color: "#FFFFFF"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key      danmaku.Key
		fileName string
	}{
		{movieKey, movieTitle},
		{danmaku.Key{MediaID: series, Season: 2, Episode: 1}, "雾港 S02E01"},
		{danmaku.Key{MediaID: series, Season: 2, Episode: 2}, "雾港 S02E02"},
		{danmaku.Key{Title: movieTitle, Season: 1, Episode: 1}, movieTitle + " S01E01"},
	} {
		items, err := service.List(ctx, tc.key)
		if err != nil {
			t.Fatal(err)
		}
		if tc.key == movieKey && (len(items) != 1 || items[0].Text != "电影弹幕") {
			t.Fatalf("按站内第 1 集读取电影弹幕失败: %+v", items)
		}
		select {
		case got := <-queries:
			if got != tc.fileName {
				t.Errorf("匹配参数 fileName = %q，期望 %q", got, tc.fileName)
			}
		default:
			t.Errorf("没有发出第三方匹配请求，期望 %q", tc.fileName)
		}
	}
	// 直接读原归档键，避免发送和读取同时换集号也能通过测试。
	if items, err := store.List(ctx, movieKey, 1); err != nil || len(items) != 1 || items[0].Text != "电影弹幕" {
		t.Fatalf("电影弹幕应保留在站内第 1 集: %+v err=%v", items, err)
	}
}

func TestTitleKeyedDanmakuAndValidation(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	upstream := &fakeUpstream{err: errors.New("弹幕库挂了")}
	service := danmaku.NewService(danmaku.NewStore(pool), media.NewStore(pool), upstream)
	ctx := context.Background()
	key := danmaku.Key{Title: "没有元数据的片子", Season: 1, Episode: 2}

	if err := service.Send(ctx, 1, key, providers.DanmuComment{Time: 1, Text: "你好"}); err != nil {
		t.Fatal(err)
	}
	// 片名写法不同但规范化后相同，应该是同一集。
	items, err := service.List(ctx, danmaku.Key{Title: "没有元数据的 片子", Season: 1, Episode: 2})
	if err != nil || len(items) != 1 {
		t.Fatalf("弹幕库出错时仍然应该返回用户弹幕: %+v err=%v", items, err)
	}
	for name, d := range map[string]providers.DanmuComment{
		"空内容":  {Time: 1, Text: "  "},
		"太长":   {Time: 1, Text: "这是一条非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常长的弹幕"},
		"颜色不对": {Time: 1, Text: "x", Color: "red"},
		"时间为负": {Time: -1, Text: "x"},
		"模式不对": {Time: 1, Text: "x", Mode: 7},
	} {
		if err := service.Send(ctx, 1, key, d); !errors.Is(err, danmaku.ErrInvalid) {
			t.Errorf("%s：应该返回 ErrInvalid，实际 %v", name, err)
		}
	}
	if err := service.Send(ctx, 1, danmaku.Key{Title: "！！", Episode: 1}, providers.DanmuComment{Time: 1, Text: "x"}); !errors.Is(err, danmaku.ErrInvalid) {
		t.Errorf("既没有作品也没有有效片名时应该拒绝，实际 %v", err)
	}
	if _, err := service.List(ctx, danmaku.Key{MediaID: 99999, Episode: 1}); err == nil {
		t.Error("作品不存在时应该报错")
	}
}

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
)

// hongguoPage 拼一个和官网结构一样的页面：数据在 _ROUTER_DATA 里，前后是一堆无关的脚本。
func hongguoPage(t *testing.T, loaderData map[string]any) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"loaderData": loaderData})
	if err != nil {
		t.Fatal(err)
	}
	return `<!doctype html><html><head><script>!function(){var e="x"}()</script></head><body><div id="root"></div>` +
		`<script async data-script-src="modern-inline">_ROUTER_DATA = ` + string(data) + `;</script><script>runWindowFn();</script></body></html>`
}

func newHongguo(t *testing.T, handler http.HandlerFunc) *providers.Hongguo {
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := providers.NewHongguo(server.Client())
	client.Base, client.Pace = server.URL, providers.NewPacer(time.Millisecond)
	return client
}

func TestHongguoSeriesReadsRouterData(t *testing.T) {
	client := newHongguo(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("series_id") {
		case "7706890402913078297":
			w.Write([]byte(hongguoPage(t, map[string]any{"detail_layout": nil, "detail_page": map[string]any{
				"recommendations": map[string]any{},
				"seriesDetail": map[string]any{
					"series_id": "7706890402913078297", "series_name": " 帐中香第二季 ", "series_intro": "尚书府嫡女……",
					"series_cover": "https://p6-novel.byteimg.com/novel-pic/abc~tplv-shrink:640:0.image",
					"tags":         []string{"恋爱", "古代", "漫剧"}, "celebrities": []any{"演员甲", map[string]any{"name": "演员乙"}},
					"episode_cnt": 135, "episode_right_text": "全135集", "first_visible_time": "1794845285",
					"series_episode_info": map[string]any{"episode_cnt": 135, "episode_total_cnt": 135, "series_status": 1},
					"season_info":         map[string]any{"series_group_id": "7687910242989919256"},
					"vid_list":            []string{"不该被读取的视频 ID"},
				},
				"seriesSocialInfo": map[string]any{"rating": 9, "series_like_count": "663000", "hot_score_data": map[string]any{"score": 70974984}},
			}})))
		case "7700000000000000001":
			// 连载中：只有"更新至"，总集数不知道，没有评分
			w.Write([]byte(hongguoPage(t, map[string]any{"detail_page": map[string]any{"seriesDetail": map[string]any{
				"series_id": "7700000000000000001", "series_name": "新剧", "episode_right_text": "更新至30集",
				"series_episode_info": map[string]any{"episode_cnt": 30, "episode_total_cnt": 0},
			}, "seriesSocialInfo": map[string]any{}}})))
		case "7700000000000000002":
			w.Write([]byte(hongguoPage(t, map[string]any{"detail_page": map[string]any{"seriesDetail": nil}})))
		case "7700000000000000003":
			w.Write([]byte(`<html><body>请完成安全验证</body></html>`))
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	series, err := client.Series(ctx, "7706890402913078297")
	if err != nil {
		t.Fatal(err)
	}
	if series.Title != "帐中香第二季" || series.Episodes != 135 || series.Total != 135 || !series.Ended || series.Rating != 9 ||
		series.Heat != 70974984 || series.GroupID != "7687910242989919256" || series.OnlineAt.Year() != 2026 ||
		strings.Join(series.Tags, ",") != "恋爱,古代" || strings.Join(series.Actors, ",") != "演员甲,演员乙" {
		t.Errorf("详情解析不对: %+v", series)
	}
	ongoing, err := client.Series(ctx, "7700000000000000001")
	if err != nil || ongoing.Ended || ongoing.Episodes != 30 || ongoing.Total != 0 || ongoing.Rating != 0 {
		t.Errorf("连载中的剧解析不对: %+v err=%v", ongoing, err)
	}
	if _, err := client.Series(ctx, "7700000000000000002"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("页面上没有这部剧时应该返回 ErrNotFound，实际 %v", err)
	}
	if _, err := client.Series(ctx, "7700000000000000003"); !errors.Is(err, providers.ErrThrottled) {
		t.Errorf("页面里没有数据（验证页）时应该按限流处理，实际 %v", err)
	}
	if _, err := client.Series(ctx, "7700000000000000009"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("404 应该返回 ErrNotFound，实际 %v", err)
	}
}

func TestHongguoRankAndCategory(t *testing.T) {
	client := newHongguo(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rank/hot-drama":
			if r.URL.Query().Get("page") != "2" {
				http.Error(w, "只测第 2 页", http.StatusBadRequest)
				return
			}
			w.Write([]byte(hongguoPage(t, map[string]any{"rank_layout": nil, "rank_hot-drama/page": map[string]any{"content": map[string]any{
				"rankList": []map[string]any{
					{"seriesId": "7705824595416190014", "title": "穿越八零", "tags": []string{"AI剧", "剧情", "赵乾志 · 陈芸"}},
					{"seriesId": "7705824595416190015", "title": "预约中", "reservationOnly": true},
					{"seriesId": "7705824595416190016", "title": "真人的", "tags": []string{"家庭"}},
				}}}})))
		case "/category/comic-drama":
			w.Write([]byte(hongguoPage(t, map[string]any{"category_$": map[string]any{
				"recommendList": []map[string]any{{"series_id": "7706512650816982040", "series_name": "漫剧一"}},
				"pagination":    map[string]any{"total": 800, "pageNum": 1, "totalPages": 34},
			}})))
		default:
			http.NotFound(w, r)
		}
	})
	cards, err := client.Rank(context.Background(), "hot-drama", 2)
	if err != nil || len(cards) != 2 || cards[0].Form != "AI剧" || cards[1].Form != "" {
		t.Fatalf("榜单应该跳过只能预约的剧，并从标签里认出剧种: %+v err=%v", cards, err)
	}
	cards, pages, err := client.Category(context.Background(), "漫剧", 1)
	if err != nil || pages != 34 || len(cards) != 1 || cards[0].Form != "漫剧" {
		t.Fatalf("分类页解析不对: %+v pages=%d err=%v", cards, pages, err)
	}
	if _, _, err := client.Category(context.Background(), "动画", 1); err == nil {
		t.Error("未知的剧种应该报错")
	}
}

func TestHongguoSitemapsUseETag(t *testing.T) {
	requests := 0
	var client *providers.Hongguo
	client = newHongguo(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/sitemap/hongguoduanju/index.xml":
			w.Write([]byte(`<sitemapindex><sitemap><loc>` + client.Base + `/sitemap/hongguoduanju/index1.xml</loc></sitemap>` +
				`<sitemap><loc>https://elsewhere.example/x.xml</loc></sitemap></sitemapindex>`))
		case "/sitemap/hongguoduanju/index1.xml":
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			w.Write([]byte(`<urlset>
<url><loc>https://hongguoduanju.com/detail?series_id=7654257687307226136</loc><lastmod>2026-07-07</lastmod></url>
<url><loc>https://hongguoduanju.com/player/7352816485586177086</loc></url>
<url><loc>https://hongguoduanju.com/detail?series_id=7593982642807114776</loc></url>
<url><loc>https://hongguoduanju.com/detail?series_id=7654257687307226136</loc></url>
</urlset>`))
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	maps, err := client.Sitemaps(ctx)
	if err != nil || len(maps) != 1 || maps[0] != "/sitemap/hongguoduanju/index1.xml" {
		t.Fatalf("应该只留下本站的子站点地图: %v err=%v", maps, err)
	}
	ids, etag, err := client.SitemapIDs(ctx, maps[0], "")
	if err != nil || etag != `"v1"` || strings.Join(ids, ",") != "7654257687307226136,7593982642807114776" {
		t.Fatalf("应该只取详情页的 ID 并去重: %v etag=%s err=%v", ids, etag, err)
	}
	if _, _, err := client.SitemapIDs(ctx, maps[0], etag); !errors.Is(err, providers.ErrNotModified) {
		t.Errorf("带上 ETag 没有变化时应该返回 ErrNotModified，实际 %v", err)
	}
}

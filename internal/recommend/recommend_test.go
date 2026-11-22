package recommend_test

import (
	"context"
	"testing"
	"time"

	"moovie/internal/db/testdb"
	"moovie/internal/media"
	"moovie/internal/recommend"
)

func vector(x float32) []float32 {
	v := make([]float32, 768)
	v[0], v[1] = x, 1-x
	return v
}

func TestRecommendByEmbedding(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	testdb.Media(t, pool, 10, 11, 12, 13)
	store := media.NewStore(pool)
	ctx := context.Background()
	pool.Exec(ctx, `UPDATE media SET poster = 'p', series_key = 'k' || id`)
	store.SetEmbedding(ctx, 10, "", vector(1))    // 用户喜欢的
	store.SetEmbedding(ctx, 11, "", vector(0.95)) // 很接近
	store.SetEmbedding(ctx, 12, "", vector(0.1))  // 很远
	store.SetEmbedding(ctx, 13, "", vector(0.9))  // 接近，但用户已经在看
	pool.Exec(ctx, `INSERT INTO user_movies (user_id, media_id, status, rating) VALUES (1, 10, 'watched', 5)`)
	pool.Exec(ctx, `INSERT INTO playback_positions (user_id, media_id, activity_at) VALUES (1, 13, now())`)

	service := recommend.NewService(pool, store)
	ids, err := service.Compute(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	// 向量最接近的排在前面；已经标记过和正在看的不出现。
	if len(ids) != 2 || ids[0] != 11 || ids[1] != 12 {
		t.Fatalf("应该按向量距离推荐 11、12: %v", ids)
	}
}

func TestRecommendFallsBackToGenresThenCharts(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1, 2)
	ids := []int64{10, 11, 12, 13, 14, 15, 16, 17, 20, 21}
	testdb.Media(t, pool, ids...)
	store := media.NewStore(pool)
	ctx := context.Background()
	pool.Exec(ctx, `UPDATE media SET poster = 'p', rating_douban = 8 + id * 0.01, genres = '{悬疑}' WHERE id BETWEEN 10 AND 17`)
	pool.Exec(ctx, `UPDATE media SET poster = 'p', rating_douban = 9.5, genres = '{喜剧}' WHERE id >= 20`)
	pool.Exec(ctx, `INSERT INTO user_movies (user_id, media_id, status) VALUES (1, 10, 'wish')`)
	store.ReplaceChart(ctx, "movie", []int64{20, 21, 10})

	service := recommend.NewService(pool, store)
	// 用户 1 标记过悬疑：先推荐其余 7 部悬疑（评分高的在前），再用热门榜补充；不包含已标记的 10。
	got, err := service.Compute(ctx, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 9 || got[0] != 17 || got[6] != 11 || got[7] != 20 || got[8] != 21 {
		t.Fatalf("应该是 7 部悬疑加 2 部热门榜: %v", got)
	}
	for _, id := range got {
		if id == 10 {
			t.Error("推荐里不应该有已经标记过的作品")
		}
	}
	// 用户 2 没有任何标记：用热门榜。
	got, err = service.Compute(ctx, 2, 20)
	if err != nil || len(got) != 3 || got[0] != 20 {
		t.Fatalf("没有标记时应该用热门榜: %v err=%v", got, err)
	}

	// ForUser 会缓存结果；标记变化后调用 Invalidate 才会重新计算。
	cards, err := service.ForUser(ctx, 2, 2)
	if err != nil || len(cards) != 2 {
		t.Fatalf("ForUser 应该返回 2 张卡片: %d err=%v", len(cards), err)
	}
	// 首页用 ForUserWithin：缓存里有就直接给。
	if cards, ok, err := service.ForUserWithin(ctx, 2, 2, time.Millisecond); err != nil || !ok || len(cards) != 2 {
		t.Fatalf("缓存命中时 ForUserWithin 应该直接返回: ok=%v %d err=%v", ok, len(cards), err)
	}
	store.ReplaceChart(ctx, "movie", []int64{21})
	if cards, _ := service.ForUser(ctx, 2, 10); len(cards) != 3 {
		t.Errorf("缓存有效期内应该还是原来的 3 部，实际 %d", len(cards))
	}
	service.Invalidate(ctx, 2)
	if cards, _ := service.ForUser(ctx, 2, 10); len(cards) != 1 {
		t.Errorf("失效后应该重新计算，实际 %d", len(cards))
	}
}

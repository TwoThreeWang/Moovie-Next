package playback_test

import (
	"context"
	"testing"
	"time"

	"moovie/internal/db/testdb"
	"moovie/internal/playback"
)

func TestUpsertKeepsNewestActivity(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	testdb.Media(t, pool, 10)
	store := playback.NewStore(pool)
	ctx := context.Background()
	now := time.Now()
	at := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }

	written, err := store.Upsert(ctx, 1, []playback.Position{{MediaID: 10, Season: 1, Episode: 3, Position: 600, Duration: 2700, At: at(-time.Hour)}})
	if err != nil || written != 1 {
		t.Fatalf("应该写入 1 条: %d %v", written, err)
	}
	// 更晚的一次播放，进度更小（用户往回拖了）：以时间为准，接受。
	written, _ = store.Upsert(ctx, 1, []playback.Position{{MediaID: 10, Season: 1, Episode: 3, Position: 120, Duration: 2700, At: at(-time.Minute)}})
	if written != 1 {
		t.Error("更晚的播放应该覆盖，即使进度更小")
	}
	// 更早的一次播放（另一台设备迟到的上报）：拒绝。
	written, _ = store.Upsert(ctx, 1, []playback.Position{{MediaID: 10, Season: 1, Episode: 3, Position: 2000, Duration: 2700, At: at(-2 * time.Hour)}})
	if written != 0 {
		t.Error("更早的播放不应该覆盖较新的进度")
	}
	positions, err := store.ForMedia(ctx, 1, 10)
	if err != nil || len(positions) != 1 || positions[0].Position != 120 {
		t.Fatalf("进度应该是 120: %+v err=%v", positions, err)
	}
}

func TestUpsertSkipsInvalidAndUnknownMedia(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	testdb.Media(t, pool, 10)
	store := playback.NewStore(pool)
	written, err := store.Upsert(context.Background(), 1, []playback.Position{
		{MediaID: 999, Season: 1, Episode: 1, Position: 10, Duration: 100, At: time.Now().UnixMilli()}, // 作品不存在
		{MediaID: 10, Season: 1, Episode: 1, Position: -5, Duration: 100},                              // 进度为负
		{MediaID: 10, Season: 1, Episode: 9999, Position: 5, Duration: 100},                            // 集数离谱
		{MediaID: 10, Season: 1, Episode: 1, Position: 30, Duration: 100},                              // 合法，没带时间
	})
	if err != nil || written != 1 {
		t.Fatalf("只有最后一条应该写入: %d %v", written, err)
	}
}

func TestRecentAndDelete(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1, 2)
	testdb.Media(t, pool, 10, 11)
	store := playback.NewStore(pool)
	ctx := context.Background()
	now := time.Now()
	store.Upsert(ctx, 1, []playback.Position{
		{MediaID: 10, Season: 1, Episode: 1, Position: 2600, Duration: 2700, Completed: true, At: now.Add(-3 * time.Hour).UnixMilli()},
		{MediaID: 10, Season: 1, Episode: 2, Position: 300, Duration: 2700, At: now.Add(-2 * time.Hour).UnixMilli()},
		{MediaID: 11, Season: 1, Episode: 1, Position: 50, Duration: 6000, At: now.Add(-time.Hour).UnixMilli()},
	})
	store.Upsert(ctx, 2, []playback.Position{{MediaID: 10, Season: 1, Episode: 5, Position: 1, Duration: 2, At: now.UnixMilli()}})

	if episodes, err := store.ForMedia(ctx, 1, 10); err != nil || len(episodes) != 2 || episodes[0].Episode != 2 {
		t.Fatalf("用户 1 在作品 10 上应该有 2 集的进度，最近看的在前: %+v err=%v", episodes, err)
	}
	recent, err := store.Recent(ctx, 1, 10, 0)
	if err != nil || len(recent) != 2 || recent[0].Card.ID != 11 || recent[1].Episode != 2 {
		t.Fatalf("继续观看：每部作品只取最近的一集，最近看的在前: %+v err=%v", recent, err)
	}
	if err := store.Delete(ctx, 1, 10); err != nil {
		t.Fatal(err)
	}
	if left, _ := store.ForMedia(ctx, 1, 10); len(left) != 0 {
		t.Errorf("删除后这部作品不应该还有进度: %+v", left)
	}
	if others, _ := store.ForMedia(ctx, 2, 10); len(others) != 1 {
		t.Errorf("不能删到别人的进度: %+v", others)
	}
	if recent, _ := store.Recent(ctx, 1, 10, 0); len(recent) != 1 {
		t.Errorf("删除后继续观看里应该只剩 1 部，实际 %d", len(recent))
	}
	// 删除后重新播放，记录恢复。
	store.Upsert(ctx, 1, []playback.Position{{MediaID: 10, Season: 1, Episode: 2, Position: 400, Duration: 2700, At: time.Now().UnixMilli()}})
	if recent, _ := store.Recent(ctx, 1, 10, 0); len(recent) != 2 {
		t.Errorf("重新播放后应该恢复，实际 %d", len(recent))
	}
	var days int
	pool.QueryRow(ctx, `SELECT count(*) FROM watch_days WHERE user_id = 1`).Scan(&days)
	if days < 1 {
		t.Error("应该记录观影日期")
	}
}

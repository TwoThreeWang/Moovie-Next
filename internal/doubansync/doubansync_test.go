package doubansync_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"moovie/internal/auth"
	"moovie/internal/db/testdb"
	"moovie/internal/doubansync"
	"moovie/internal/ingest"
	"moovie/internal/jobs"
	"moovie/internal/library"
	"moovie/internal/media"
	"moovie/internal/providers/providerstest"
)

func TestBindAndSync(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	fake := providerstest.New(t)
	queue := jobs.NewQueue(pool)
	mediaStore := media.NewStore(pool)
	catalog := &ingest.Service{Media: mediaStore, Queue: queue, Douban: fake.Douban(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	service := &doubansync.Service{Pool: pool, Users: auth.NewStore(pool), Douban: fake.Douban(), Catalog: catalog, Queue: queue}
	ctx := context.Background()

	if err := service.Bind(ctx, 1, "no/such user"); !errors.Is(err, doubansync.ErrInvalidUser) {
		t.Errorf("格式不对的豆瓣 ID 应该被拒绝，实际 %v", err)
	}
	if err := service.Bind(ctx, 1, "ghost"); !errors.Is(err, doubansync.ErrInvalidUser) {
		t.Errorf("豆瓣上不存在的用户应该被拒绝，实际 %v", err)
	}

	// 60 条看过（跨两页）和 2 条想看。
	done := []map[string]any{}
	for index := range 60 {
		done = append(done, map[string]any{
			"comment": fmt.Sprintf("短评 %d", index), "create_time": "2026-10-03 21:00:00", "rating": map[string]any{"value": 4},
			"subject": map[string]any{"id": fmt.Sprintf("31%06d", index), "title": fmt.Sprintf("电影 %d", index), "type": "movie", "year": "2020"},
		})
	}
	fake.Interests["evan/done"] = done
	fake.Interests["evan/mark"] = []map[string]any{
		{"subject": map[string]any{"id": "32000001", "title": "想看一", "type": "tv"}},
		{"subject": map[string]any{"id": "31000000", "title": "电影 0", "type": "movie"}}, // 已经在看过里，不应该被降级
	}
	if err := service.Bind(ctx, 1, "evan"); err != nil {
		t.Fatal(err)
	}

	// 本站已有的标记：一个是想看（应该被升级成看过），一个是看过（保持原样）。
	first, _, _ := mediaStore.EnsureDouban(ctx, "31000001", nil)
	second, _, _ := mediaStore.EnsureDouban(ctx, "31000002", nil)
	lib := library.NewStore(pool)
	lib.Save(ctx, 1, first, library.StatusWish, 0, "")
	lib.Save(ctx, 1, second, library.StatusWatched, 2, "本站写的短评")

	var last doubansync.Progress
	if err := service.Sync(ctx, 1, true, func(p doubansync.Progress) { last = p }); err != nil {
		t.Fatal(err)
	}
	if last.Total != 62 || last.Processed != 62 || last.Imported != 60 {
		t.Errorf("进度不对（60 条新增或升级，1 条已有的看过保持不变，1 条想看被已有的看过挡住）: %+v", last)
	}
	wish, watched, _ := lib.Counts(ctx, 1)
	if wish != 1 || watched != 60 {
		t.Errorf("同步后应该有 60 部看过、1 部想看，实际 %d / %d", watched, wish)
	}
	if entry, _, _ := lib.Get(ctx, 1, first); entry.Status != library.StatusWatched || entry.Rating != 4 {
		t.Errorf("本站的想看应该被升级成看过: %+v", entry)
	}
	if entry, _, _ := lib.Get(ctx, 1, second); entry.Rating != 2 || entry.Comment != "本站写的短评" {
		t.Errorf("本站已有的看过不应该被覆盖: %+v", entry)
	}
	// 再同步一次不会重复导入。
	last = doubansync.Progress{}
	service.Sync(ctx, 1, true, func(p doubansync.Progress) { last = p })
	if last.Imported != 0 || last.Processed != 62 {
		t.Errorf("完整同步应该把 62 条都读一遍但不再导入，实际 %+v", last)
	}
	// 增量同步：第一页（50 条）都已经导入过，后面的 10 条不用再读。
	last = doubansync.Progress{}
	service.Sync(ctx, 1, false, func(p doubansync.Progress) { last = p })
	if last.Imported != 0 || last.Processed != 52 {
		t.Errorf("增量同步读完第一页就该停（看过 50 条 + 想看 2 条），实际 %+v", last)
	}
	// 豆瓣上新标记了一部：增量同步能发现它。
	fake.Interests["evan/done"] = append([]map[string]any{{
		"create_time": "2026-11-01 20:00:00", "rating": map[string]any{"value": 5},
		"subject": map[string]any{"id": "31999999", "title": "新看的", "type": "movie", "year": "2026"},
	}}, done...)
	last = doubansync.Progress{}
	service.Sync(ctx, 1, false, func(p doubansync.Progress) { last = p })
	if last.Imported != 1 {
		t.Errorf("增量同步应该导入新标记的那一部，实际 %+v", last)
	}

	// 每天自动的那次排在最后；用户自己点同步时，把它提前并换成完整同步。
	if err := service.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	if job, found, _ := service.Queue.Latest(ctx, doubansync.KindSync, "1"); !found || job.Priority >= 0 || string(job.Payload) != `{"full": false}` {
		t.Errorf("每天自动的同步应该是低优先级的增量同步: %+v", job)
	}
	if started, _ := service.Start(ctx, 1); started {
		t.Error("已经有一个在排队，不应该再新建一个")
	}
	if job, _, _ := service.Queue.Latest(ctx, doubansync.KindSync, "1"); job.Priority <= 0 || string(job.Payload) != `{"full": true}` {
		t.Errorf("用户点同步后，排着的那个应该被提前并换成完整同步: %+v", job)
	}
	pool.Exec(ctx, `DELETE FROM jobs`)

	if started, err := service.Start(ctx, 1); err != nil || !started {
		t.Errorf("应该能把同步任务放进队列: %v %v", started, err)
	}
	if started, _ := service.Start(ctx, 1); started {
		t.Error("已经在排队时不应该重复入队")
	}
	service.Unbind(ctx, 1)
	if err := service.Sync(ctx, 1, true, nil); !errors.Is(err, jobs.ErrSkip) {
		t.Errorf("解绑后的同步任务应该被跳过，实际 %v", err)
	}
}

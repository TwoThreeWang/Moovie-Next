package doubansync_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"moovie/internal/auth"
	"moovie/internal/db/testdb"
	"moovie/internal/doubansync"
	"moovie/internal/ingest"
	"moovie/internal/jobs"
	"moovie/internal/library"
	"moovie/internal/media"
	"moovie/internal/providers"
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

	// 每天自动的那次是增量同步；用户自己点同步时，把它提前并换成完整同步。
	if err := service.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	if job, found, _ := service.Queue.Latest(ctx, doubansync.KindSync, "1"); !found || job.Priority != ingest.PriorityDailySync || string(job.Payload) != `{"full": false}` {
		t.Errorf("每天自动的同步应该是增量同步: %+v", job)
	}
	if started, _ := service.Start(ctx, 1); started {
		t.Error("已经有一个在排队，不应该再新建一个")
	}
	if job, _, _ := service.Queue.Latest(ctx, doubansync.KindSync, "1"); job.Priority != ingest.PriorityManual || string(job.Payload) != `{"full": true}` {
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

// 每天的增量同步先看 RSS：上一次同步之后没有新的看过或想看，就一页标记列表都不用读；
// 只有想看有新动态，就只读想看。RSS 说明不了问题时（读不到、覆盖不到上一次同步），照原来的办法翻标记列表。
func TestIncrementalSyncChecksFeedFirst(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	fake := providerstest.New(t)
	queue := jobs.NewQueue(pool)
	catalog := &ingest.Service{Media: media.NewStore(pool), Queue: queue, Douban: fake.Douban(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	service := &doubansync.Service{Pool: pool, Users: auth.NewStore(pool), Douban: fake.Douban(), Catalog: catalog, Queue: queue}
	ctx := context.Background()

	fake.Interests["evan/done"] = []map[string]any{{"subject": map[string]any{"id": "31000001", "title": "看过一", "type": "movie"}}}
	fake.Interests["evan/mark"] = []map[string]any{{"subject": map[string]any{"id": "32000001", "title": "想看一", "type": "movie"}}}
	if err := service.Bind(ctx, 1, "evan"); err != nil {
		t.Fatal(err)
	}
	const interests = "/rexxar/api/v2/user/evan/interests"
	feedTime := func(ago time.Duration) string { return time.Now().Add(-ago).UTC().Format(time.RFC1123) }
	sync := func() doubansync.Progress {
		t.Helper()
		var last doubansync.Progress
		if err := service.Sync(ctx, 1, false, func(p doubansync.Progress) { last = p }); err != nil {
			t.Fatal(err)
		}
		return last
	}
	old := map[string]string{"title": "看过看过一", "link": "https://movie.douban.com/subject/31000001/", "pubDate": feedTime(72 * time.Hour)}
	fake.Feeds["evan"] = []map[string]string{old}

	// 还没成功同步过：不看 RSS，两种状态都读。
	before := fake.Count(interests)
	if progress := sync(); progress.Total != 2 || fake.Count(interests)-before != 2 {
		t.Fatalf("没同步过时应该把看过和想看都读一遍: %+v，请求了 %d 次", progress, fake.Count(interests)-before)
	}
	if fake.Count("/feed/people/evan/interests") != 0 {
		t.Error("没同步过时不用看 RSS")
	}

	// 昨天同步过，RSS 里最新的动态是三天前的：一页都不用读。
	pool.Exec(ctx, `INSERT INTO jobs (kind, subject, status, run_at, finished_at) VALUES ($1, '1', 'done', now() - interval '1 day', now() - interval '1 day')`, doubansync.KindSync)
	before = fake.Count(interests)
	sync()
	if fake.Count(interests) != before {
		t.Errorf("没有新动态时不该再读标记列表，多请求了 %d 次", fake.Count(interests)-before)
	}

	// 同步之后新标记了一部想看：只读想看。
	fake.Interests["evan/mark"] = append([]map[string]any{{"subject": map[string]any{"id": "32000002", "title": "想看二", "type": "movie"}}}, fake.Interests["evan/mark"]...)
	fake.Feeds["evan"] = []map[string]string{
		{"title": "想看想看二", "link": "https://movie.douban.com/subject/32000002/", "pubDate": feedTime(time.Hour)},
		{"title": "想读某本书", "link": "https://book.douban.com/subject/1000001/", "pubDate": feedTime(2 * time.Hour)},
		old,
	}
	before = fake.Count(interests)
	if progress := sync(); progress.Total != 2 || progress.Imported != 1 || fake.Count(interests)-before != 1 {
		t.Errorf("只有想看有新动态时应该只读想看: %+v，请求了 %d 次", progress, fake.Count(interests)-before)
	}

	// RSS 里最早的一条也比上一次同步晚：更早的可能被挤掉了，两种都读。
	fake.Feeds["evan"] = []map[string]string{{"title": "想读某本书", "link": "https://book.douban.com/subject/1000001/", "pubDate": feedTime(time.Minute)}}
	before = fake.Count(interests)
	sync()
	if fake.Count(interests)-before != 2 {
		t.Errorf("RSS 覆盖不到上一次同步时应该两种都读，请求了 %d 次", fake.Count(interests)-before)
	}

	// RSS 读不到：两种都读。
	delete(fake.Feeds, "evan")
	before = fake.Count(interests)
	sync()
	if fake.Count(interests)-before != 2 {
		t.Errorf("RSS 读不到时应该两种都读，请求了 %d 次", fake.Count(interests)-before)
	}

	// 换绑豆瓣账号后，旧账号的同步记录不再算数。
	fake.Feeds["evan"] = []map[string]string{old}
	if err := service.Bind(ctx, 1, "evan"); err != nil {
		t.Fatal(err)
	}
	before = fake.Count(interests)
	sync()
	if fake.Count(interests)-before != 2 {
		t.Errorf("重新绑定后的第一次增量同步应该两种都读，请求了 %d 次", fake.Count(interests)-before)
	}
}

// failingCatalog 让指定的条目收不进作品库，模拟数据库出错这类下次还能成功的失败。
type failingCatalog struct {
	doubansync.Catalog
	fail map[string]bool
}

func (c *failingCatalog) EnsureDouban(ctx context.Context, item providers.DoubanItem, chartType string, priority int) (int64, error) {
	if c.fail[item.ID] {
		return 0, errors.New("数据库暂时不可用")
	}
	return c.Catalog.EnsureDouban(ctx, item, chartType, priority)
}

// 有条目没导入成功的同步之后，下一次同步不看 RSS、也不读到整页已导入就停，而是完整地读一遍，
// 否则那一条在列表里靠后、又早于上一次同步，会被永远漏掉。直到有一次同步没有失败，才恢复增量。
// 重试中的任务（以前跑过一部分）也完整地读。不合法的条目 ID 收不进来，不算失败。
func TestSyncAfterFailuresReadsEverything(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	fake := providerstest.New(t)
	queue := jobs.NewQueue(pool)
	inner := &ingest.Service{Media: media.NewStore(pool), Queue: queue, Douban: fake.Douban(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	catalog := &failingCatalog{Catalog: inner, fail: map[string]bool{"31000055": true}}
	service := &doubansync.Service{Pool: pool, Users: auth.NewStore(pool), Douban: fake.Douban(), Catalog: catalog, Queue: queue}
	lib := library.NewStore(pool)
	ctx := context.Background()

	done := []map[string]any{{"subject": map[string]any{"id": "12", "title": "坏 ID", "type": "movie"}}}
	for index := range 60 {
		done = append(done, map[string]any{"subject": map[string]any{"id": fmt.Sprintf("31%06d", index), "title": fmt.Sprintf("电影 %d", index), "type": "movie"}})
	}
	fake.Interests["evan/done"] = done
	fake.Interests["evan/mark"] = []map[string]any{}
	if err := service.Bind(ctx, 1, "evan"); err != nil {
		t.Fatal(err)
	}
	fake.Feeds["evan"] = []map[string]string{{"title": "看过电影 0", "link": "https://movie.douban.com/subject/31000000/", "pubDate": time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC1123)}}
	finish := func(progress doubansync.Progress, ago string) {
		encoded, _ := json.Marshal(progress)
		pool.Exec(ctx, `INSERT INTO jobs (kind, subject, status, run_at, finished_at, progress) VALUES ($1, '1', 'done', now() - $2::interval, now() - $2::interval, $3)`,
			doubansync.KindSync, ago, encoded)
	}
	var last doubansync.Progress
	if err := service.Sync(ctx, 1, true, func(p doubansync.Progress) { last = p }); err != nil {
		t.Fatal(err)
	}
	if last.Failed != 1 || last.Imported != 59 {
		t.Fatalf("应该有 1 条没导入成功（坏 ID 不算失败）: %+v", last)
	}
	finish(last, "2 days")

	// 数据库恢复了。RSS 说没有新动态、第一页也都导入过，但上一次有失败：完整地读，把漏的那条补上。
	delete(catalog.fail, "31000055")
	last = doubansync.Progress{}
	if err := service.Sync(ctx, 1, false, func(p doubansync.Progress) { last = p }); err != nil {
		t.Fatal(err)
	}
	if last.Imported != 1 || last.Processed != 61 || last.Failed != 0 {
		t.Fatalf("上一次有失败时应该完整地读一遍、补上漏的那条: %+v", last)
	}
	finish(last, "1 day")

	// 这一次没有失败了：恢复增量，RSS 说没有新动态就一页都不读。
	const interests = "/rexxar/api/v2/user/evan/interests"
	before := fake.Count(interests)
	if err := service.Sync(ctx, 1, false, nil); err != nil {
		t.Fatal(err)
	}
	if fake.Count(interests) != before {
		t.Errorf("没有失败、也没有新动态时不该读标记列表，多请求了 %d 次", fake.Count(interests)-before)
	}

	// 重试中的任务：以前跑过一部分，前面已经导入了，后面那一页里有一条还没导入。完整地读才能找到它。
	pool.Exec(ctx, `DELETE FROM jobs`)
	missing, _, _ := inner.Media.EnsureDouban(ctx, "31000058", nil)
	pool.Exec(ctx, `DELETE FROM user_movies WHERE user_id = 1 AND media_id = $1`, missing)
	pool.Exec(ctx, `INSERT INTO jobs (kind, subject, payload, progress) VALUES ($1, '1', '{"full": false}', '{"processed": 50}')`, doubansync.KindSync)
	worker := jobs.NewWorker(queue, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service.Register(worker)
	if worked, err := worker.RunOne(ctx); err != nil || !worked {
		t.Fatalf("应该跑完重试的任务: %v %v", worked, err)
	}
	if _, found, _ := lib.Get(ctx, 1, missing); !found {
		t.Error("重试的任务应该完整地读，把后面那一页里没导入的补上")
	}
}

// 豆瓣的电影、剧集详情都停着（IP 被标记）时，想看、看过照样同步进来：标记列表走自己的节奏，
// 新收进来的作品马上排建档（不用豆瓣），豆瓣详情排在后面慢慢补。
func TestSyncWhileDoubanDetailsPaused(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1)
	fake := providerstest.New(t)
	queue := jobs.NewQueue(pool)
	mediaStore := media.NewStore(pool)
	douban := fake.Douban()
	douban.MoviePace, douban.TVPace = providers.NewPacer(time.Millisecond), providers.NewPacer(time.Millisecond)
	douban.MoviePace.Flagged()
	douban.TVPace.Flagged()
	catalog := &ingest.Service{Media: mediaStore, Queue: queue, Douban: douban, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	service := &doubansync.Service{Pool: pool, Users: auth.NewStore(pool), Douban: douban, Catalog: catalog, Queue: queue}
	ctx := context.Background()
	fake.Interests["evan/done"] = []map[string]any{{"subject": map[string]any{"id": "31000001", "title": "看过一", "type": "movie", "year": "2020"}}}
	fake.Interests["evan/mark"] = []map[string]any{{"subject": map[string]any{"id": "32000001", "title": "想看一", "type": "tv"}}}
	if err := service.Bind(ctx, 1, "evan"); err != nil {
		t.Fatal(err)
	}
	if err := service.Sync(ctx, 1, true, func(doubansync.Progress) {}); err != nil {
		t.Fatal(err)
	}
	wish, watched, _ := library.NewStore(pool).Counts(ctx, 1)
	if wish != 1 || watched != 1 {
		t.Errorf("详情停着也应该同步标记: 想看 %d 看过 %d", wish, watched)
	}
	m, err := mediaStore.ByDoubanID(ctx, "31000001")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := queue.Latest(ctx, ingest.KindEnrich, strconv.FormatInt(m.ID, 10)); !found || m.Title != "看过一" {
		t.Errorf("新收进来的作品应该带着列表里的片名、马上排建档: %+v", m)
	}
}

package ingest_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"moovie/internal/db/testdb"
	"moovie/internal/ingest"
	"moovie/internal/jobs"
	"moovie/internal/media"
)

func TestTMDBFillsTitleAndRejectsAmbiguousSearch(t *testing.T) {
	service, fake := newService(t)
	ctx := context.Background()
	known, _, err := service.Media.EnsureDouban(ctx, "42000021", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Media.SetExternalID(ctx, known, media.ProviderTMDB, "movie/603"); err != nil {
		t.Fatal(err)
	}
	fake.TMDB["/3/movie/603"] = map[string]any{"title": "黑客帝国", "release_date": "1999-03-31"}
	if err := service.Enrich(ctx, known, jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	m, err := service.Media.Get(ctx, known)
	if err != nil || m.Title != "黑客帝国" || m.Year != 1999 {
		t.Fatalf("可信 TMDB 编号应补片名和年份: %+v %v", m, err)
	}
	ambiguous, _, err := service.Media.EnsureDouban(ctx, "42000022", media.Patch{"title": "同名电影"})
	if err != nil {
		t.Fatal(err)
	}
	fake.TMDB["/3/search/movie"] = map[string]any{"results": []map[string]any{
		{"id": 604, "title": "同名电影"}, {"id": 605, "original_title": "同名电影"},
	}}
	if err := service.Enrich(ctx, ambiguous, jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	m, err = service.Media.Get(ctx, ambiguous)
	if err != nil || m.TMDBID != "" || m.Year != 0 || fake.Count("/3/movie/604") != 0 {
		t.Fatalf("同名候选有多个时应放弃匹配: %+v %v", m, err)
	}
}

func TestTMDBSeriesSweepMatchesSeasonAndUpdatesOnlyTMDB(t *testing.T) {
	service, fake := newService(t)
	seedShow(fake)
	pool := testdb.Pool(t)
	ctx := context.Background()
	today := media.Today()
	old := today.AddDate(0, -1, 0)
	fake.TMDB["/3/tv/4455"] = map[string]any{
		"name": "雾港", "status": "Returning Series",
		"seasons": []map[string]any{
			{"season_number": 1, "episode_count": 2, "air_date": old.AddDate(-1, 0, 0).Format("2006-01-02")},
			{"season_number": 2, "episode_count": 3, "air_date": old.Format("2006-01-02")},
		},
	}
	fake.TMDB["/3/tv/4455/season/1"] = map[string]any{
		"episodes": []map[string]any{{"episode_number": 1, "name": "第一集"}},
	}
	fake.TMDB["/3/tv/4455/season/2"] = map[string]any{
		"episodes": []map[string]any{{"episode_number": 1, "name": "第二季第一集"}},
	}
	insert := func(doubanID, kind string, season, year int, last time.Time) int64 {
		t.Helper()
		id, _, err := service.Media.EnsureDouban(ctx, doubanID, media.Patch{
			"media_type": kind, "title": "雾港", "year": year, "season": season, "episode_count": 12, "series_status": "ongoing",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Media.SetExternalID(ctx, id, media.ProviderTMDB, "tv/4455"); err != nil {
			t.Fatal(err)
		}
		if err := service.Media.MarkEnriched(ctx, id, time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "UPDATE media SET tmdb_series_checked_at = $2 WHERE id = $1", id, last); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := insert("42000031", media.TypeTV, 1, old.Year()-1, today.Add(-72*time.Hour))
	second := insert("42000032", media.TypeTV, 2, old.Year(), today.Add(-48*time.Hour))
	fresh := insert("42000033", media.TypeTV, 2, old.Year(), time.Now())
	wrong := insert("42000034", media.TypeAnime, 1, old.Year(), today.Add(-48*time.Hour))
	want := []int64{first, second, wrong}
	got, err := service.Media.NeedsTMDBSeries(ctx, ingest.KindTMDBSeries, ingest.KindEnrich, 10)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("应先挑最久未查的季: %v %v", got, err)
	}
	if err := service.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range want {
		job, found, err := service.Queue.Latest(ctx, ingest.KindTMDBSeries, strconv.FormatInt(id, 10))
		if err != nil || !found || job.Status != "queued" {
			t.Fatalf("应排独立 TMDB 季任务: %+v %v", job, err)
		}
		if err := service.RefreshTMDBSeries(ctx, id, job); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.RefreshTMDBSeries(ctx, fresh, jobs.Job{}); !errors.Is(err, jobs.ErrSkip) {
		t.Fatalf("当天查过的季应跳过: %v", err)
	}
	for _, tc := range []struct {
		id           int64
		status       string
		count, units int
	}{
		{first, "ended", 2, 1},
		{second, "ongoing", 3, 1},
		{wrong, "ongoing", 12, 0},
	} {
		m, err := service.Media.Get(ctx, tc.id)
		if err != nil || m.SeriesStatus != tc.status || m.EpisodeCount != tc.count || m.TMDBSeriesCheckedAt == nil ||
			m.TMDBSeasonUnmatched != (tc.id == wrong) {
			t.Fatalf("季状态或集数不对: %+v %v", m, err)
		}
		units, err := service.Media.Units(ctx, tc.id)
		if err != nil || len(units) != tc.units || (len(units) > 0 && units[0].Season != m.Season) {
			t.Fatalf("分集必须对应同一季: %+v %v", units, err)
		}
	}
	if fake.Count("/3/tv/4455/season/1") != 1 || fake.Count("/3/tv/4455/season/2") != 1 ||
		fake.Count("/rexxar") != 0 || fake.Count("/sparql") != 0 {
		t.Fatalf("巡检应跳过错季，且只查 TMDB: %v", fake.Requests)
	}
	fake.TMDB["/3/tv/4455/season/2"] = map[string]any{"episodes": []map[string]any{
		{"episode_number": 3, "episode_type": "finale", "air_date": old.Format("2006-01-02")},
	}}
	if _, err := pool.Exec(ctx, "UPDATE media SET tmdb_series_checked_at = $2 WHERE id = $1", second, today.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := service.RefreshTMDBSeries(ctx, second, jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	if m, err := service.Media.Get(ctx, second); err != nil || m.SeriesStatus != "ended" {
		t.Fatalf("整剧仍在续订时，也可根据已播出的季终确认当季完结: %+v %v", m, err)
	}
	if err := service.Queue.Drop(ctx, ingest.KindTMDBSeries, strconv.FormatInt(wrong, 10)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		age time.Duration
		due bool
	}{{48 * time.Hour, false}, {8 * 24 * time.Hour, true}} {
		if _, err := pool.Exec(ctx, "UPDATE media SET tmdb_series_checked_at = $2 WHERE id = $1", wrong, time.Now().Add(-tc.age)); err != nil {
			t.Fatal(err)
		}
		ids, err := service.Media.NeedsTMDBSeries(ctx, ingest.KindTMDBSeries, ingest.KindEnrich, 10)
		if err != nil || slices.Contains(ids, wrong) != tc.due {
			t.Fatalf("未匹配的季未遵守一周巡检间隔: %v %v", ids, err)
		}
		if err := service.RefreshTMDBSeries(ctx, wrong, jobs.Job{}); errors.Is(err, jobs.ErrSkip) == tc.due ||
			(err != nil && !errors.Is(err, jobs.ErrSkip)) {
			t.Fatalf("出队时应使用同样的巡检间隔: %v", err)
		}
	}
}

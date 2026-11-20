package report

import (
	"context"
	"errors"
	"testing"
	"time"

	"moovie/internal/db/testdb"
)

func TestLongestStreak(t *testing.T) {
	day := func(n int) time.Time { return time.Date(2026, 10, n, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		days []time.Time
		want int
	}{
		{nil, 0}, {[]time.Time{day(1)}, 1}, {[]time.Time{day(1), day(2), day(3), day(7), day(8)}, 3},
		{[]time.Time{day(1), day(3), day(5)}, 1},
	}
	for _, c := range cases {
		if got := longestStreak(c.days); got != c.want {
			t.Errorf("longestStreak(%v) = %d，期望 %d", c.days, got, c.want)
		}
	}
}

func TestMonthlyReport(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1, 2, 3)
	testdb.Media(t, pool, 10, 11, 12)
	ctx := context.Background()
	pool.Exec(ctx, `UPDATE media SET genres = '{悬疑,犯罪}', duration_minutes = 45, episode_count = 10, media_type = 'tv', poster = 'https://img1.doubanio.com/a.jpg' WHERE id = 10`)
	pool.Exec(ctx, `UPDATE media SET genres = '{悬疑}', duration_minutes = 120 WHERE id = 11`)
	pool.Exec(ctx, `UPDATE media SET genres = '{喜剧}', duration_minutes = 90 WHERE id = 12`)
	mark := func(userID, mediaID int64, rating int, at string) {
		if _, err := pool.Exec(ctx, `INSERT INTO user_movies (user_id, media_id, status, rating, updated_at) VALUES ($1, $2, 'watched', $3, $4::timestamptz)`, userID, mediaID, rating, at); err != nil {
			t.Fatal(err)
		}
	}
	mark(1, 10, 5, "2026-10-03 20:00:00+08")
	mark(1, 11, 4, "2026-10-10 20:00:00+08")
	mark(1, 12, 0, "2026-09-30 20:00:00+08") // 上个月的，不算
	mark(2, 10, 3, "2026-10-05 20:00:00+08") // 用户 2 这个月看了 1 部
	pool.Exec(ctx, `INSERT INTO watch_days (user_id, day) VALUES (1, '2026-10-03'), (1, '2026-10-04'), (1, '2026-10-10'), (1, '2026-09-30')`)

	service := NewService(pool)
	service.now = func() time.Time { return time.Date(2026, 11, 15, 12, 0, 0, 0, time.Local) }
	report, err := service.Get(ctx, 1, "2026-10")
	if err != nil {
		t.Fatal(err)
	}
	if report.WatchedCount != 2 || report.TotalMinutes != 45*10+120 || report.AverageRating != 4.5 {
		t.Errorf("数量、时长或均分不对: %+v", report)
	}
	if len(report.Genres) != 2 || report.Genres[0].Name != "悬疑" || report.Genres[0].Count != 2 {
		t.Errorf("类型统计不对: %+v", report.Genres)
	}
	if report.Top == nil || report.Top.MediaID != 10 || report.ActiveDays != 3 || report.LongestStreak != 2 {
		t.Errorf("最爱或观影天数不对: %+v", report)
	}
	if report.Percentile != 100 || report.PersonaTitle != "线索收集者" || len(report.Posters) != 1 {
		t.Errorf("排名、身份或海报墙不对: %+v", report)
	}
	if len(report.Posters) == 1 && (report.Posters[0].Title != "作品10" || report.Posters[0].URL != "https://img1.doubanio.com/a.jpg") {
		t.Errorf("海报必须保留作品片名用于图片备份命名: %+v", report.Posters)
	}
	if report.Label() != "2026 年 10 月" || report.Hours() != 9 {
		t.Errorf("展示用的方法不对: %q %d", report.Label(), report.Hours())
	}

	// 报告每次现算：数据变了，再打开就是新的（过去的月份也一样）。
	pool.Exec(ctx, `UPDATE user_movies SET status = 'watched', updated_at = '2026-10-20 20:00:00+08' WHERE user_id = 1 AND media_id = 12`)
	if fresh, err := service.Get(ctx, 1, "2026-10"); err != nil || fresh.WatchedCount != 3 {
		t.Errorf("再次打开应该包含新数据: %d err=%v", fresh.WatchedCount, err)
	}

	// 没有记录的用户得到一份空报告。
	empty, err := service.Get(ctx, 3, "2026-11")
	if err != nil || empty.WatchedCount != 0 || empty.PersonaTitle != "潜水观察员" {
		t.Errorf("空报告不对: %+v err=%v", empty, err)
	}
	var stored int
	pool.QueryRow(ctx, `SELECT count(*) FROM monthly_reports WHERE year_month = '2026-11'`).Scan(&stored)
	if stored != 0 {
		t.Error("当月的报告不应该保存")
	}
	for _, bad := range []string{"2026-13", "202610", "2027-01", ""} {
		if _, err := service.Get(ctx, 1, bad); !errors.Is(err, ErrInvalidMonth) {
			t.Errorf("月份 %q 应该被拒绝，实际 %v", bad, err)
		}
	}
	months, err := service.Months(ctx, 1, 12)
	if err != nil || len(months) != 1 || months[0] != "2026-10" {
		t.Errorf("有记录的月份不对: %v err=%v", months, err)
	}
}

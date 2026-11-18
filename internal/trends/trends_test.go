package trends

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"moovie/internal/db/testdb"
)

// 最近 1 天的窗口，现在是中午：今天全算，昨天算一半；上一个窗口是昨天的另一半加前天的一半。
func TestWindowWeights(t *testing.T) {
	current, previous := windowWeights(1, 0.5)
	if fmt.Sprint(current) != "[1 0.5 0]" || fmt.Sprint(previous) != "[0 0.5 0.5]" {
		t.Errorf("1 天窗口的权重不对: %v %v", current, previous)
	}
	current, previous = windowWeights(7, 0.25)
	sum := func(values []float64) (total float64) {
		for _, v := range values {
			total += v
		}
		return
	}
	// 现在过了一天的 1/4：最近 7×24 小时 = 今天已过去的部分 + 前 6 天 + 第 7 天的 3/4。
	if sum(current[:7]) != 7 || current[7] != 0.75 || previous[7] != 0.25 || sum(previous[8:14]) != 6 || previous[14] != 0.75 {
		t.Errorf("7 天窗口的权重不对: %v %v", current, previous)
	}
}

func TestTop(t *testing.T) {
	pool := testdb.Pool(t)
	store := NewStore(pool)
	ctx := context.Background()
	today := time.Now()
	noon := time.Date(today.Year(), today.Month(), today.Day(), 12, 0, 0, 0, time.Local)
	store.now = func() time.Time { return noon }
	put := func(keyword string, daysAgo, count int) {
		day := noon.AddDate(0, 0, -daysAgo).Format(time.DateOnly)
		if _, err := pool.Exec(ctx, `INSERT INTO search_counts (day, normalized, keyword, count) VALUES ($1, $2, $2, $3)`, day, keyword, count); err != nil {
			t.Fatal(err)
		}
	}
	put("雾港", 0, 6)
	put("雾港", 2, 4) // 上一个窗口只有前天的一半：2 次，今天明显上升
	put("山海食堂", 0, 3)
	put("山海食堂", 1, 2) // 最近 24 小时 = 3 + 2×0.5 = 4；上一个窗口 = 2×0.5 + 4×0.5 = 3：不算上升
	put("山海食堂", 2, 4)
	put("白夜", 0, 3) // 以前没出现过：新上榜
	put("只搜过一次", 0, 1)
	put("某某禁片", 0, 9)
	pool.Exec(ctx, `INSERT INTO blocked_keywords (kind, keyword) VALUES ('copyright', '禁片')`)

	top, err := store.Top(ctx, 1, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 || top[0].Keyword != "雾港" || top[0].Count != 6 || top[1].Keyword != "山海食堂" || top[1].Count != 4 || top[2].Keyword != "白夜" {
		t.Fatalf("热搜榜不对: %+v", top)
	}
	if !top[0].Rising || top[1].Rising || top[1].Previous != 3 || !top[2].New || top[0].Heat != 100 || top[1].Heat != 67 {
		t.Errorf("上升、新上榜和热度不对: %+v", top)
	}
	if week, _ := store.Top(ctx, 7, 2, 10); len(week) != 3 || week[1].Count != 9 {
		t.Errorf("本周应该把这几天的都算上（山海食堂 3+2+4=9）: %+v", week)
	}
}

// 搜索次数先在内存里累加，Flush 时一条语句写进库，已有的词加上新的次数。
func TestRecordAndFlush(t *testing.T) {
	pool := testdb.Pool(t)
	store := NewStore(pool)
	ctx := context.Background()
	for _, keyword := range []string{"白夜", "白夜", " 白夜 ", "  ", "这是一个" + strings.Repeat("非常", 20) + "长的搜索词"} {
		store.Record(ctx, keyword)
	}
	var rows, total int
	count := func() {
		pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(count), 0) FROM search_counts`).Scan(&rows, &total)
	}
	if count(); rows != 0 {
		t.Fatal("Flush 之前不应该写库")
	}
	if err := store.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	store.Record(ctx, "白夜")
	store.Flush(ctx)
	if count(); rows != 1 || total != 4 {
		t.Errorf("应该是一行、共 4 次（空的和太长的不记），实际 %d 行 %d 次", rows, total)
	}
	pool.Exec(ctx, `UPDATE search_counts SET day = current_date - 40`)
	if err := store.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if count(); rows != 0 {
		t.Error("31 天前的计数应该被删掉")
	}
}

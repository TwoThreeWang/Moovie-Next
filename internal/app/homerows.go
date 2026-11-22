package app

import (
	"context"
	"sync"
	"time"

	"moovie/internal/collections"
	"moovie/internal/media"
)

// chartCacheSize 是缓存的榜单长度：首页一行 24 部，顶部横幅从前 12 部里挑。
const chartCacheSize = 24

// chartCacheTTL 是榜单在内存里放多久。榜单由后台任务每天更新，后台任务可能在另一个进程里跑，
// 所以靠过期而不是通知来刷新；后台隐藏作品时会立刻清掉。
const chartCacheTTL = 5 * time.Minute

// forYouWait 是首页最多等"为你推荐"多久。缓存里有的时候几毫秒；要现算又算不完，就先放占位行，
// 计算在后台继续，占位行的请求会接着用上同一次计算的结果。
const forYouWait = 300 * time.Millisecond

// chartCache 缓存各类榜单的前 24 部。首页、横幅和公开的榜单行都从这里取，内容对所有人都一样。
type chartCache struct {
	mu      sync.Mutex
	entries map[string]chartEntry
}

type chartEntry struct {
	cards  []media.Card
	expiry time.Time
}

func (a *App) chart(ctx context.Context, kind string, limit int) ([]media.Card, error) {
	a.charts.mu.Lock()
	entry, ok := a.charts.entries[kind]
	a.charts.mu.Unlock()
	if !ok || time.Now().After(entry.expiry) {
		cards, err := a.Media.Chart(ctx, kind, chartCacheSize)
		if err != nil {
			if ok {
				a.Log.Warn("读取榜单失败，先用旧的", "kind", kind, "error", err)
				return firstN(entry.cards, limit), nil
			}
			return nil, err
		}
		entry = chartEntry{cards: cards, expiry: time.Now().Add(chartCacheTTL)}
		a.charts.mu.Lock()
		if a.charts.entries == nil {
			a.charts.entries = map[string]chartEntry{}
		}
		a.charts.entries[kind] = entry
		a.charts.mu.Unlock()
	}
	return firstN(entry.cards, limit), nil
}

// forgetCharts 清空榜单缓存（隐藏、恢复作品之后调用）。
func (a *App) forgetCharts() {
	a.charts.mu.Lock()
	a.charts.entries = nil
	a.charts.mu.Unlock()
}

// homeRow 是首页的一行：Lazy 不为空时先放占位行，滚动到附近再加载；否则直接写在页面里。
type homeRow struct {
	Lazy        string
	Class       string
	Row         RowView
	Collections []collections.Collection
}

func lazyRow(src, class string) homeRow { return homeRow{Lazy: src, Class: class} }

// eagerCount 是首屏第一行里不延迟加载的海报数：大致是一屏能看到的张数。
const eagerCount = 6

func markEager(cards []CardView) {
	for i := range firstN(cards, eagerCount) {
		cards[i].Eager = true
	}
}

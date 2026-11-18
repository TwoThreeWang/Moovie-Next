// Package trends 根据大家在作品库里的搜索生成热搜榜。
//
// 不保存每一次搜索，只按天给每个词计数（search_counts 一天一个词一行）：行数取决于每天有多少个不同的词，
// 和搜索次数无关。计数先在内存里累加，每分钟写一次库；进程重启最多丢一分钟的计数，对热搜榜没有影响。
package trends

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"moovie/internal/db"
	"moovie/internal/media"
)

// keepDays 是计数保留的天数：后台概览要画最近 30 天每天的搜索量，热搜榜的"本周"还要和上一周比。
const keepDays = 31

// maxPending 是内存里最多攒多少个不同的词，超过就提前写一次库。
const maxPending = 10000

// Store 读写 search_counts 表。
type Store struct {
	pool *db.Pool
	now  func() time.Time

	mu      sync.Mutex
	pending map[pendingKey]*pendingCount
}

type pendingKey struct {
	day        string // YYYY-MM-DD，按记录时的本地日期算
	normalized string
}

type pendingCount struct {
	keyword string
	count   int
}

// NewStore 创建 Store。要定期调用 Flush（Run 会做），否则计数只在内存里。
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool, now: time.Now, pending: map[pendingKey]*pendingCount{}}
}

// Record 记一次搜索。关键词太短、太长或者只有符号的不记。
func (s *Store) Record(ctx context.Context, keyword string) error {
	keyword = strings.TrimSpace(keyword)
	normalized := media.Normalize(keyword)
	if normalized == "" || len([]rune(keyword)) > 40 {
		return nil
	}
	key := pendingKey{day: s.now().Format(time.DateOnly), normalized: normalized}
	s.mu.Lock()
	if entry, ok := s.pending[key]; ok {
		entry.count++
	} else {
		s.pending[key] = &pendingCount{keyword: keyword, count: 1}
	}
	full := len(s.pending) >= maxPending
	s.mu.Unlock()
	if full {
		return s.Flush(ctx)
	}
	return nil
}

// Flush 把内存里的计数写进库：一条语句批量写，已有的词加上这次的次数。写失败时计数放回去，下次再写。
func (s *Store) Flush(ctx context.Context) error {
	s.mu.Lock()
	batch := s.pending
	s.pending = map[pendingKey]*pendingCount{}
	s.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	days := make([]string, 0, len(batch))
	normalized := make([]string, 0, len(batch))
	keywords := make([]string, 0, len(batch))
	counts := make([]int32, 0, len(batch))
	for key, entry := range batch {
		days, normalized = append(days, key.day), append(normalized, key.normalized)
		keywords, counts = append(keywords, entry.keyword), append(counts, int32(entry.count))
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO search_counts (day, normalized, keyword, count)
SELECT * FROM unnest($1::date[], $2::text[], $3::text[], $4::int[])
ON CONFLICT (day, normalized) DO UPDATE SET count = search_counts.count + EXCLUDED.count`, days, normalized, keywords, counts)
	if err != nil {
		s.mu.Lock()
		for key, entry := range batch {
			if existing, ok := s.pending[key]; ok {
				existing.count += entry.count
			} else {
				s.pending[key] = entry
			}
		}
		s.mu.Unlock()
	}
	return err
}

// Run 每分钟写一次库，直到 ctx 取消。退出前不再写：由调用方在服务停下后再调一次 Flush。
func (s *Store) Run(ctx context.Context, log *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Flush(ctx); err != nil && ctx.Err() == nil {
				log.Warn("热搜计数写入失败，下次再试", "error", err)
			}
		}
	}
}

// Keyword 是热搜榜上的一个词。
type Keyword struct {
	Keyword  string // 最近一天里的写法
	Count    int
	Previous int  // 上一个同样长度的时间段里的次数
	Rising   bool // 比上一个时间段明显上升
	New      bool // 上一个时间段没有出现过
	Heat     int  // 0–100，相对榜首的热度，用来画条
}

// windowWeights 算出"最近 days 天"和"再往前 days 天"两个滑动窗口里，每一天的计数各占多少。
//
// 库里只有按天的计数，窗口的两头各落在某一天的中间：今天已经过去的比例是 elapsed，
// 那么最近 24 小时 = 今天的全部 + 昨天的 (1 − elapsed)。这是常用的滑动窗口近似，
// 前提是一天里的搜索大致均匀，对热搜榜够用。返回值的下标 i 表示 i 天前（0 是今天）。
func windowWeights(days int, elapsed float64) (current, previous []float64) {
	current, previous = make([]float64, 2*days+1), make([]float64, 2*days+1)
	for i := range 2*days + 1 {
		switch {
		case i < days:
			current[i] = 1
		case i == days:
			current[i], previous[i] = 1-elapsed, elapsed
		case i < 2*days:
			previous[i] = 1
		default:
			previous[i] = 1 - elapsed
		}
	}
	return current, previous
}

// Top 返回最近 days 天（1 或 7）搜得最多的词。至少被搜过 minCount 次才上榜；命中屏蔽词的不上榜。
func (s *Store) Top(ctx context.Context, days, minCount, limit int) ([]Keyword, error) {
	now := s.now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	current, previous := windowWeights(days, now.Sub(midnight).Hours()/24)
	rows, err := s.pool.Query(ctx, `WITH weighted AS (
  SELECT normalized, keyword, day, count * ($3::float8[])[$1::date - day + 1] AS cur, count * ($4::float8[])[$1::date - day + 1] AS prev
  FROM search_counts WHERE day > $1::date - $2::int * 2 - 1 AND day <= $1::date
), totals AS (
  SELECT normalized, (array_agg(keyword ORDER BY day DESC))[1] AS keyword, round(sum(cur))::int AS n, round(sum(prev))::int AS p
  FROM weighted GROUP BY normalized
)
SELECT keyword, n, p FROM totals
WHERE n >= $5 AND NOT EXISTS (SELECT 1 FROM blocked_keywords b WHERE totals.keyword ILIKE '%' || b.keyword || '%')
ORDER BY n DESC, keyword LIMIT $6`, midnight.Format(time.DateOnly), days, current, previous, minCount, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Keyword{}
	for rows.Next() {
		var k Keyword
		if err := rows.Scan(&k.Keyword, &k.Count, &k.Previous); err != nil {
			return nil, err
		}
		k.New = k.Previous == 0
		k.Rising = !k.New && k.Count >= k.Previous*3/2+1
		list = append(list, k)
	}
	for index := range list {
		list[index].Heat = max(4, int(math.Round(float64(list[index].Count)*100/float64(list[0].Count))))
	}
	return list, rows.Err()
}

// Prune 删除 31 天前的计数。
func (s *Store) Prune(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM search_counts WHERE day < current_date - $1::int`, keepDays)
	return err
}

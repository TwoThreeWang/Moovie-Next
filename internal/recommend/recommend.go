// Package recommend 生成个性化推荐。
//
// 有向量时：把用户喜欢的作品的向量取平均，找最接近的、还没标记过的作品。
// 没有向量时：按用户最常看的类型挑高分作品；再不行就用热门榜。
package recommend

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"moovie/internal/db"
	"moovie/internal/media"
)

const snapshotTTL = 6 * time.Hour

// computeTimeout 是一次推荐计算最长花多久。计算不跟着请求取消：首页等不及先显示占位行，
// 算完的结果照样放进缓存，占位行随后发来的请求会直接用上（或者等着同一次计算算完）。
const computeTimeout = 15 * time.Second

// Service 生成推荐结果，并在内存里缓存 6 小时（重启后重新算一次即可，不需要存数据库）。
type Service struct {
	pool  *db.Pool
	media *media.Store

	mu    sync.Mutex
	cache map[int64]cached
	// invalidations 每次 Invalidate 加一。计算开始后它变了，说明有人刚标记过作品，算出来的结果就不放进缓存。
	invalidations uint64
	// computing 让同一个用户同时只有一次计算：首页和占位行的请求一起来时共用结果。
	computing singleflight.Group
}

type cached struct {
	ids     []int64
	expires time.Time
}

// NewService 创建 Service。
func NewService(pool *db.Pool, mediaStore *media.Store) *Service {
	return &Service{pool: pool, media: mediaStore, cache: map[int64]cached{}}
}

// ForUser 返回给某个用户的推荐。
func (s *Service) ForUser(ctx context.Context, userID int64, limit int) ([]media.Card, error) {
	ids, err := s.snapshot(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.media.Cards(ctx, firstN(ids, limit))
}

// ForUserWithin 和 ForUser 一样，但最多等 wait：缓存里有就立刻返回，没有就算，算不完返回 ok=false，
// 计算在后台继续，结果照样进缓存。首页用它决定"为你推荐"是直接写进页面，还是先放一个占位行。
func (s *Service) ForUserWithin(ctx context.Context, userID int64, limit int, wait time.Duration) (cards []media.Card, ok bool, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ids, err := s.snapshot(waitCtx, userID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, false, nil
		}
		return nil, false, err
	}
	cards, err = s.media.Cards(ctx, firstN(ids, limit))
	return cards, err == nil, err
}

// snapshot 返回缓存里的推荐，过期或没有时重新计算（同一个用户的并发请求共用一次计算）。
func (s *Service) snapshot(ctx context.Context, userID int64) ([]int64, error) {
	s.mu.Lock()
	entry, ok := s.cache[userID]
	s.mu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.ids, nil
	}
	done := s.computing.DoChan(strconv.FormatInt(userID, 10), func() (any, error) {
		s.mu.Lock()
		started := s.invalidations
		s.mu.Unlock()
		computeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), computeTimeout)
		defer cancel()
		ids, err := s.Compute(computeCtx, userID, 60)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.invalidations == started {
			if len(s.cache) > 20000 {
				s.cache = map[int64]cached{}
			}
			s.cache[userID] = cached{ids: ids, expires: time.Now().Add(snapshotTTL)}
		}
		s.mu.Unlock()
		return ids, nil
	})
	select {
	case result := <-done:
		if result.Err != nil {
			return nil, result.Err
		}
		return result.Val.([]int64), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func firstN(ids []int64, limit int) []int64 {
	if len(ids) > limit {
		return ids[:limit]
	}
	return ids
}

// Invalidate 让用户的推荐缓存失效（标记了新的想看或看过之后调用）。
func (s *Service) Invalidate(_ context.Context, userID int64) error {
	s.mu.Lock()
	delete(s.cache, userID)
	s.invalidations++
	s.mu.Unlock()
	s.computing.Forget(strconv.FormatInt(userID, 10))
	return nil
}

// seen 是"用户已经接触过的作品"：标记过的，和有播放进度的。
const seen = `(SELECT media_id FROM user_movies WHERE user_id = $1
UNION SELECT media_id FROM playback_positions WHERE user_id = $1)`

// candidates 是向量检索先取的最近邻个数，过滤掉看过的、同系列的、没海报的以后再取前面的。
// HNSW 每次最多返回 hnsw.ef_search 个结果（默认 40），所以查询前把它调到同样大。
const candidates = 400

// Compute 计算推荐的作品 ID，不读写缓存。三种办法依次补充，直到凑够数量：
// 向量相似 → 用户常看的类型里的高分作品 → 热门榜。
func (s *Service) Compute(ctx context.Context, userID int64, limit int) ([]int64, error) {
	result := []int64{}
	added := map[int64]bool{}
	add := func(ids []int64) {
		for _, id := range ids {
			if !added[id] && len(result) < limit {
				added[id] = true
				result = append(result, id)
			}
		}
	}
	similar, err := s.similar(ctx, userID, limit)
	if err != nil {
		return nil, err
	}
	add(similar)
	queries := []string{
		// 2. 类型：用户标记最多的三个类型里的高分作品。
		`WITH favorite AS (
  SELECT genre FROM user_movies um JOIN media m ON m.id = um.media_id, unnest(m.genres) AS genre
  WHERE um.user_id = $1 GROUP BY genre ORDER BY count(*) DESC LIMIT 3
)
SELECT m.id FROM media m
WHERE NOT m.hidden AND m.poster <> '' AND m.rating_douban >= 7.5
AND m.genres && ARRAY(SELECT genre FROM favorite) AND m.id NOT IN ` + seen + `
ORDER BY m.rating_douban DESC, m.id DESC LIMIT $2`,
		// 3. 热门榜兜底。
		`SELECT c.media_id FROM chart_items c JOIN media m ON m.id = c.media_id
WHERE NOT m.hidden AND m.media_type <> 'short' AND c.media_id NOT IN ` + seen + ` GROUP BY c.media_id ORDER BY min(c.rank), c.media_id LIMIT $2`,
	}
	for _, query := range queries {
		if len(result) >= limit {
			break
		}
		ids, err := s.ids(ctx, s.pool, query, userID, limit)
		if err != nil {
			return nil, err
		}
		add(ids)
	}
	return result, nil
}

// similar 是第 1 种办法：把用户喜欢的作品（想看的 + 评分 ≥ 4 或没评分的看过）的向量取平均，找最接近的作品。
//
// 平均向量先单独算出来，再作为参数传给最近邻查询：排序依据必须是常量，HNSW 索引才用得上；
// 写成同一条查询里的子查询结果，就会对全库每一部有向量的作品算一遍距离再排序，作品多了要好几秒。
func (s *Service) similar(ctx context.Context, userID int64, limit int) ([]int64, error) {
	var taste *string
	if err := s.pool.QueryRow(ctx, `SELECT avg(embedding)::text FROM (
  SELECT m.embedding FROM user_movies um JOIN media m ON m.id = um.media_id
  WHERE um.user_id = $1 AND m.embedding IS NOT NULL AND (um.status = 'wish' OR um.rating >= 4 OR um.rating = 0)
  ORDER BY um.updated_at DESC LIMIT 40) liked`, userID).Scan(&taste); err != nil || taste == nil {
		return nil, err
	}
	var ids []int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL hnsw.ef_search = `+strconv.Itoa(candidates)); err != nil {
			return err
		}
		var err error
		ids, err = s.ids(ctx, tx, `SELECT c.id FROM (
  SELECT id, series_key, hidden, poster, embedding <=> $2::vector AS distance FROM media
  WHERE embedding IS NOT NULL ORDER BY embedding <=> $2::vector LIMIT `+strconv.Itoa(candidates)+`
) c
WHERE NOT c.hidden AND c.poster <> '' AND c.id NOT IN `+seen+`
AND c.series_key NOT IN (SELECT m2.series_key FROM media m2 WHERE m2.id IN `+seen+`)
ORDER BY c.distance LIMIT $3`, userID, *taste, limit)
		return err
	})
	return ids, err
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func (s *Service) ids(ctx context.Context, q querier, sql string, args ...any) ([]int64, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

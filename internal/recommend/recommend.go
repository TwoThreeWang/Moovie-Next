// Package recommend 生成个性化推荐。
//
// 有向量时：把用户喜欢的作品的向量取平均，找最接近的、还没标记过的作品。
// 没有向量时：按用户最常看的类型挑高分作品；再不行就用热门榜。
package recommend

import (
	"context"
	"sync"
	"time"

	"moovie/internal/db"
	"moovie/internal/media"
)

const snapshotTTL = 6 * time.Hour

// Service 生成推荐结果，并在内存里缓存 6 小时（重启后重新算一次即可，不需要存数据库）。
type Service struct {
	pool  *db.Pool
	media *media.Store

	mu    sync.Mutex
	cache map[int64]cached
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
	s.mu.Lock()
	entry, ok := s.cache[userID]
	s.mu.Unlock()
	ids := entry.ids
	if !ok || time.Now().After(entry.expires) {
		computed, err := s.Compute(ctx, userID, 60)
		if err != nil {
			return nil, err
		}
		ids = computed
		s.mu.Lock()
		if len(s.cache) > 20000 {
			s.cache = map[int64]cached{}
		}
		s.cache[userID] = cached{ids: ids, expires: time.Now().Add(snapshotTTL)}
		s.mu.Unlock()
	}
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return s.media.Cards(ctx, ids)
}

// Invalidate 让用户的推荐缓存失效（标记了新的想看或看过之后调用）。
func (s *Service) Invalidate(_ context.Context, userID int64) error {
	s.mu.Lock()
	delete(s.cache, userID)
	s.mu.Unlock()
	return nil
}

// seen 是"用户已经接触过的作品"：标记过的，和有播放进度的。
const seen = `(SELECT media_id FROM user_movies WHERE user_id = $1
UNION SELECT media_id FROM playback_positions WHERE user_id = $1)`

// Compute 计算推荐的作品 ID，不读写缓存。三种办法依次补充，直到凑够数量：
// 向量相似 → 用户常看的类型里的高分作品 → 热门榜。
func (s *Service) Compute(ctx context.Context, userID int64, limit int) ([]int64, error) {
	queries := []string{
		// 1. 向量：喜欢的作品 = 想看的 + 评分 ≥ 4 或没评分的看过。
		`WITH liked AS (
  SELECT m.embedding FROM user_movies um JOIN media m ON m.id = um.media_id
  WHERE um.user_id = $1 AND m.embedding IS NOT NULL AND (um.status = 'wish' OR um.rating >= 4 OR um.rating = 0)
  ORDER BY um.updated_at DESC LIMIT 40
), taste AS (SELECT avg(embedding) AS v FROM liked)
SELECT m.id FROM media m, taste
WHERE taste.v IS NOT NULL AND m.embedding IS NOT NULL AND NOT m.hidden AND m.poster <> ''
AND m.id NOT IN ` + seen + `
AND m.series_key NOT IN (SELECT m2.series_key FROM media m2 WHERE m2.id IN ` + seen + `)
ORDER BY m.embedding <=> taste.v LIMIT $2`,
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
	result := []int64{}
	added := map[int64]bool{}
	for _, query := range queries {
		ids, err := s.ids(ctx, query, userID, limit)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if !added[id] && len(result) < limit {
				added[id] = true
				result = append(result, id)
			}
		}
		if len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (s *Service) ids(ctx context.Context, sql string, args ...any) ([]int64, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
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

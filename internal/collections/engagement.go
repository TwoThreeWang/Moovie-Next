package collections

import (
	"context"
	"time"
)

type Engagement struct {
	Favorites int64
	Favorited bool
}

func (s *Store) Engagement(ctx context.Context, collectionID, viewerID int64) (Engagement, error) {
	var e Engagement
	err := s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(bool_or(user_id = $2), false)
FROM collection_favorites WHERE collection_id = $1 AND active`, collectionID, viewerID).Scan(&e.Favorites, &e.Favorited)
	return e, err
}

func (s *Store) SetFavorite(ctx context.Context, collectionID, userID int64, active bool) error {
	if !active {
		_, err := s.pool.Exec(ctx, `UPDATE collection_favorites SET active = false WHERE collection_id = $1 AND user_id = $2 AND active`, collectionID, userID)
		return err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO collection_favorites (collection_id, user_id)
SELECT id, $2 FROM collections WHERE id = $1 AND status = 'published' AND owner_user_id <> $2 FOR SHARE
ON CONFLICT (collection_id, user_id) DO UPDATE
SET active = true,
    -- 取消后重新收藏算一次新的收藏：计入近 7 天热度，并排到收藏列表最前面。
    created_at = CASE WHEN collection_favorites.active THEN collection_favorites.created_at ELSE now() END`, collectionID, userID)
	return requireChanged(tag.RowsAffected(), err, ErrNotFound)
}

type Favorite struct {
	ID    int64
	Slug  string
	Title string
}

// Favorites 保留暂时下架的收藏，公开详情恢复前隐藏其内容。
func (s *Store) Favorites(ctx context.Context, userID int64, limit, offset int) ([]Favorite, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id,
CASE WHEN c.status = 'published' THEN c.slug ELSE '' END,
CASE WHEN c.status = 'published' THEN c.title ELSE '' END
FROM collection_favorites f JOIN collections c ON c.id = f.collection_id
WHERE f.user_id = $1 AND f.active ORDER BY f.created_at DESC, c.id DESC LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Favorite{}
	for rows.Next() {
		var entry Favorite
		if err := rows.Scan(&entry.ID, &entry.Slug, &entry.Title); err != nil {
			return nil, err
		}
		list = append(list, entry)
	}
	return list, rows.Err()
}

func (s *Store) RecordView(ctx context.Context, slug string, userID int64, day time.Time, viewer []byte) error {
	_, err := s.pool.Exec(ctx, `WITH counted AS (
  INSERT INTO collection_views (collection_id, day, viewer)
  SELECT id, $3::date, $4 FROM collections WHERE slug = $1 AND status = 'published' AND owner_user_id <> $2
  ON CONFLICT DO NOTHING RETURNING collection_id
)
UPDATE collections c SET views = c.views + 1 FROM counted WHERE c.id = counted.collection_id`, slug, userID, day, viewer)
	return err
}

// PruneViews 清理去重记录，累计浏览量保留在片单上。
func (s *Store) PruneViews(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM collection_views WHERE NOT (`+recentViews+`)`)
	return err
}

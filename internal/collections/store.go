// Package collections 管理片单。
package collections

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"moovie/internal/db"
	"moovie/internal/media"
)

// Collection 是一个片单。
type Collection struct {
	ID          int64
	OwnerID     int64
	Slug        string
	Title       string
	Description string
	Featured    bool
	UpdatedAt   time.Time
	Count       int
	Covers      []string // 前几部作品的海报，用作片单封面
}

// Item 是片单里的一部作品。
type Item struct {
	Card media.Card
	Note string
}

// ErrNotFound 表示片单不存在。
var ErrNotFound = errors.New("片单不存在")

// ErrInvalid 表示片单内容不合法。
var ErrInvalid = errors.New("片单内容不合法")

// Store 读写片单相关的表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

const collectionColumns = `c.id, c.owner_user_id, c.slug, c.title, c.description, c.featured, c.updated_at,
(SELECT count(*) FROM collection_items i JOIN media m ON m.id = i.media_id WHERE i.collection_id = c.id AND NOT m.hidden),
ARRAY(SELECT m.poster FROM collection_items i JOIN media m ON m.id = i.media_id
      WHERE i.collection_id = c.id AND NOT m.hidden AND m.poster <> '' ORDER BY i.position, m.id LIMIT 4)`

func scanCollection(row pgx.Row) (Collection, error) {
	var c Collection
	err := row.Scan(&c.ID, &c.OwnerID, &c.Slug, &c.Title, &c.Description, &c.Featured, &c.UpdatedAt, &c.Count, &c.Covers)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// List 列出片单，精选的在前。
func (s *Store) List(ctx context.Context, limit, offset int) ([]Collection, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+collectionColumns+` FROM collections c
ORDER BY c.featured DESC, c.updated_at DESC, c.id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Collection{}
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, rows.Err()
}

// BySlug 按 slug 读取片单。
func (s *Store) BySlug(ctx context.Context, slug string) (Collection, error) {
	return scanCollection(s.pool.QueryRow(ctx, `SELECT `+collectionColumns+` FROM collections c WHERE c.slug = $1`, slug))
}

// ByID 按 ID 读取片单。
func (s *Store) ByID(ctx context.Context, id int64) (Collection, error) {
	return scanCollection(s.pool.QueryRow(ctx, `SELECT `+collectionColumns+` FROM collections c WHERE c.id = $1`, id))
}

// Items 返回片单里的作品，按编排顺序。
func (s *Store) Items(ctx context.Context, collectionID int64) ([]Item, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.id, m.media_type, m.title, m.year, m.poster, COALESCE(m.backdrops[1], ''),
CASE WHEN m.rating_douban > 0 THEN m.rating_douban ELSE m.rating_tmdb END, m.season, m.episode_count, i.note
FROM collection_items i JOIN media m ON m.id = i.media_id
WHERE i.collection_id = $1 AND NOT m.hidden ORDER BY i.position, m.id`, collectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var item Item
		c := &item.Card
		if err := rows.Scan(&c.ID, &c.Type, &c.Title, &c.Year, &c.Poster, &c.Backdrop, &c.Rating, &c.Season, &c.EpisodeCount, &item.Note); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// RelatedTo 返回和这个片单相关的片单（最多 3 个）：共同收录的作品越多越靠前；不够时用精选片单补上。
// 实时算，不需要编辑手动维护，也不需要存。
func (s *Store) RelatedTo(ctx context.Context, collectionID int64) ([]Collection, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+collectionColumns+` FROM collections c
LEFT JOIN LATERAL (
  SELECT count(*) AS shared FROM collection_items a JOIN collection_items b ON b.media_id = a.media_id
  WHERE a.collection_id = $1 AND b.collection_id = c.id
) overlap ON true
WHERE c.id <> $1 AND (overlap.shared > 0 OR c.featured)
ORDER BY overlap.shared DESC, c.featured DESC, c.updated_at DESC LIMIT 3`, collectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Collection{}
	for rows.Next() {
		collection, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, collection)
	}
	return list, rows.Err()
}

// Draft 是保存片单时提交的内容。
type Draft struct {
	ID          int64 // 0 表示新建
	OwnerID     int64
	Slug        string
	Title       string
	Description string
	Featured    bool
	MediaIDs    []int64 // 按编排顺序
	Notes       map[int64]string
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,60}$`)

// Save 新建或更新片单，并整体替换其中的作品和关联片单。
func (s *Store) Save(ctx context.Context, draft Draft) (int64, error) {
	draft.Slug, draft.Title = strings.TrimSpace(draft.Slug), strings.TrimSpace(draft.Title)
	if !slugPattern.MatchString(draft.Slug) || draft.Title == "" || len([]rune(draft.Title)) > 80 {
		return 0, ErrInvalid
	}
	id := draft.ID
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if id == 0 {
			err = tx.QueryRow(ctx, `INSERT INTO collections (owner_user_id, slug, title, description, featured)
VALUES ($1, $2, $3, $4, $5) RETURNING id`, draft.OwnerID, draft.Slug, draft.Title, draft.Description, draft.Featured).Scan(&id)
		} else {
			var tag interface{ RowsAffected() int64 }
			tag, err = tx.Exec(ctx, `UPDATE collections SET slug = $2, title = $3, description = $4, featured = $5, updated_at = now()
WHERE id = $1`, id, draft.Slug, draft.Title, draft.Description, draft.Featured)
			if err == nil && tag.RowsAffected() == 0 {
				return ErrNotFound
			}
		}
		if db.IsUniqueViolation(err) {
			return ErrInvalid
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM collection_items WHERE collection_id = $1`, id); err != nil {
			return err
		}
		for position, mediaID := range draft.MediaIDs {
			if _, err := tx.Exec(ctx, `INSERT INTO collection_items (collection_id, media_id, position, note)
SELECT $1, m.id, $3, $4 FROM media m WHERE m.id = $2 ON CONFLICT DO NOTHING`, id, mediaID, position, draft.Notes[mediaID]); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

// Delete 删除片单。
func (s *Store) Delete(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM collections WHERE id = $1`, id)
	return err
}

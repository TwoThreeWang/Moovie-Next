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
	ID           int64
	OwnerID      int64
	Slug         string
	Title        string
	Description  string
	AuthorName   string
	AuthorAvatar string
	Status       string
	Revision     string // 已发布片单的修改稿状态：""、RevisionPending、RevisionReturned
	ReviewReason string
	LinkVisible  bool
	Views        int64
	PublishedAt  *time.Time
	UpdatedAt    time.Time
	Count        int
	Covers       []media.Picture // 前几部作品的海报及片名，用作片单封面
}

const (
	DraftStatus = "draft"
	Pending     = "pending"
	Published   = "published"
	Returned    = "returned"

	RevisionPending  = "pending"
	RevisionReturned = "returned"
)

func (c Collection) StatusLabel() string {
	switch c.Status {
	case Pending:
		return "待审核"
	case Published:
		switch c.Revision {
		case RevisionPending:
			return "已发布 · 修改待审核"
		case RevisionReturned:
			return "已发布 · 修改被退回"
		}
		return "已发布"
	case Returned:
		return "待修改"
	default:
		if c.LinkVisible {
			return "仅链接可见"
		}
		return "私密"
	}
}

// AwaitingReview 表示有内容等管理员处理：新投稿，或已发布片单的修改稿。
func (c Collection) AwaitingReview() bool {
	return c.Status == Pending || c.Revision == RevisionPending
}

// Item 是片单里的一部作品。
type Item struct {
	Card   media.Card
	Note   string
	Hidden bool
}

// ErrNotFound 表示片单不存在。
var ErrNotFound = errors.New("片单不存在")

// ErrInvalid 表示片单内容不合法。
var ErrInvalid = errors.New("片单内容不合法")
var ErrConflict = errors.New("片单已经被更新")
var ErrMissingItem = errors.New("片单中的作品已不存在")

// Store 读写片单相关的表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

const collectionColumns = `c.id, c.owner_user_id, c.slug, c.title, c.description, u.username, u.avatar, c.status,
CASE WHEN c.revision_submitted_at IS NOT NULL THEN 'pending' WHEN c.revision IS NOT NULL THEN 'returned' ELSE '' END,
c.review_reason, c.link_visible, c.views, c.published_at, c.updated_at,
(SELECT count(*) FROM collection_items i JOIN media m ON m.id = i.media_id WHERE i.collection_id = c.id AND NOT m.hidden),
COALESCE((SELECT jsonb_agg(jsonb_build_object('url', p.poster, 'title', p.title) ORDER BY p.position, p.id)
FROM (SELECT m.id, m.poster, m.title, i.position FROM collection_items i JOIN media m ON m.id = i.media_id
      WHERE i.collection_id = c.id AND NOT m.hidden AND m.poster <> '' ORDER BY i.position, m.id LIMIT 4) p), '[]'::jsonb)`

const collectionSelect = `SELECT ` + collectionColumns + ` FROM collections c JOIN users u ON u.id = c.owner_user_id`
const recentViews = `day >= (now() AT TIME ZONE 'UTC')::date - 6`

func scanCollection(row pgx.Row) (Collection, error) {
	var c Collection
	err := row.Scan(&c.ID, &c.OwnerID, &c.Slug, &c.Title, &c.Description, &c.AuthorName, &c.AuthorAvatar,
		&c.Status, &c.Revision, &c.ReviewReason, &c.LinkVisible, &c.Views, &c.PublishedAt, &c.UpdatedAt, &c.Count, &c.Covers)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func (s *Store) list(ctx context.Context, query string, args ...any) ([]Collection, error) {
	rows, err := s.pool.Query(ctx, collectionSelect+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Collection{}
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// Browse 只列已通过审核的片单，热度按近 7 天收藏优先、浏览其次。
// 热度先按片单汇总近期的收藏和浏览再排序，不对每个片单分别计数。
func (s *Store) Browse(ctx context.Context, sort string, limit, offset int) ([]Collection, error) {
	if sort != "hot" {
		return s.list(ctx, ` WHERE c.status = 'published' ORDER BY c.published_at DESC, c.id DESC LIMIT $1 OFFSET $2`, limit, offset)
	}
	return s.list(ctx, `
LEFT JOIN (SELECT collection_id, count(*) AS n FROM collection_favorites
  WHERE active AND created_at >= now() - interval '7 days' GROUP BY collection_id) f ON f.collection_id = c.id
LEFT JOIN (SELECT collection_id, count(*) AS n FROM collection_views WHERE `+recentViews+` GROUP BY collection_id) v ON v.collection_id = c.id
WHERE c.status = 'published'
ORDER BY COALESCE(f.n, 0) DESC, COALESCE(v.n, 0) DESC, c.published_at DESC, c.id DESC LIMIT $1 OFFSET $2`, limit, offset)
}

func (s *Store) ByAuthor(ctx context.Context, ownerID int64, limit, offset int) ([]Collection, error) {
	return s.list(ctx, ` WHERE c.owner_user_id = $1 AND c.status = 'published'
ORDER BY c.published_at DESC, c.id DESC LIMIT $2 OFFSET $3`, ownerID, limit, offset)
}

func (s *Store) Owned(ctx context.Context, ownerID int64, limit, offset int) ([]Collection, error) {
	return s.list(ctx, ` WHERE c.owner_user_id = $1 ORDER BY c.updated_at DESC, c.id DESC LIMIT $2 OFFSET $3`, ownerID, limit, offset)
}

// awaitingReview 和 Collection.AwaitingReview 一致：新投稿，或已发布片单待审的修改稿。
const awaitingReview = `c.status = 'pending' OR c.revision_submitted_at IS NOT NULL`

// PendingCount 返回等待审核的片单数量。
func (s *Store) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM collections c WHERE `+awaitingReview).Scan(&n)
	return n, err
}

// ModerationQueue 列出某个审核状态的片单。待审核包括新投稿和已发布片单的修改稿，先提交的在前。
func (s *Store) ModerationQueue(ctx context.Context, status string, limit, offset int) ([]Collection, error) {
	if status == Pending {
		return s.list(ctx, ` WHERE `+awaitingReview+`
ORDER BY COALESCE(c.submitted_at, c.revision_submitted_at), c.id LIMIT $1 OFFSET $2`, limit, offset)
	}
	return s.list(ctx, ` WHERE c.status = $1 ORDER BY c.updated_at DESC, c.id DESC LIMIT $2 OFFSET $3`, status, limit, offset)
}

func (s *Store) ByID(ctx context.Context, id int64) (Collection, error) {
	return scanCollection(s.pool.QueryRow(ctx, collectionSelect+` WHERE c.id = $1`, id))
}

// OwnerDetail 返回作者正在编辑的版本：有修改稿时是修改稿，公开内容不变。
func (s *Store) OwnerDetail(ctx context.Context, id, ownerID int64) (Collection, []Item, error) {
	return s.detail(ctx, true, func(c Collection) bool { return c.Revision != "" },
		`c.id = $1 AND c.owner_user_id = $2`, id, ownerID)
}

// ModerationDetail 返回等待审核的版本：修改稿待审时是修改稿，否则是正式内容。
func (s *Store) ModerationDetail(ctx context.Context, id int64) (Collection, []Item, error) {
	return s.detail(ctx, true, func(c Collection) bool { return c.Revision == RevisionPending },
		`c.id = $1 AND c.status IN ('pending', 'published', 'returned')`, id)
}

// VisibleDetail 返回访客看到的正式内容。修改稿审核期间仍显示已发布的版本。
func (s *Store) VisibleDetail(ctx context.Context, slug string) (Collection, []Item, error) {
	return s.detail(ctx, false, nil, `c.slug = $1 AND (c.status = 'published' OR c.link_visible)`, slug)
}

// 同一个数据库快照检查可见性并读取条目，避免并发修改时读到前后不一致的标题和作品。
func (s *Store) detail(ctx context.Context, includeHidden bool, useRevision func(Collection) bool, where string, args ...any) (Collection, []Item, error) {
	var c Collection
	var items []Item
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		c, err = scanCollection(tx.QueryRow(ctx, collectionSelect+` WHERE `+where, args...))
		if err != nil {
			return err
		}
		if useRevision == nil || !useRevision(c) {
			items, err = readItems(ctx, tx, liveItems, c.ID, includeHidden)
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT revision->>'title', revision->>'description' FROM collections WHERE id = $1`, c.ID).
			Scan(&c.Title, &c.Description); err != nil {
			return err
		}
		if items, err = readItems(ctx, tx, revisionItems, c.ID, includeHidden); err != nil {
			return err
		}
		c.Count = 0
		for _, item := range items {
			if !item.Hidden {
				c.Count++
			}
		}
		return nil
	})
	return c, items, err
}

const itemColumns = `m.id, m.media_type, m.title, m.year, m.poster, COALESCE(m.backdrops[1], ''),
CASE WHEN m.rating_douban > 0 THEN m.rating_douban ELSE m.rating_tmdb END, m.season, m.episode_count`

const liveItems = `SELECT ` + itemColumns + `, i.note, m.hidden
FROM collection_items i JOIN media m ON m.id = i.media_id
WHERE i.collection_id = $1 AND ($2 OR NOT m.hidden) ORDER BY i.position, m.id`

// revisionEntries 按修改稿里的顺序展开作品，已从作品库删除的作品自然跳过。
const revisionEntries = `FROM collections c
CROSS JOIN LATERAL jsonb_array_elements(c.revision->'items') WITH ORDINALITY AS e(item, position)
JOIN media m ON m.id = (e.item->>'media_id')::bigint`

const revisionItems = `SELECT ` + itemColumns + `, COALESCE(e.item->>'note', ''), m.hidden ` + revisionEntries + `
WHERE c.id = $1 AND ($2 OR NOT m.hidden) ORDER BY e.position`

func readItems(ctx context.Context, q db.Querier, query string, collectionID int64, includeHidden bool) ([]Item, error) {
	rows, err := q.Query(ctx, query, collectionID, includeHidden)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var item Item
		c := &item.Card
		if err := rows.Scan(&c.ID, &c.Type, &c.Title, &c.Year, &c.Poster, &c.Backdrop, &c.Rating, &c.Season, &c.EpisodeCount, &item.Note, &item.Hidden); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// RelatedTo 按共同收录的可见作品推荐其他已公开片单。
func (s *Store) RelatedTo(ctx context.Context, collectionID int64) ([]Collection, error) {
	return s.list(ctx, `
JOIN LATERAL (
  SELECT count(*) AS shared FROM collection_items a JOIN collection_items b ON b.media_id = a.media_id
  JOIN media m ON m.id = a.media_id AND NOT m.hidden
  WHERE a.collection_id = $1 AND b.collection_id = c.id
) overlap ON overlap.shared > 0
WHERE c.id <> $1 AND c.status = 'published'
ORDER BY overlap.shared DESC, c.published_at DESC, c.id DESC LIMIT 3`, collectionID)
}

// Draft 是用户提交的片单内容。
type Draft struct {
	ID          int64
	OwnerID     int64
	Slug        string
	Title       string
	Description string
	MediaIDs    []int64
	Notes       map[int64]string
	Base        time.Time
	Submit      bool
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,60}$`)

// Save 保存私密片单或送审。已发布片单的修改存成修改稿并送审，公开内容在审核通过前保持不变。
func (s *Store) Save(ctx context.Context, draft Draft) (int64, error) {
	draft.Title = strings.TrimSpace(draft.Title)
	if draft.Title == "" || len([]rune(draft.Title)) > 80 || len(draft.MediaIDs) == 0 || len(draft.MediaIDs) > 200 ||
		(draft.ID == 0 && !slugPattern.MatchString(draft.Slug)) {
		return 0, ErrInvalid
	}
	id := draft.ID
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if id == 0 {
			status := DraftStatus
			if draft.Submit {
				status = Pending
			}
			err := tx.QueryRow(ctx, `INSERT INTO collections (owner_user_id, slug, title, description, status, submitted_at)
VALUES ($1, $2, $3, $4, $5, CASE WHEN $5 = 'pending' THEN now() END) RETURNING id`,
				draft.OwnerID, draft.Slug, draft.Title, draft.Description, status).Scan(&id)
			if db.IsUniqueViolation(err) {
				return ErrInvalid
			}
			if err != nil {
				return err
			}
			return replaceItems(ctx, tx, id, draft)
		}
		status, err := lockOwned(ctx, tx, id, draft.OwnerID, draft.Base)
		if err != nil {
			return err
		}
		switch {
		case status == Published && draft.Submit:
			return saveRevision(ctx, tx, id, draft)
		case status == Published || (status == Pending && !draft.Submit):
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE collections
SET title = $2, description = $3, updated_at = clock_timestamp(),
    status = CASE WHEN $4 THEN 'pending' ELSE status END,
    submitted_at = CASE WHEN $4 THEN COALESCE(submitted_at, now()) END,
    review_reason = CASE WHEN $4 THEN '' ELSE review_reason END,
    link_visible = CASE WHEN $4 THEN false ELSE link_visible END
WHERE id = $1`, id, draft.Title, draft.Description, draft.Submit); err != nil {
			return err
		}
		return replaceItems(ctx, tx, id, draft)
	})
	return id, err
}

// lockOwned 锁住作者自己的片单并核对编辑时的版本，返回当前状态。
func lockOwned(ctx context.Context, tx pgx.Tx, id, ownerID int64, base time.Time) (string, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM collections WHERE id = $1 AND owner_user_id = $2 AND updated_at = $3 FOR UPDATE`,
		id, ownerID, base).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrConflict
	}
	return status, err
}

func replaceItems(ctx context.Context, tx pgx.Tx, id int64, draft Draft) error {
	if _, err := tx.Exec(ctx, `DELETE FROM collection_items WHERE collection_id = $1`, id); err != nil {
		return err
	}
	for position, mediaID := range draft.MediaIDs {
		tag, err := tx.Exec(ctx, `INSERT INTO collection_items (collection_id, media_id, position, note)
SELECT $1, m.id, $3, $4 FROM media m WHERE m.id = $2 ON CONFLICT DO NOTHING`, id, mediaID, position, draft.Notes[mediaID])
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrMissingItem
		}
	}
	return nil
}

type revision struct {
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Items       []revisionItem `json:"items"`
}

type revisionItem struct {
	MediaID int64  `json:"media_id"`
	Note    string `json:"note,omitempty"`
}

// saveRevision 只保存作者提出的新内容；正式内容和作品条目在审核通过时才替换。
func saveRevision(ctx context.Context, tx pgx.Tx, id int64, draft Draft) error {
	var found int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM media WHERE id = ANY($1)`, draft.MediaIDs).Scan(&found); err != nil {
		return err
	}
	if found != len(draft.MediaIDs) {
		return ErrMissingItem
	}
	next := revision{Title: draft.Title, Description: draft.Description, Items: make([]revisionItem, 0, len(draft.MediaIDs))}
	for _, mediaID := range draft.MediaIDs {
		next.Items = append(next.Items, revisionItem{MediaID: mediaID, Note: draft.Notes[mediaID]})
	}
	_, err := tx.Exec(ctx, `UPDATE collections SET revision = $2, revision_submitted_at = now(), review_reason = '',
updated_at = clock_timestamp() WHERE id = $1`, id, next)
	return err
}

// mergeRevision 把修改稿合并为正式内容并清空修改稿；没有修改稿时什么也不做。
func mergeRevision(ctx context.Context, tx pgx.Tx, id int64) error {
	tag, err := tx.Exec(ctx, `UPDATE collections SET title = revision->>'title', description = revision->>'description'
WHERE id = $1 AND revision IS NOT NULL`, id)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM collection_items WHERE collection_id = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO collection_items (collection_id, media_id, position, note)
SELECT c.id, m.id, e.position - 1, COALESCE(e.item->>'note', '') `+revisionEntries+`
WHERE c.id = $1 ON CONFLICT DO NOTHING`, id); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE collections SET revision = NULL, revision_submitted_at = NULL WHERE id = $1`, id)
	return err
}

func (s *Store) SetLinkVisibility(ctx context.Context, id, ownerID int64, base time.Time, enabled bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE collections SET link_visible = $4
WHERE id = $1 AND owner_user_id = $2 AND updated_at = $3 AND status IN ('draft', 'returned')`, id, ownerID, base, enabled)
	return requireChanged(tag.RowsAffected(), err, ErrConflict)
}

// Withdraw 撤回投稿或撤下已发布片单。有修改稿时先合并，私密片单里保留作者最新的修改。
func (s *Store) Withdraw(ctx context.Context, id, ownerID int64, base time.Time, keepLink bool) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		status, err := lockOwned(ctx, tx, id, ownerID, base)
		if err != nil {
			return err
		}
		if status != Published && (status != Pending || keepLink) {
			return ErrConflict
		}
		if err := mergeRevision(ctx, tx, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE collections
SET status = 'draft', submitted_at = NULL, updated_at = clock_timestamp(), link_visible = $2 WHERE id = $1`, id, keepLink)
		return err
	})
}

// DiscardRevision 放弃已发布片单的修改稿，公开内容保持不变。
func (s *Store) DiscardRevision(ctx context.Context, id, ownerID int64, base time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE collections
SET revision = NULL, revision_submitted_at = NULL, review_reason = '', updated_at = clock_timestamp()
WHERE id = $1 AND owner_user_id = $2 AND updated_at = $3 AND revision IS NOT NULL`, id, ownerID, base)
	return requireChanged(tag.RowsAffected(), err, ErrConflict)
}

func (s *Store) DeleteOwned(ctx context.Context, id, ownerID int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM collections WHERE id = $1 AND owner_user_id = $2`, id, ownerID)
	return requireChanged(tag.RowsAffected(), err, ErrNotFound)
}

func requireChanged(rows int64, err, ifMissing error) error {
	if err == nil && rows == 0 {
		return ifMissing
	}
	return err
}

// Review 处理审核：通过或退回新投稿、通过或退回已发布片单的修改稿、下架已发布片单。
func (s *Store) Review(ctx context.Context, id, adminID int64, base time.Time, action, reason string) error {
	reason = strings.TrimSpace(reason)
	switch action {
	case "approve":
		reason = ""
	case "return", "remove":
		if reason == "" || len([]rune(reason)) > 300 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		var revisionPending bool
		err := tx.QueryRow(ctx, `SELECT status, revision_submitted_at IS NOT NULL FROM collections
WHERE id = $1 AND updated_at = $2 FOR UPDATE`, id, base).Scan(&status, &revisionPending)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		var changes, kind string
		merge := false
		switch {
		case action == "approve" && status == Pending:
			changes, kind = `status = 'published', submitted_at = NULL, published_at = COALESCE(published_at, now()), link_visible = false, `, "collection_approved"
		case action == "approve" && revisionPending:
			kind, merge = "collection_approved", true
		case action == "return" && status == Pending:
			changes, kind = `status = 'returned', submitted_at = NULL, link_visible = false, `, "collection_returned"
		case action == "return" && revisionPending:
			changes, kind = `revision_submitted_at = NULL, `, "collection_returned"
		case action == "remove" && status == Published:
			changes, kind, merge = `status = 'returned', link_visible = false, `, "collection_removed", true
		default:
			return ErrConflict
		}
		if merge {
			if err := mergeRevision(ctx, tx, id); err != nil {
				return err
			}
		}
		var ownerID int64
		var title string
		if err := tx.QueryRow(ctx, `UPDATE collections SET `+changes+`review_reason = $2, updated_at = clock_timestamp()
WHERE id = $1 RETURNING owner_user_id, title`, id, reason).Scan(&ownerID, &title); err != nil {
			return err
		}
		if action == "approve" {
			var hasWorks bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM collection_items i JOIN media m ON m.id = i.media_id
WHERE i.collection_id = $1 AND NOT m.hidden)`, id).Scan(&hasWorks); err != nil {
				return err
			}
			if !hasWorks {
				return ErrMissingItem
			}
		}
		if ownerID == adminID {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO notifications (user_id, actor_id, kind, collection_id, body, reason)
VALUES ($1, $2, $3, $4, $5, $6)`, ownerID, adminID, kind, id, title, reason)
		return err
	})
}

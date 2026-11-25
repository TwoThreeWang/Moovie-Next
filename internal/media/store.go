package media

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"moovie/internal/db"
)

// Store 读写作品相关的表。
type Store struct{ pool *db.Pool }

// NewStore 创建 Store。
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

const mediaColumns = `m.id, m.media_type, m.douban_id, m.tmdb_id, m.imdb_id, m.wikidata_id, m.hongguo_id, m.title, m.original_title, m.year, m.season, m.episode_count,
m.poster, m.backdrops, m.summary, m.genres, m.countries, m.directors, m.actors, m.duration_minutes,
m.rating_douban, m.rating_tmdb, m.votes_tmdb, m.rating_hongguo, m.heat, m.series_status, m.series_key, m.hidden, m.metadata_status,
m.metadata_synced_at, m.enriched_at, m.semantic_text, m.semantic_hash, m.embedding IS NOT NULL, m.updated_at`

func scanMedia(row pgx.Row) (Media, error) {
	var m Media
	err := row.Scan(&m.ID, &m.Type, &m.DoubanID, &m.TMDBID, &m.IMDbID, &m.WikidataID, &m.HongguoID, &m.Title, &m.OriginalTitle, &m.Year, &m.Season, &m.EpisodeCount,
		&m.Poster, &m.Backdrops, &m.Summary, &m.Genres, &m.Countries, &m.Directors, &m.Actors, &m.DurationMinutes,
		&m.RatingDouban, &m.RatingTMDB, &m.VotesTMDB, &m.RatingHongguo, &m.Heat, &m.SeriesStatus, &m.SeriesKey, &m.Hidden, &m.MetadataStatus,
		&m.MetadataSyncedAt, &m.EnrichedAt, &m.SemanticText, &m.SemanticHash, &m.HasEmbedding, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

const cardColumns = `m.id, m.media_type, m.title, m.year, m.poster, COALESCE(m.backdrops[1], ''),
CASE WHEN m.rating_douban > 0 THEN m.rating_douban WHEN m.rating_tmdb > 0 THEN m.rating_tmdb ELSE m.rating_hongguo END, m.season, m.episode_count, m.summary, m.genres`

func scanCards(rows pgx.Rows, err error) ([]Card, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cards := []Card{}
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.ID, &c.Type, &c.Title, &c.Year, &c.Poster, &c.Backdrop, &c.Rating, &c.Season, &c.EpisodeCount, &c.Summary, &c.Genres); err != nil {
			return nil, err
		}
		cards = append(cards, c)
	}
	return cards, rows.Err()
}

// Get 按 ID 读取一部作品。
func (s *Store) Get(ctx context.Context, id int64) (Media, error) {
	return scanMedia(s.pool.QueryRow(ctx, `SELECT `+mediaColumns+` FROM media m WHERE m.id = $1`, id))
}

// ByDoubanID 按豆瓣 ID 读取一部作品。
func (s *Store) ByDoubanID(ctx context.Context, doubanID string) (Media, error) {
	return scanMedia(s.pool.QueryRow(ctx, `SELECT `+mediaColumns+` FROM media m WHERE m.douban_id = $1 AND $1 <> ''`, doubanID))
}

// ValidDoubanID 判断字符串是不是合法的豆瓣条目 ID：7–9 位数字，不以 0 开头。
// 豆瓣的条目编号从 100 万开始，现在在 3900 万上下（老系统里是 129 万到 3854 万），
// 不合这个格式的一律不收录，也就不会拿去请求豆瓣。
//
// 格式对、但豆瓣上没有的 ID（比如图书、音乐的条目），第一次请求后会记成 not_found 永久保留，
// 之后从任何入口再送来都不会再请求，只有管理员手动刷新会再查一次。
func ValidDoubanID(value string) bool {
	if len(value) < 7 || len(value) > 9 || value[0] == '0' {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// EnsureDouban 保证一个豆瓣条目在作品库里有一行，返回作品 ID 和是不是新建的。
func (s *Store) EnsureDouban(ctx context.Context, doubanID string, seed Patch) (id int64, created bool, err error) {
	if !ValidDoubanID(doubanID) {
		return 0, false, fmt.Errorf("豆瓣 ID 不合法: %q", doubanID)
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO media (douban_id) VALUES ($1)
ON CONFLICT (douban_id) WHERE douban_id <> '' DO NOTHING RETURNING id`, doubanID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.pool.QueryRow(ctx, `SELECT id FROM media WHERE douban_id = $1`, doubanID).Scan(&id)
		return id, false, err
	}
	if err != nil {
		return 0, false, err
	}
	if len(seed) > 0 {
		// 刚建的空行：seed 直接写入，之后豆瓣的资料会覆盖它。
		if err := s.applyPatch(ctx, id, ProviderManual, seed); err != nil {
			return id, true, err
		}
	}
	return id, true, nil
}

// ApplyListing 用豆瓣列表（热门榜、用户的标记列表）里顺手带的资料更新已有的作品，不用为此再请求详情。
// 评分是豆瓣当前的值，给了就写；年份和海报只补空着的：列表里的海报可能比详情里的小一档，不能覆盖。
// 给了评分就记下 douban_listed_at，近期电影的详情因此可以隔得更久再抓（见 Stale）。
func (s *Store) ApplyListing(ctx context.Context, id int64, rating float64, year int, poster string) error {
	if rating <= 0 && year <= 0 && poster == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE media SET
  rating_douban = CASE WHEN $2::real > 0 THEN $2::real ELSE rating_douban END,
  year = CASE WHEN year = 0 AND $3::int > 0 THEN $3::int ELSE year END,
  poster = CASE WHEN poster = '' THEN $4::text ELSE poster END,
  douban_listed_at = CASE WHEN $2::real > 0 THEN now() ELSE douban_listed_at END,
  updated_at = CASE WHEN ($2::real > 0 AND rating_douban <> $2::real) OR (year = 0 AND $3::int > 0) OR (poster = '' AND $4::text <> '') THEN now() ELSE updated_at END
WHERE id = $1 AND douban_id <> ''`, id, float32(rating), year, poster)
	return err
}

// ApplyPatch 把某个来源的字段合并进作品。合并规则固定，不需要逐字段记录来源：
//   - 豆瓣是主要来源（manual 是演示数据和新建时的初始信息），给的值总是写入；
//   - 红果是短剧唯一的来源，给的值也总是写入；
//   - TMDB 是辅助来源，只补空着的字段；TMDB 独有的字段（TMDB 评分和票数、剧照、连载状态）直接写。
//
// Patch 里只放有值的字段（Set 会跳过空值），所以豆瓣没给的字段不会把已有的值清掉。
func (s *Store) ApplyPatch(ctx context.Context, id int64, provider string, patch Patch) error {
	if provider != ProviderDouban && provider != ProviderTMDB && provider != ProviderManual && provider != ProviderHongguo {
		return fmt.Errorf("未知的元数据来源: %q", provider)
	}
	return s.applyPatch(ctx, id, provider, patch)
}

// tmdbOwned 是只有 TMDB 提供的字段：TMDB 给了就直接写。
var tmdbOwned = map[string]bool{"rating_tmdb": true, "votes_tmdb": true, "backdrops": true, "series_status": true}

// emptyCheck 返回"这个字段还是空的"的 SQL 条件（辅助来源只补空字段）。
func emptyCheck(field string) string {
	switch field {
	case "backdrops", "genres", "countries", "directors", "actors":
		return "cardinality(" + field + ") = 0"
	case "year", "season", "episode_count", "duration_minutes", "rating_douban", "rating_tmdb", "votes_tmdb", "rating_hongguo", "heat":
		return field + " = 0"
	}
	return field + " = ''"
}

func (s *Store) applyPatch(ctx context.Context, id int64, provider string, patch Patch) error {
	if len(patch) == 0 {
		return nil
	}
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM media WHERE id = $1 FOR UPDATE`, id).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		fillOnly := provider == ProviderTMDB
		sets := []string{"updated_at = now()"}
		args := []any{id}
		for _, field := range patchFields {
			value, ok := patch[field]
			if !ok {
				continue
			}
			args = append(args, value)
			placeholder := "$" + strconv.Itoa(len(args))
			condition := ""
			if fillOnly && !tmdbOwned[field] {
				condition = emptyCheck(field)
			}
			if condition == "" {
				sets = append(sets, field+" = "+placeholder)
			} else {
				sets = append(sets, field+" = CASE WHEN "+condition+" THEN "+placeholder+" ELSE "+field+" END")
			}
			if _, explicit := patch["series_key"]; field == "title" && !explicit {
				args = append(args, SeriesKey(value.(string)))
				key := "$" + strconv.Itoa(len(args))
				if condition == "" {
					sets = append(sets, "series_key = "+key)
				} else {
					sets = append(sets, "series_key = CASE WHEN "+condition+" THEN "+key+" ELSE series_key END")
				}
			}
		}
		if len(sets) == 1 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE media SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...); err != nil {
			return fmt.Errorf("写入作品字段: %w", err)
		}
		// 片名和原名自动成为别名，这样搜索和匹配都能找到。
		for _, field := range []string{"title", "original_title"} {
			if value, ok := patch[field].(string); ok && value != "" {
				if err := addAliases(ctx, tx, id, value); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// EnsureHongguo 保证一部红果短剧在作品库里有一行，返回作品 ID 和是不是新建的。新建的行只有 ID，资料由后台去抓。
func (s *Store) EnsureHongguo(ctx context.Context, seriesID string) (id int64, created bool, err error) {
	if !ValidHongguoID(seriesID) {
		return 0, false, fmt.Errorf("红果 series_id 不合法: %q", seriesID)
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO media (hongguo_id, media_type) VALUES ($1, 'short')
ON CONFLICT (hongguo_id) WHERE hongguo_id <> '' DO NOTHING RETURNING id`, seriesID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.pool.QueryRow(ctx, `SELECT id FROM media WHERE hongguo_id = $1`, seriesID).Scan(&id)
		return id, false, err
	}
	return id, err == nil, err
}

// ValidHongguoID 判断字符串是不是红果的 series_id：19 位数字。
func ValidHongguoID(value string) bool {
	if len(value) != 19 || value[0] == '0' {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// UnknownHongguo 从一批红果 series_id 里挑出作品库里还没有的，保持原来的顺序。
func (s *Store) UnknownHongguo(ctx context.Context, seriesIDs []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT wanted.id FROM unnest($1::text[]) WITH ORDINALITY AS wanted(id, position)
WHERE NOT EXISTS (SELECT 1 FROM media m WHERE m.hongguo_id = wanted.id) ORDER BY wanted.position`, seriesIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	unknown := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		unknown = append(unknown, id)
	}
	return unknown, rows.Err()
}

// SetShortForm 给短剧记上剧种（真人剧、漫剧、AI剧），放在 genres 的最前面。原来记的剧种不一样时替换掉。
func (s *Store) SetShortForm(ctx context.Context, id int64, form string) error {
	if !IsShortForm(form) {
		return fmt.Errorf("未知的剧种: %q", form)
	}
	_, err := s.pool.Exec(ctx, `UPDATE media SET genres = array_prepend($2::text, ARRAY(SELECT g FROM unnest(genres) g WHERE g <> ALL($3::text[])))
WHERE id = $1 AND NOT (genres @> ARRAY[$2::text])`, id, form, ShortForms)
	return err
}

// AddAliases 给作品添加别名。
func (s *Store) AddAliases(ctx context.Context, id int64, aliases ...string) error {
	return addAliases(ctx, s.pool, id, aliases...)
}

func addAliases(ctx context.Context, q db.Querier, id int64, aliases ...string) error {
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		normalized := Normalize(alias)
		if normalized == "" || len(alias) > 200 {
			continue
		}
		if _, err := q.Exec(ctx, `INSERT INTO media_aliases (media_id, alias, normalized)
VALUES ($1, $2, $3) ON CONFLICT (media_id, normalized) DO NOTHING`, id, alias, normalized); err != nil {
			return err
		}
	}
	return nil
}

// externalColumns 是各来源 ID 所在的列。
var externalColumns = map[string]string{ProviderDouban: "douban_id", ProviderTMDB: "tmdb_id", ProviderIMDb: "imdb_id", ProviderWikidata: "wikidata_id"}

// SetExternalID 记录作品在某个来源的 ID。这部作品在这个来源已经有 ID 时保持原样。
func (s *Store) SetExternalID(ctx context.Context, id int64, provider, externalID string) error {
	column, ok := externalColumns[provider]
	if externalID == "" || !ok {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE media SET `+column+` = $2 WHERE id = $1 AND `+column+` = ''`, id, externalID)
	return err
}

// ReplaceUnits 用新的分集列表替换某一季的分集。
func (s *Store) ReplaceUnits(ctx context.Context, id int64, season int, units []Unit) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM media_units WHERE media_id = $1 AND season = $2`, id, season); err != nil {
			return err
		}
		for _, unit := range units {
			if _, err := tx.Exec(ctx, `INSERT INTO media_units (media_id, season, episode, title, air_date, runtime_minutes)
VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, id, season, unit.Episode, unit.Title, unit.AirDate, unit.RuntimeMinutes); err != nil {
				return err
			}
		}
		return nil
	})
}

// Units 返回作品的分集信息。
func (s *Store) Units(ctx context.Context, id int64) ([]Unit, error) {
	rows, err := s.pool.Query(ctx, `SELECT season, episode, title, air_date, runtime_minutes FROM media_units
WHERE media_id = $1 ORDER BY season, episode`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	units := []Unit{}
	for rows.Next() {
		var u Unit
		if err := rows.Scan(&u.Season, &u.Episode, &u.Title, &u.AirDate, &u.RuntimeMinutes); err != nil {
			return nil, err
		}
		units = append(units, u)
	}
	return units, rows.Err()
}

// MarkSynced 记录一次元数据同步的结果。
func (s *Store) MarkSynced(ctx context.Context, id int64, status string) error {
	// 以前确认过"豆瓣没有这个条目"（所以被隐藏了）、现在又同步成功了：恢复显示。
	// 屏蔽词命中的作品随后会被 ApplyBlockedKeywords 重新隐藏。
	_, err := s.pool.Exec(ctx, `UPDATE media SET metadata_status = $2, metadata_synced_at = now(),
hidden = CASE WHEN metadata_status = 'not_found' AND $2 <> 'not_found' THEN false ELSE hidden END WHERE id = $1`, id, status)
	return err
}

// Cards 按给定顺序返回卡片，自动跳过隐藏和不存在的作品。
func (s *Store) Cards(ctx context.Context, ids []int64) ([]Card, error) {
	if len(ids) == 0 {
		return []Card{}, nil
	}
	return scanCards(s.pool.Query(ctx, `SELECT `+cardColumns+` FROM unnest($1::bigint[]) WITH ORDINALITY AS wanted(id, position)
JOIN media m ON m.id = wanted.id WHERE NOT m.hidden ORDER BY wanted.position`, ids))
}

// Filter 是发现页的筛选条件。
type Filter struct {
	Type    string
	Genre   string
	Form    string // 短剧的剧种
	Country string
	Decade  int    // 例如 2020 表示 2020–2029；0 表示不限
	Sort    string // hot（默认）、rating、new
	Limit   int
	Offset  int
}

// Discover 按筛选条件列出作品。
func (s *Store) Discover(ctx context.Context, filter Filter) ([]Card, error) {
	where := []string{"NOT m.hidden", "m.title <> ''"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, strings.ReplaceAll(clause, "?", "$"+strconv.Itoa(len(args))))
	}
	// 短剧只在短剧频道里出现："全部"指的是电影、剧集、综艺、动漫。
	short := filter.Type == TypeShort
	if ValidType(filter.Type) {
		add("m.media_type = ?", filter.Type)
	} else {
		where = append(where, "m.media_type <> 'short'")
	}
	tags := []string{}
	if filter.Genre != "" {
		tags = append(tags, filter.Genre)
	}
	if short && IsShortForm(filter.Form) {
		tags = append(tags, filter.Form)
	}
	if len(tags) > 0 {
		add("m.genres @> ?::text[]", tags) // 用得上 genres 的 GIN 索引；= ANY(genres) 用不上
	}
	if filter.Country != "" {
		add("? = ANY(m.countries)", filter.Country)
	}
	if filter.Decade > 0 {
		add("m.year >= ?", filter.Decade)
		add("m.year <= ?", filter.Decade+9)
	}
	// 最热：先是首页热门榜上的（豆瓣热门 + TMDB 一周趋势 + 本站最近 7 天在看的，见 ingest.RefreshCharts），
	// 按榜上的名次；其余按 TMDB 的近期关注度（每天更新），没有 TMDB 数据的再按累计评分人数。
	from := `media m LEFT JOIN (SELECT media_id, kind, min(rank) AS rank FROM chart_items GROUP BY media_id, kind) ch
  ON ch.media_id = m.id AND ch.kind = m.media_type`
	order := "ch.rank IS NULL, ch.rank, m.popularity_tmdb DESC, m.votes_tmdb DESC, m.id DESC"
	plain := "media m" // 不按榜单排的几种不用连 chart_items
	switch {
	case short && filter.Sort == "rating":
		from, order = plain, "m.rating_hongguo DESC, m.heat DESC, m.id DESC"
	case short && filter.Sort == "new":
		// 红果的 series_id 随时间递增、位数相同：按它倒序就是最新上架的在前，走 hongguo_id 的唯一索引。
		where = append(where, "m.hongguo_id <> ''")
		from, order = plain, "m.hongguo_id DESC"
	case short:
		from, order = plain, "m.heat DESC, m.id DESC" // 和 media_short_heat_idx 的顺序一致
	case filter.Sort == "rating":
		from, order = plain, "m.rating_douban DESC, m.id DESC"
	case filter.Sort == "new":
		// 最新：按上映日期（剧集是这一季的首播日期）。还没上映的不列出来；没有日期的按年份，排在那一年的最后。
		where = append(where, "(m.release_date IS NULL OR m.release_date <= current_date)",
			"(m.release_date IS NOT NULL OR m.year <= extract(year FROM current_date))")
		from, order = plain, "COALESCE(m.release_date, CASE WHEN m.year > 0 THEN make_date(m.year, 1, 1) END) DESC NULLS LAST, m.id DESC"
	}
	if filter.Limit <= 0 || filter.Limit > 60 {
		filter.Limit = 30
	}
	args = append(args, filter.Limit, max(0, filter.Offset))
	return scanCards(s.pool.Query(ctx, `SELECT `+cardColumns+` FROM `+from+` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY `+order+` LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...))
}

// SetRelease 记下 TMDB 给的上映日期和近期关注度，并标记这部作品已经向 TMDB 查过日期（查不到也算查过）。
// released 为 nil 时保留原来的日期；popularity 为 0 时保留原来的关注度。
func (s *Store) SetRelease(ctx context.Context, id int64, released *time.Time, popularity float64) error {
	_, err := s.pool.Exec(ctx, `UPDATE media SET release_date = COALESCE($2::date, release_date),
popularity_tmdb = CASE WHEN $3::float8 > 0 THEN $3::float8 ELSE popularity_tmdb END, release_checked = true WHERE id = $1`,
		id, released, popularity)
	return err
}

// NeedsRelease 返回有 TMDB 条目、还没查过上映日期的长剧（这个字段加上之前就入库的作品）。
func (s *Store) NeedsRelease(ctx context.Context, jobKind string, limit int) ([]int64, error) {
	return s.ids(ctx, `SELECT id FROM media m WHERE media_type <> 'short' AND tmdb_id <> '' AND NOT release_checked AND `+idle+`
ORDER BY id LIMIT $2`, jobKind, limit)
}

// TMDBIndex 返回作品库里每个 TMDB 条目（如 "movie/603"、"tv/1399"）对应的作品 ID。剧集的几季共用一个 TMDB 条目。
func (s *Store) TMDBIndex(ctx context.Context) (map[string][]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, tmdb_id FROM media WHERE media_type <> 'short' AND tmdb_id <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	index := map[string][]int64{}
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		index[key] = append(index[key], id)
	}
	return index, rows.Err()
}

// SetPopularity 批量写入 TMDB 的近期关注度，ids 和 values 一一对应。
func (s *Store) SetPopularity(ctx context.Context, ids []int64, values []float64) error {
	for start := 0; start < len(ids); start += 5000 {
		end := min(start+5000, len(ids))
		if _, err := s.pool.Exec(ctx, `UPDATE media m SET popularity_tmdb = v.p
FROM unnest($1::bigint[], $2::float8[]) AS v(id, p) WHERE m.id = v.id AND m.popularity_tmdb <> v.p::real`, ids[start:end], values[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// Search 在作品库里按片名和别名搜索。匹配程度相同时，短剧排在电影、剧集后面。
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Card, error) {
	normalized := Normalize(query)
	if normalized == "" {
		return []Card{}, nil
	}
	return scanCards(s.pool.Query(ctx, `WITH hits AS (
  SELECT a.media_id, max(CASE
    WHEN a.normalized = $1 THEN 3.0
    WHEN a.normalized LIKE $1 || '%' THEN 2.0
    WHEN a.normalized LIKE '%' || $1 || '%' THEN 1.5
    ELSE similarity(a.normalized, $1) END) AS score
  FROM media_aliases a
  WHERE a.normalized LIKE '%' || $1 || '%' OR a.normalized % $1
  GROUP BY a.media_id
)
SELECT `+cardColumns+` FROM hits h JOIN media m ON m.id = h.media_id
WHERE NOT m.hidden ORDER BY h.score DESC, m.media_type = 'short', m.rating_douban DESC, m.heat DESC, m.id DESC LIMIT $2`, normalized, limit))
}

const briefColumns = `m.id, m.title, m.year, m.media_type, m.season, m.episode_count, m.poster`

func scanBriefs(rows pgx.Rows, err error) ([]Brief, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	briefs := []Brief{}
	for rows.Next() {
		var b Brief
		if err := rows.Scan(&b.ID, &b.Title, &b.Year, &b.Type, &b.Season, &b.EpisodeCount, &b.Poster); err != nil {
			return nil, err
		}
		briefs = append(briefs, b)
	}
	return briefs, rows.Err()
}

// Resolve 把浏览器发来的片名信息对应到作品。先用豆瓣 ID 精确匹配，再用规范化片名加年份、类型匹配。
func (s *Store) Resolve(ctx context.Context, candidates []Candidate) ([]Resolution, error) {
	results := make([]Resolution, len(candidates))
	for index, candidate := range candidates {
		resolution, err := s.resolveOne(ctx, candidate)
		if err != nil {
			return nil, err
		}
		results[index] = resolution
	}
	return results, nil
}

func (s *Store) resolveOne(ctx context.Context, candidate Candidate) (Resolution, error) {
	if ValidDoubanID(candidate.DoubanID) {
		briefs, err := scanBriefs(s.pool.Query(ctx, `SELECT `+briefColumns+` FROM media m WHERE m.douban_id = $1 AND NOT m.hidden`, candidate.DoubanID))
		if err != nil {
			return Resolution{}, err
		}
		if len(briefs) == 1 {
			return Resolution{Status: "matched", Media: &briefs[0]}, nil
		}
	}
	normalized := Normalize(candidate.Title)
	if normalized == "" {
		return Resolution{Status: "unknown"}, nil
	}
	briefs, err := scanBriefs(s.pool.Query(ctx, `SELECT DISTINCT `+briefColumns+` FROM media_aliases a
JOIN media m ON m.id = a.media_id WHERE a.normalized = $1 AND NOT m.hidden ORDER BY m.id LIMIT 20`, normalized))
	if err != nil {
		return Resolution{}, err
	}
	kept := briefs[:0]
	for _, brief := range briefs {
		// 资源站的分类五花八门，这里只区分"电影"和"分集的"两大类。
		if ValidType(candidate.Type) && (candidate.Type == TypeMovie) != (brief.Type == TypeMovie) {
			continue
		}
		if candidate.Year > 0 && brief.Year > 0 && abs(candidate.Year-brief.Year) > 1 {
			continue
		}
		kept = append(kept, brief)
	}
	switch len(kept) {
	case 0:
		return Resolution{Status: "unknown"}, nil
	case 1:
		return Resolution{Status: "matched", Media: &kept[0]}, nil
	}
	if len(kept) > 6 {
		kept = kept[:6]
	}
	return Resolution{Status: "ambiguous", Options: kept}, nil
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// Chart 返回某个榜单的作品。资料还没抓到（没有片名）的先不显示，比如刚上红果榜单、详情还在排队的短剧。
func (s *Store) Chart(ctx context.Context, kind string, limit int) ([]Card, error) {
	return scanCards(s.pool.Query(ctx, `SELECT `+cardColumns+` FROM chart_items c JOIN media m ON m.id = c.media_id
WHERE c.kind = $1 AND NOT m.hidden AND m.title <> '' ORDER BY c.rank LIMIT $2`, kind, limit))
}

// Ranked 是热门榜候选里的一部作品。
type Ranked struct {
	ID   int64
	Type string
}

// ByTMDB 按 TMDB 编号（如 "tv/1399"）找作品库里对应的作品，顺序和传入的一致，对不上的跳过。
// TMDB 一部剧的所有季共用一个编号，这时取最新的一季。
func (s *Store) ByTMDB(ctx context.Context, tmdbIDs []string) ([]Ranked, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (m.tmdb_id) m.tmdb_id, m.id, m.media_type
FROM media m WHERE m.tmdb_id = ANY($1) AND NOT m.hidden ORDER BY m.tmdb_id, m.season DESC, m.year DESC, m.id DESC`, tmdbIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]Ranked{}
	for rows.Next() {
		var tmdbID string
		var item Ranked
		if err := rows.Scan(&tmdbID, &item.ID, &item.Type); err != nil {
			return nil, err
		}
		found[tmdbID] = item
	}
	list := make([]Ranked, 0, len(found))
	for _, tmdbID := range tmdbIDs {
		if item, ok := found[tmdbID]; ok {
			list = append(list, item)
		}
	}
	return list, rows.Err()
}

// Watching 返回最近 days 天本站看的人最多的作品（按人数，同一个人看多少集都只算一次），以及这段时间一共有多少人看过东西。
func (s *Store) Watching(ctx context.Context, days, limit int) ([]Ranked, int, error) {
	var viewers int
	if err := s.pool.QueryRow(ctx, `SELECT count(DISTINCT user_id) FROM watch_days WHERE day > current_date - $1::int`, days).Scan(&viewers); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT m.id, m.media_type FROM playback_positions p JOIN media m ON m.id = p.media_id
WHERE p.activity_at > now() - make_interval(days => $1) AND NOT m.hidden
GROUP BY m.id ORDER BY count(DISTINCT p.user_id) DESC, max(p.activity_at) DESC LIMIT $2`, days, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := []Ranked{}
	for rows.Next() {
		var item Ranked
		if err := rows.Scan(&item.ID, &item.Type); err != nil {
			return nil, 0, err
		}
		list = append(list, item)
	}
	return list, viewers, rows.Err()
}

// ReplaceChart 用新的作品列表整批替换某个榜单。
func (s *Store) ReplaceChart(ctx context.Context, kind string, ids []int64) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM chart_items WHERE kind = $1`, kind); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO chart_items (kind, rank, media_id)
SELECT $1, position, id FROM unnest($2::bigint[]) WITH ORDINALITY AS item(id, position)`, kind, ids)
		return err
	})
}

// Series 返回同一系列的其他季。
func (s *Store) Series(ctx context.Context, m Media) ([]Card, error) {
	if m.SeriesKey == "" {
		return []Card{}, nil
	}
	return scanCards(s.pool.Query(ctx, `SELECT `+cardColumns+` FROM media m
WHERE m.series_key = $1 AND m.id <> $2 AND NOT m.hidden AND (m.media_type = 'movie') = $3
ORDER BY m.season, m.year, m.id LIMIT 30`, m.SeriesKey, m.ID, m.Type == TypeMovie))
}

// Similar 返回相似的作品。有向量时按向量距离，没有时按类型重合度。
func (s *Store) Similar(ctx context.Context, m Media, limit int) ([]Card, error) {
	if m.HasEmbedding {
		cards, err := scanCards(s.pool.Query(ctx, `SELECT `+cardColumns+` FROM media m
WHERE m.id <> $1 AND NOT m.hidden AND m.embedding IS NOT NULL AND m.series_key <> $3
ORDER BY m.embedding <=> (SELECT embedding FROM media WHERE id = $1) LIMIT $2`, m.ID, limit, m.SeriesKey))
		if err != nil || len(cards) > 0 {
			return cards, err
		}
	}
	if len(m.Genres) == 0 {
		return []Card{}, nil
	}
	if m.IsShort() {
		return s.similarShort(ctx, m, limit)
	}
	return scanCards(s.pool.Query(ctx, `SELECT `+cardColumns+` FROM media m
WHERE m.id <> $1 AND NOT m.hidden AND m.media_type = $2 AND m.genres && $3 AND m.series_key <> $5
ORDER BY cardinality(ARRAY(SELECT unnest(m.genres) INTERSECT SELECT unnest($3::text[]))) DESC, m.rating_douban DESC, m.id DESC
LIMIT $4`, m.ID, m.Type, m.Genres, limit, m.SeriesKey))
}

// similarShort 是短剧的相似推荐：短剧没有向量，按题材重合度排。短剧太多，同一个题材动辄几万部，
// 所以先按热度取前 500 部有共同题材的（走热度索引，很快就能凑够），再在里面按重合度排。
// 只推荐短剧：看短剧的人推长剧、看长剧的人推短剧，都不是想要的。
func (s *Store) similarShort(ctx context.Context, m Media, limit int) ([]Card, error) {
	topics := m.Topics()
	if len(topics) == 0 {
		topics = m.Genres
	}
	return scanCards(s.pool.Query(ctx, `WITH pool AS (
  SELECT m.id, m.heat, cardinality(ARRAY(SELECT unnest(m.genres) INTERSECT SELECT unnest($2::text[]))) AS overlap
  FROM media m WHERE m.media_type = 'short' AND NOT m.hidden AND m.id <> $1 AND m.genres && $2 AND m.series_key <> $4 AND m.title <> ''
  ORDER BY m.heat DESC, m.id DESC LIMIT 500
)
SELECT `+cardColumns+` FROM pool p JOIN media m ON m.id = p.id ORDER BY p.overlap DESC, p.heat DESC, p.id DESC LIMIT $3`,
		m.ID, topics, limit, m.SeriesKey))
}

// ShortTopics 返回短剧里最常见的题材（不含剧种），给短剧频道做筛选项。只统计最热的 5000 部：
// 冷门短剧的标签五花八门，统计全部既慢又没有意义。
func (s *Store) ShortTopics(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT g FROM (
  SELECT genres FROM media WHERE media_type = 'short' AND NOT hidden ORDER BY heat DESC, id DESC LIMIT 5000
) top, unnest(top.genres) AS g WHERE g <> ALL($1::text[]) GROUP BY g ORDER BY count(*) DESC, g LIMIT $2`, ShortForms, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	topics := []string{}
	for rows.Next() {
		var topic string
		if err := rows.Scan(&topic); err != nil {
			return nil, err
		}
		topics = append(topics, topic)
	}
	return topics, rows.Err()
}

// VectorLiteral 把向量格式化成 pgvector 能解析的文本。
func VectorLiteral(vector []float32) string {
	var builder strings.Builder
	builder.WriteByte('[')
	for index, value := range vector {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(strconv.FormatFloat(float64(value), 'f', -1, 32))
	}
	builder.WriteByte(']')
	return builder.String()
}

// SaveSemantic 保存 AI 写的语义描述。描述和原来不一样时，旧向量随之清空，等着重算。
// 它和向量分开保存：AI 成功、向量失败时，描述照样留着，重试向量不会再调用一次 AI。
func (s *Store) SaveSemantic(ctx context.Context, id int64, hash, text string) error {
	_, err := s.pool.Exec(ctx, `UPDATE media SET semantic_hash = $2, semantic_text = $3,
embedding = CASE WHEN semantic_text = $3 THEN embedding ELSE NULL END WHERE id = $1`, id, hash, text)
	return err
}

// SetEmbedding 保存向量。text 是算向量时用的语义描述：期间描述又被改写了，这个向量就作废，不保存。
func (s *Store) SetEmbedding(ctx context.Context, id int64, text string, vector []float32) error {
	_, err := s.pool.Exec(ctx, `UPDATE media SET embedding = $3::vector WHERE id = $1 AND semantic_text = $2`, id, text, VectorLiteral(vector))
	return err
}

// MarkEnriched 记下这一轮的 Wikidata、TMDB 资料已经补好。
func (s *Store) MarkEnriched(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE media SET enriched_at = now() WHERE id = $1`, id)
	return err
}

// idle 要求作品没有这种任务正在排队、运行或者处在失败冷却期（见 jobs.Queue.Enqueue）。
// 巡检只挑这样的作品：否则一批一直失败的作品每次都排在最前面，后面的永远轮不到。
const idle = `NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind = $1 AND j.subject = m.id::text AND j.status <> 'done')`

// Stale 返回需要从豆瓣同步的作品 ID（短剧不在内，见 StaleShort）。只同步资料还会变的，其余的同步一次就够了：
//   - 从没同步成功过的；
//   - 还在连载的剧集（TMDB 标了连载中，或者没标状态但是近两年的），每天一次，为了集数和播出时间；
//   - 近两年上映的，每周一次，因为评分还在变。
//
// 更早的作品不再自动同步，后台仍然可以手动刷新。
func (s *Store) Stale(ctx context.Context, jobKind string, limit int) ([]int64, error) {
	return s.ids(ctx, `SELECT id FROM media m WHERE media_type <> 'short' AND metadata_status <> 'not_found' AND `+idle+` AND (
  metadata_synced_at IS NULL
  OR (media_type <> 'movie' AND metadata_synced_at < now() - interval '1 day'
      AND (series_status = 'ongoing' OR (series_status = '' AND year >= $2)))
  OR (year >= $2 AND metadata_synced_at < now() - interval '7 days'
      -- 近期的电影每周重抓主要是为了评分；这一周已经从列表里拿到过评分的，放宽到 30 天
      AND (media_type <> 'movie' OR douban_listed_at IS NULL OR douban_listed_at <= now() - interval '7 days'
           OR metadata_synced_at <= now() - interval '30 days'))
) ORDER BY metadata_synced_at NULLS FIRST, id LIMIT $3`, jobKind, time.Now().Year()-1, limit)
}

// StaleShort 返回需要从红果重新抓的短剧：从没抓到过的，和连载中、一天没更新的（集数还在变）。
// 已完结的只抓一次；热度和评分的变化由每天的榜单和重点剧目刷新负责，不在这里挑。
func (s *Store) StaleShort(ctx context.Context, jobKind string, limit int) ([]int64, error) {
	return s.ids(ctx, `SELECT id FROM media m WHERE media_type = 'short' AND series_status <> 'ended'
AND metadata_status <> 'not_found' AND (metadata_synced_at IS NULL OR metadata_synced_at < now() - interval '1 day') AND `+idle+`
ORDER BY metadata_synced_at NULLS FIRST, id LIMIT $2`, jobKind, limit)
}

// NeedsEnrich 返回豆瓣同步过、但这一轮还没从 Wikidata、TMDB 补到资料的作品。
func (s *Store) NeedsEnrich(ctx context.Context, jobKind string, limit int) ([]int64, error) {
	return s.ids(ctx, `SELECT id FROM media m WHERE media_type <> 'short' AND metadata_status = 'complete' AND NOT hidden AND `+idle+`
AND (enriched_at IS NULL OR enriched_at < metadata_synced_at) ORDER BY metadata_synced_at DESC, id LIMIT $2`, jobKind, limit)
}

// NeedsSemantic 返回资料够了、但还没有语义描述的作品。正在补 Wikidata、TMDB 的先不挑，等它补完，
// 免得 AI 先按不全的资料写一遍、补完又写一遍。
func (s *Store) NeedsSemantic(ctx context.Context, jobKind, enrichKind string, limit int) ([]int64, error) {
	return s.ids(ctx, `SELECT id FROM media m WHERE media_type <> 'short' AND metadata_status = 'complete' AND NOT hidden AND `+idle+`
AND semantic_text = '' AND title <> '' AND (summary <> '' OR original_title <> '')
AND NOT EXISTS (SELECT 1 FROM jobs e WHERE e.kind = $3 AND e.subject = m.id::text AND e.status IN ('queued', 'running'))
ORDER BY id DESC LIMIT $2`, jobKind, limit, enrichKind)
}

// MissingEmbedding 返回有语义描述、但还没有向量的作品。
func (s *Store) MissingEmbedding(ctx context.Context, jobKind string, limit int) ([]int64, error) {
	return s.ids(ctx, `SELECT id FROM media m WHERE media_type <> 'short' AND semantic_text <> '' AND embedding IS NULL AND NOT hidden AND `+idle+`
ORDER BY id DESC LIMIT $2`, jobKind, limit)
}

func (s *Store) ids(ctx context.Context, sql string, args ...any) ([]int64, error) {
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

// SetHidden 隐藏或恢复展示一部作品。
func (s *Store) SetHidden(ctx context.Context, id int64, hidden bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE media SET hidden = $2 WHERE id = $1`, id, hidden)
	return err
}

// ApplyBlockedKeywords 按屏蔽词重新计算哪些作品要隐藏。id 为 0 表示处理全部作品。
// 只隐藏、不自动恢复：管理员手动隐藏的作品不会因为屏蔽词变化而重新出现。
func (s *Store) ApplyBlockedKeywords(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE media m SET hidden = true
WHERE ($1 = 0 OR m.id = $1) AND NOT m.hidden AND EXISTS (
  SELECT 1 FROM blocked_keywords b WHERE m.title ILIKE '%' || b.keyword || '%' OR m.original_title ILIKE '%' || b.keyword || '%')`, id)
	return err
}

// AdminList 给管理后台列出作品，包含隐藏的。query 可以是片名、豆瓣 ID 或红果的 series_id；
// kind 为 "short" 只列短剧，"long" 只列电影、剧集、综艺、动漫，空字符串列全部。
//
// 别名的匹配写成 id IN (子查询)：子查询只执行一次（走别名表的三元组索引），
// 不会对几十万部短剧每一行都去别名表里查一遍。
func (s *Store) AdminList(ctx context.Context, query, kind string, limit, offset int) ([]Media, int, error) {
	pattern := "%" + Normalize(query) + "%"
	where := `($1 = '%%' OR m.douban_id = $2 OR m.hongguo_id = $2 OR m.id IN (SELECT a.media_id FROM media_aliases a WHERE a.normalized LIKE $1))`
	switch kind {
	case "short":
		where += ` AND m.media_type = 'short'`
	case "long":
		where += ` AND m.media_type <> 'short'`
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM media m WHERE `+where, pattern, strings.TrimSpace(query)).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+mediaColumns+` FROM media m WHERE `+where+`
ORDER BY m.id DESC LIMIT $3 OFFSET $4`, pattern, strings.TrimSpace(query), limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := []Media{}
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, m)
	}
	return list, total, rows.Err()
}

// ShortCounts 返回短剧的统计：一共多少部、资料抓到了多少部、还没抓到的多少部（不含确认下架的）。
func (s *Store) ShortCounts(ctx context.Context) (total, complete, waiting int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE metadata_status = 'complete'),
count(*) FILTER (WHERE metadata_synced_at IS NULL) FROM media WHERE media_type = 'short'`).Scan(&total, &complete, &waiting)
	return
}

// Counts 返回作品库的统计数字。
func (s *Store) Counts(ctx context.Context) (total, complete, hidden, embedded int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE metadata_status = 'complete'),
count(*) FILTER (WHERE hidden), count(*) FILTER (WHERE embedding IS NOT NULL) FROM media`).Scan(&total, &complete, &hidden, &embedded)
	return
}

// AiringOn 返回某一天有新一集播出、并且这个用户在追的作品（想看的，或者有播放进度的）。
// 一部作品一行：同一天更新好几集的，给出第一集和最后一集。
func (s *Store) AiringOn(ctx context.Context, userID int64, day time.Time, limit int) ([]Airing, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cardColumns+`, min(u.episode), max(u.episode) FROM media_units u JOIN media m ON m.id = u.media_id
WHERE u.air_date = $2::date AND NOT m.hidden AND (
  EXISTS (SELECT 1 FROM user_movies um WHERE um.user_id = $1 AND um.media_id = m.id AND um.status = 'wish')
  OR EXISTS (SELECT 1 FROM playback_positions p WHERE p.user_id = $1 AND p.media_id = m.id))
GROUP BY m.id ORDER BY m.id LIMIT $3`, userID, day.Format("2006-01-02"), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Airing{}
	for rows.Next() {
		var a Airing
		c := &a.Card
		if err := rows.Scan(&c.ID, &c.Type, &c.Title, &c.Year, &c.Poster, &c.Backdrop, &c.Rating, &c.Season, &c.EpisodeCount, &c.Summary, &c.Genres, &a.First, &a.Last); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// Airing 是一部今天有更新的作品，更新了第 First 到第 Last 集。
type Airing struct {
	Card        Card
	First, Last int
}

// Label 是卡片上的说明："更新第 21 集"，或者一天更新好几集时的"更新第 21–24 集"。综艺是"期"。
// 分集资料（media_units）里的综艺也是按期编号的，不是集号编码。
func (a Airing) Label() string {
	unit := EpisodeUnit(a.Card.Type)
	if a.Last > a.First {
		return fmt.Sprintf("更新第 %d–%d %s", a.First, a.Last, unit)
	}
	return fmt.Sprintf("更新第 %d %s", a.First, unit)
}

// Aliases 返回作品的别名（包括片名和原名），用于在用户的源里搜索。
func (s *Store) Aliases(ctx context.Context, id int64, limit int) ([]string, error) {
	// 别名是给浏览器匹配资源站片名用的，资源站的片名是中文：含汉字的排前面，再按短的优先。
	rows, err := s.pool.Query(ctx, `SELECT alias FROM media_aliases WHERE media_id = $1
ORDER BY (alias ~ '[一-龥]') DESC, length(alias), alias LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	aliases := []string{}
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		aliases = append(aliases, alias)
	}
	return aliases, rows.Err()
}

// Create 新建一部没有豆瓣 ID 的作品（比如以 TMDB 为首个来源的作品，或者演示数据），返回它的 ID。
func (s *Store) Create(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO media DEFAULT VALUES RETURNING id`).Scan(&id)
	return id, err
}

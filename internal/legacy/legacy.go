// Package legacy 把旧版 Moovie 积累的公共数据导入新库。这是新系统和旧版唯一的联系：
// 账号、标记、短评、片单、进度、弹幕等属于用户的数据一概不迁移。
//
// 导入的是：
//   - 采集过的影视元数据：作品资料、外部 ID（豆瓣、TMDB、IMDb、Wikidata）、别名、分集，
//     以及 AI 写的语义描述和向量（重新生成要花钱，而且新旧的生成方式完全一样，见 ingest 包的提示词）；
//   - 众包确认过的广告指纹：只有片段特征的哈希和确认、否认的票数，不关联任何人和任何片子。
//
// 规则：
//   - 源数据只读；所有写入在目标库的一个事务里完成，出错或试运行时整体回滚。
//   - 可以反复运行：按作品 ID"有则更新、无则插入"，ID 保持不变。
//   - 只导入至少有一个豆瓣、TMDB 或 IMDb 外部 ID 的作品；和资源站有关的数据一律不要。
package legacy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"moovie/internal/db"
	"moovie/internal/media"
)

// Report 记录每张表读了多少、写了多少、跳过了多少，以及跳过的原因。
type Report struct {
	tables []string
	stats  map[string]*TableStats
}

// TableStats 是一张表的迁移统计。
type TableStats struct {
	Read, Written int
	Skipped       map[string]int
}

func newReport() *Report { return &Report{stats: map[string]*TableStats{}} }

func (r *Report) table(name string) *TableStats {
	if stats, ok := r.stats[name]; ok {
		return stats
	}
	r.tables = append(r.tables, name)
	r.stats[name] = &TableStats{Skipped: map[string]int{}}
	return r.stats[name]
}

// Stats 返回某张表的统计，没有时返回零值。
func (r *Report) Stats(name string) TableStats {
	if stats, ok := r.stats[name]; ok {
		return *stats
	}
	return TableStats{Skipped: map[string]int{}}
}

// Print 把报告写成人能读的表格。
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "%-24s %8s %8s  %s\n", "表", "读取", "写入", "跳过")
	for _, name := range r.tables {
		stats := r.stats[name]
		reasons := make([]string, 0, len(stats.Skipped))
		for reason, count := range stats.Skipped {
			reasons = append(reasons, fmt.Sprintf("%s %d", reason, count))
		}
		sort.Strings(reasons)
		fmt.Fprintf(w, "%-24s %8d %8d  %s\n", name, stats.Read, stats.Written, strings.Join(reasons, "，"))
	}
}

// Options 是迁移的选项。
type Options struct {
	// DryRun 为 true 时，全部写完后回滚，只输出报告。
	DryRun bool
}

// migrator 持有一次迁移的状态。
type migrator struct {
	ctx    context.Context
	src    *db.Pool
	tx     pgx.Tx
	report *Report
	media  map[int64]bool // 迁移过来的作品
	movies map[int64]bool // 迁移过来的作品里哪些是电影
}

// Run 执行迁移。src 是旧库（只读），dst 是新库（表结构已经建好）。
func Run(ctx context.Context, src, dst *db.Pool, options Options) (*Report, error) {
	if _, err := src.Exec(ctx, `SET default_transaction_read_only = on`); err != nil {
		return nil, err
	}
	tx, err := dst.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	m := &migrator{
		ctx: ctx, src: src, tx: tx, report: newReport(),
		media: map[int64]bool{}, movies: map[int64]bool{},
	}
	steps := []func() error{m.migrateMedia, m.migrateExternalIDs, m.migrateAliases, m.migrateUnits, m.migrateFingerprints, m.finish}
	for _, step := range steps {
		if err := step(); err != nil {
			return m.report, err
		}
	}
	if options.DryRun {
		return m.report, nil
	}
	return m.report, tx.Commit(ctx)
}

// each 逐行读取源库的查询结果。
func (m *migrator) each(query string, scan func(pgx.Rows) error) error {
	rows, err := m.src.Query(m.ctx, query)
	if err != nil {
		return fmt.Errorf("读取旧库: %w\n%s", err, query)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *migrator) exec(stats *TableStats, query string, args ...any) error {
	return m.execWith(stats, "目标库里已经有了", query, args...)
}

// execWith 执行一条写入；没有写入任何行时按 reason 记为跳过。
func (m *migrator) execWith(stats *TableStats, reason, query string, args ...any) error {
	tag, err := m.tx.Exec(m.ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		stats.Written++
	} else {
		stats.Skipped[reason]++
	}
	return nil
}

// ---------- 字段转换 ----------

var leadingInt = regexp.MustCompile(`[0-9]+`)

// firstInt 取文本里的第一个整数，比如 "2024年" → 2024、"45分钟" → 45。
func firstInt(text string) int {
	value, _ := strconv.Atoi(leadingInt.FindString(text))
	return value
}

// splitList 解析旧库里的列表字段：可能是 JSON 数组（字符串或 {name} 对象），也可能是逗号或斜杠分隔的文本。
func splitList(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{}
	}
	if strings.HasPrefix(text, "[") {
		var names []string
		if json.Unmarshal([]byte(text), &names) == nil {
			return clean(names)
		}
		var people []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(text), &people) == nil {
			for _, person := range people {
				names = append(names, person.Name)
			}
			return clean(names)
		}
	}
	return clean(strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '/' || r == '，' || r == '、' }))
}

// splitURLs 解析地址列表：JSON 数组或逗号分隔。地址里有斜杠，不能用 splitList。
func splitURLs(text string) []string {
	text = strings.TrimSpace(text)
	var urls []string
	if strings.HasPrefix(text, "[") && json.Unmarshal([]byte(text), &urls) == nil {
		return clean(urls)
	}
	return clean(strings.Split(text, ","))
}

func clean(values []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func mediaType(old string) string {
	switch old {
	case "movie", "tv", "show":
		return old
	case "cartoon", "anime":
		return media.TypeAnime
	}
	return media.TypeMovie
}

func seriesStatus(old string) string {
	switch strings.ToLower(strings.TrimSpace(old)) {
	case "returning series", "in production", "planned", "ongoing":
		return "ongoing"
	case "ended", "canceled", "cancelled", "released":
		return "ended"
	}
	return ""
}

func (m *migrator) migrateMedia() error {
	stats := m.report.table("media")
	// 先算出每部作品的集数（旧库没有单独的集数字段）。
	episodes := map[int64]int{}
	if err := m.each(`SELECT media_id, max(episode_number) FROM media_units WHERE unit_type = 'episode' GROUP BY media_id`, func(rows pgx.Rows) error {
		var id int64
		var count *int
		if err := rows.Scan(&id, &count); err != nil {
			return err
		}
		if count != nil {
			episodes[id] = *count
		}
		return nil
	}); err != nil {
		return err
	}
	return m.each(`SELECT m.id, m.media_type, m.title, COALESCE(m.original_title, ''), COALESCE(m.year, ''), COALESCE(m.poster, ''),
COALESCE(m.backdrops, ''), COALESCE(m.summary, ''), COALESCE(m.genres, ''), COALESCE(m.countries, ''), COALESCE(m.directors, ''),
COALESCE(m.actors, ''), COALESCE(m.duration, ''), COALESCE(m.rating_douban, 0), COALESCE(m.rating_tmdb, 0), COALESCE(m.vote_count_tmdb, 0),
COALESCE(m.series_status, ''), COALESCE(m.metadata_status, ''), m.last_metadata_sync_at, m.created_at, m.updated_at,
COALESCE(NULLIF(m.douban_id, ''), (SELECT e.external_id FROM media_external_ids e WHERE e.media_id = m.id AND e.provider = 'douban' LIMIT 1), ''),
m.embedding::text, COALESCE(m.embedding_content, ''),
EXISTS (SELECT 1 FROM media_external_ids e WHERE e.media_id = m.id AND e.provider IN ('douban', 'tmdb', 'imdb'))
FROM media m ORDER BY m.id`, func(rows pgx.Rows) error {
		var id int64
		var kind, title, original, year, poster, backdrops, summary, genres, countries, directors, actors, duration, series, status, doubanID string
		var ratingDouban, ratingTMDB float64
		var votes int
		var synced *time.Time
		var created, updated time.Time
		var embedding *string
		var semanticText string
		var hasExternal bool
		if err := rows.Scan(&id, &kind, &title, &original, &year, &poster, &backdrops, &summary, &genres, &countries, &directors, &actors,
			&duration, &ratingDouban, &ratingTMDB, &votes, &series, &status, &synced, &created, &updated, &doubanID, &embedding, &semanticText, &hasExternal); err != nil {
			return err
		}
		stats.Read++
		if doubanID == "" && !hasExternal {
			stats.Skipped["没有豆瓣、TMDB 或 IMDb 的 ID"]++
			return nil
		}
		if strings.TrimSpace(title) == "" {
			stats.Skipped["没有片名"]++
			return nil
		}
		if !media.ValidDoubanID(doubanID) {
			doubanID = ""
		}
		kind = mediaType(kind)
		_, season := media.SplitSeason(title)
		metadataStatus := "partial"
		if status == "ready" || status == "complete" {
			metadataStatus = "complete"
		}
		backdropList := splitURLs(backdrops)
		for index, url := range backdropList {
			// 旧库存的是 w1280 尺寸，新版按需要换尺寸，统一存原图地址。
			backdropList[index] = regexp.MustCompile(`/t/p/[a-z0-9]+/`).ReplaceAllString(url, "/t/p/original/")
		}
		episodeCount := 0
		if kind != media.TypeMovie {
			episodeCount = episodes[id]
		}
		if embedding != nil && strings.Count(*embedding, ",") != 767 {
			embedding = nil // 维度不对的向量不要，后台会重新生成
		}
		actorList := splitList(actors)
		if len(actorList) > 12 {
			actorList = actorList[:12]
		}
		row := media.Media{
			Title: strings.TrimSpace(title), OriginalTitle: strings.TrimSpace(original), Year: firstInt(year), Summary: summary,
			Genres: splitList(genres), Countries: splitList(countries), Directors: splitList(directors), Actors: actorList,
		}
		// 老系统 AI 写的语义描述原样带过来，哈希按新系统的算法用迁移后的资料重算：
		// 之后豆瓣资料没变，新系统就认得这段描述，不会对所有作品重新调用一遍 AI。
		semanticText = strings.TrimSpace(semanticText)
		semanticHash := ""
		if semanticText != "" {
			semanticHash = row.InputHash()
		}
		// 老系统资料完整的作品已经补过 TMDB，记成补过，免得上线后全部重新补一遍。
		var enriched *time.Time
		if metadataStatus == "complete" {
			enriched = synced
		}
		_, err := m.tx.Exec(m.ctx, `INSERT INTO media (id, media_type, douban_id, title, original_title, series_key, year, season, episode_count,
poster, backdrops, summary, genres, countries, directors, actors, duration_minutes, rating_douban, rating_tmdb, votes_tmdb,
series_status, metadata_status, metadata_synced_at, embedding, created_at, updated_at, enriched_at, semantic_text, semantic_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24::vector, $25, $26, $27, $28, $29)
ON CONFLICT (id) DO UPDATE SET media_type = EXCLUDED.media_type, douban_id = EXCLUDED.douban_id, title = EXCLUDED.title,
original_title = EXCLUDED.original_title, series_key = EXCLUDED.series_key, year = EXCLUDED.year, season = EXCLUDED.season,
episode_count = EXCLUDED.episode_count, poster = EXCLUDED.poster, backdrops = EXCLUDED.backdrops, summary = EXCLUDED.summary,
genres = EXCLUDED.genres, countries = EXCLUDED.countries, directors = EXCLUDED.directors, actors = EXCLUDED.actors,
duration_minutes = EXCLUDED.duration_minutes, rating_douban = EXCLUDED.rating_douban, rating_tmdb = EXCLUDED.rating_tmdb,
votes_tmdb = EXCLUDED.votes_tmdb, series_status = EXCLUDED.series_status, metadata_status = EXCLUDED.metadata_status,
metadata_synced_at = EXCLUDED.metadata_synced_at, embedding = EXCLUDED.embedding, updated_at = EXCLUDED.updated_at,
enriched_at = EXCLUDED.enriched_at, semantic_text = EXCLUDED.semantic_text, semantic_hash = EXCLUDED.semantic_hash`,
			id, kind, doubanID, row.Title, row.OriginalTitle, media.SeriesKey(title), row.Year, season, episodeCount,
			poster, backdropList, summary, row.Genres, row.Countries, row.Directors, actorList, firstInt(duration),
			ratingDouban, ratingTMDB, votes, seriesStatus(series), metadataStatus, synced, embedding, created, updated,
			enriched, semanticText, semanticHash)
		if db.IsUniqueViolation(err) {
			stats.Skipped["豆瓣 ID 和已有作品重复"]++
			return nil
		}
		if err != nil {
			return fmt.Errorf("写入作品 %d: %w", id, err)
		}
		stats.Written++
		m.media[id] = true
		if kind == media.TypeMovie {
			m.movies[id] = true
		}
		// 片名和原名作为别名，供匹配和搜索使用。
		for _, alias := range []string{title, original} {
			if normalized := media.Normalize(alias); normalized != "" {
				if _, err := m.tx.Exec(m.ctx, `INSERT INTO media_aliases (media_id, alias, normalized) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING`, id, strings.TrimSpace(alias), normalized); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (m *migrator) migrateExternalIDs() error {
	stats := m.report.table("media_external_ids")
	return m.each(`SELECT media_id, provider, external_id, COALESCE(external_type, '') FROM media_external_ids
WHERE provider IN ('tmdb', 'imdb', 'wikidata') ORDER BY media_id, is_primary DESC, id`, func(rows pgx.Rows) error {
		var mediaID int64
		var provider, externalID, externalType string
		if err := rows.Scan(&mediaID, &provider, &externalID, &externalType); err != nil {
			return err
		}
		stats.Read++
		if !m.media[mediaID] {
			stats.Skipped["作品没有迁移"]++
			return nil
		}
		externalID = strings.TrimSpace(externalID)
		if provider == "tmdb" && !strings.Contains(externalID, "/") {
			kind := "tv"
			if strings.HasPrefix(externalType, "movie") || (externalType == "" && m.movies[mediaID]) {
				kind = "movie"
			}
			externalID = kind + "/" + externalID
		}
		// 新库里各来源的 ID 是 media 表上的列（豆瓣 ID 已经在迁移作品时写入）。
		column := map[string]string{"tmdb": "tmdb_id", "imdb": "imdb_id", "wikidata": "wikidata_id"}[provider]
		return m.execWith(stats, "同一作品在同一来源有多个 ID（保留第一个）",
			`UPDATE media SET `+column+` = $2 WHERE id = $1 AND `+column+` = ''`, mediaID, externalID)
	})
}

func (m *migrator) migrateAliases() error {
	stats := m.report.table("media_aliases")
	return m.each(`SELECT media_id, alias FROM media_aliases ORDER BY id`, func(rows pgx.Rows) error {
		var mediaID int64
		var alias string
		if err := rows.Scan(&mediaID, &alias); err != nil {
			return err
		}
		stats.Read++
		normalized := media.Normalize(alias)
		if !m.media[mediaID] || normalized == "" {
			stats.Skipped["作品没有迁移或别名为空"]++
			return nil
		}
		return m.execWith(stats, "和片名或其他别名重复", `INSERT INTO media_aliases (media_id, alias, normalized) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING`, mediaID, strings.TrimSpace(alias), normalized)
	})
}

func (m *migrator) migrateUnits() error {
	stats := m.report.table("media_units")
	return m.each(`SELECT id, media_id, unit_type, COALESCE(season_number, 1), COALESCE(episode_number, 0), COALESCE(title, ''),
air_date, COALESCE(runtime_minutes, 0) FROM media_units ORDER BY id`, func(rows pgx.Rows) error {
		var id, mediaID int64
		var unitType, title string
		var season, episode, runtime int
		var airDate *time.Time
		if err := rows.Scan(&id, &mediaID, &unitType, &season, &episode, &title, &airDate, &runtime); err != nil {
			return err
		}
		stats.Read++
		if !m.media[mediaID] {
			stats.Skipped["作品没有迁移"]++
			return nil
		}
		switch {
		case unitType == "feature":
			stats.Skipped["电影正片（新版不需要）"]++
			return nil
		case unitType != "episode":
			stats.Skipped["预告、花絮等"]++
			return nil
		case episode <= 0:
			stats.Skipped["没有集号（综艺特辑等，新版按集号记录）"]++
			return nil
		}
		if season <= 0 {
			season = 1
		}
		return m.exec(stats, `INSERT INTO media_units (media_id, season, episode, title, air_date, runtime_minutes) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (media_id, season, episode) DO UPDATE SET title = EXCLUDED.title, air_date = EXCLUDED.air_date, runtime_minutes = EXCLUDED.runtime_minutes`,
			mediaID, season, episode, title, airDate, runtime)
	})
}

// migrateFingerprints 导入广告指纹和它们的票数。新旧播放器算指纹的方法一样（web/static/js/lib/m3u8.js），
// 旧版确认过的指纹在新版里直接生效。长度不是 32 字节的不要。
func (m *migrator) migrateFingerprints() error {
	stats := m.report.table("ad_fingerprints")
	return m.each(`SELECT fingerprint, COALESCE(confirm_count, 0), COALESCE(reject_count, 0) FROM ad_fingerprints`, func(rows pgx.Rows) error {
		var fingerprint []byte
		var confirm, reject int
		if err := rows.Scan(&fingerprint, &confirm, &reject); err != nil {
			return err
		}
		stats.Read++
		if len(fingerprint) != 32 {
			stats.Skipped["长度不对"]++
			return nil
		}
		return m.exec(stats, `INSERT INTO ad_fingerprints (fingerprint, confirm_count, reject_count) VALUES ($1, $2, $3)
ON CONFLICT (fingerprint) DO UPDATE SET confirm_count = EXCLUDED.confirm_count, reject_count = EXCLUDED.reject_count, updated_at = now()`,
			fingerprint, max(confirm, 0), max(reject, 0))
	})
}

// finish 让作品 ID 的序列从迁移过来的最大 ID 之后开始。
func (m *migrator) finish() error {
	_, err := m.tx.Exec(m.ctx, `SELECT setval(pg_get_serial_sequence('media', 'id'), GREATEST((SELECT COALESCE(max(id), 0) FROM media), 1))`)
	return err
}

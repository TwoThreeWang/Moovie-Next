package app

import (
	"encoding/xml"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// 每个文件使用固定的 ID 范围。新增、隐藏和删除都不会让其他范围的 URL 改换文件。
const (
	sitemapShardSize = 10_000
	sitemapNamespace = "http://www.sitemaps.org/schemas/sitemap/0.9"
)

// sitemapSource 是一类可收录的页面：表、筛选条件和页面地址。
type sitemapSource struct {
	from, where, key, prefix string
}

var sitemapSources = map[string]sitemapSource{
	"collections": {from: "collections c", key: "slug", prefix: "/list/", where: `c.status = 'published' AND EXISTS (
		SELECT 1 FROM collection_items i JOIN media m ON m.id = i.media_id
		WHERE i.collection_id = c.id AND NOT m.hidden)`},
	// 短剧不进站点地图（页面也是 noindex，见 mediaSEO）。
	"media": {from: "media", key: "id::text", prefix: "/m/",
		where: `media_type <> 'short' AND NOT hidden AND title <> '' AND metadata_status = 'complete'`},
}

// shardsSQL 找出有内容的分片。每一步沿主键跳到下一个分片里第一条符合条件的记录，
// 查询次数等于分片数，不需要扫描整张表再去重。
func (s sitemapSource) shardsSQL() string {
	return `WITH RECURSIVE shard(first) AS (
  SELECT min(id) FROM ` + s.from + ` WHERE ` + s.where + `
  UNION ALL
  SELECT (SELECT min(id) FROM ` + s.from + ` WHERE ` + s.where + ` AND id > ((shard.first - 1) / $1::bigint + 1) * $1::bigint)
  FROM shard WHERE shard.first IS NOT NULL
) SELECT (first - 1) / $1::bigint + 1 FROM shard WHERE first IS NOT NULL`
}

type sitemapEntry struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

type sitemapIndexXML struct {
	XMLName  xml.Name       `xml:"sitemapindex"`
	XMLNS    string         `xml:"xmlns,attr"`
	Sitemaps []sitemapEntry `xml:"sitemap"`
}

type sitemapFileXML struct {
	XMLName xml.Name       `xml:"urlset"`
	XMLNS   string         `xml:"xmlns,attr"`
	URLs    []sitemapEntry `xml:"url"`
}

func (a *App) sitemapIndex(c *gin.Context) {
	index := sitemapIndexXML{XMLNS: sitemapNamespace, Sitemaps: []sitemapEntry{{Loc: a.absolute("/sitemap-pages.xml")}}}
	for _, kind := range []string{"collections", "media"} {
		rows, err := a.Pool.Query(c.Request.Context(), sitemapSources[kind].shardsSQL(), sitemapShardSize)
		if err != nil {
			a.fail(c, err)
			return
		}
		shards, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			a.fail(c, err)
			return
		}
		for _, shard := range shards {
			index.Sitemaps = append(index.Sitemaps, sitemapEntry{Loc: a.absolute("/sitemap-" + kind + "-" + strconv.FormatInt(shard, 10) + ".xml")})
		}
	}
	a.writeSitemap(c, index)
}

func (a *App) sitemapFile(c *gin.Context) {
	file := c.Param("file")
	sitemap := sitemapFileXML{XMLNS: sitemapNamespace}
	if file == "pages.xml" {
		for _, path := range []string{"/", "/discover", "/discover/movie", "/discover/tv", "/discover/show", "/discover/anime",
			"/list", "/trends", "/m3u8", "/cinema", "/about", "/install"} {
			sitemap.URLs = append(sitemap.URLs, sitemapEntry{Loc: a.absolute(path)})
		}
		a.writeSitemap(c, sitemap)
		return
	}

	base, isXML := strings.CutSuffix(file, ".xml")
	kind, number, valid := strings.Cut(base, "-")
	source, known := sitemapSources[kind]
	shard, err := strconv.ParseInt(number, 10, 64)
	if !isXML || !valid || !known || err != nil || shard < 1 || shard > math.MaxInt64/sitemapShardSize+1 ||
		strconv.FormatInt(shard, 10) != number {
		c.Status(http.StatusNotFound)
		return
	}
	first := (shard - 1) * sitemapShardSize
	last := first + min(sitemapShardSize, math.MaxInt64-first)
	rows, err := a.Pool.Query(c.Request.Context(), "SELECT "+source.key+", updated_at FROM "+source.from+" WHERE "+source.where+
		" AND id > $1 AND id <= $2 ORDER BY id", first, last)
	if err != nil {
		a.fail(c, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var updated time.Time
		if err := rows.Scan(&key, &updated); err != nil {
			a.fail(c, err)
			return
		}
		sitemap.URLs = append(sitemap.URLs, sitemapEntry{Loc: a.absolute(source.prefix + key), LastMod: updated.UTC().Format(time.RFC3339)})
	}
	if err := rows.Err(); err != nil {
		a.fail(c, err)
		return
	}
	if len(sitemap.URLs) == 0 {
		c.Status(http.StatusNotFound)
		return
	}
	a.writeSitemap(c, sitemap)
}

func (a *App) writeSitemap(c *gin.Context, data any) {
	body, err := xml.MarshalIndent(data, "", "  ")
	if err != nil {
		a.fail(c, err)
		return
	}
	c.Header("Cache-Control", "public, max-age=300")
	c.Data(http.StatusOK, "application/xml; charset=utf-8", append([]byte(xml.Header), body...))
}

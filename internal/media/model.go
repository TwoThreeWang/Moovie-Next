// Package media 管理作品元数据：存储、多来源字段合并、搜索，以及把片名对应到作品。
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 作品类型。
const (
	TypeMovie = "movie"
	TypeTV    = "tv"
	TypeShow  = "show"
	TypeAnime = "anime"
	TypeShort = "short" // 短剧，资料来自红果短剧
)

// Types 是所有作品类型和它们的中文名，按展示顺序排列。
var Types = []struct{ Key, Label string }{
	{TypeMovie, "电影"}, {TypeTV, "剧集"}, {TypeShow, "综艺"}, {TypeAnime, "动漫"}, {TypeShort, "短剧"},
}

// ShortForms 是短剧的剧种。详情页不给剧种，只有分类页和分剧种的榜单给，所以只有上过这些列表的短剧才知道剧种；
// 知道的就作为一个值放进 genres，和题材一起筛选。
var ShortForms = []string{"真人剧", "漫剧", "AI剧"}

// IsShortForm 判断一个类型值是不是短剧的剧种。
func IsShortForm(genre string) bool {
	for _, form := range ShortForms {
		if form == genre {
			return true
		}
	}
	return false
}

// TypeLabel 返回类型的中文名。
func TypeLabel(mediaType string) string {
	for _, item := range Types {
		if item.Key == mediaType {
			return item.Label
		}
	}
	return ""
}

// ValidType 判断是不是合法的作品类型。
func ValidType(mediaType string) bool { return TypeLabel(mediaType) != "" }

// 元数据来源。数值越大优先级越高，高优先级来源的字段不会被低优先级来源覆盖。
const (
	ProviderManual   = "manual"
	ProviderDouban   = "douban"
	ProviderTMDB     = "tmdb"
	ProviderIMDb     = "imdb"
	ProviderWikidata = "wikidata"
	ProviderHongguo  = "hongguo" // 短剧的唯一来源
)

// ErrNotFound 表示作品不存在。
var ErrNotFound = errors.New("作品不存在")

// Picture 给组合封面保留所属作品的片名，供图片签名和 Telegram 文件命名使用。
type Picture struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

// Media 是一部作品的完整资料。
type Media struct {
	ID               int64
	Type             string
	DoubanID         string
	TMDBID           string // 带类型前缀，如 "movie/603"、"tv/1399"
	IMDbID           string
	WikidataID       string
	HongguoID        string // 红果的 series_id，只有短剧有
	Title            string
	OriginalTitle    string
	Year             int
	Season           int
	EpisodeCount     int
	Poster           string
	Backdrops        []string
	Summary          string
	Genres           []string
	Countries        []string
	Directors        []string
	Actors           []string
	DurationMinutes  int
	RatingDouban     float64
	RatingTMDB       float64
	VotesTMDB        int
	RatingHongguo    float64
	Heat             int64
	SeriesStatus     string
	SeriesKey        string
	Hidden           bool
	MetadataStatus   string
	MetadataSyncedAt *time.Time
	EnrichedAt       *time.Time
	SemanticText     string
	SemanticHash     string
	HasEmbedding     bool
	UpdatedAt        time.Time
}

// SemanticReady 表示资料够不够让 AI 写语义描述：豆瓣的资料已经同步，至少有片名和简介（或原名）。
func (m Media) SemanticReady() bool {
	return m.MetadataStatus == "complete" && !m.Hidden && m.Title != "" && (m.Summary != "" || m.OriginalTitle != "")
}

// SemanticInput 是发给 AI 的资料。格式和老系统一字不差：老系统迁移过来的语义描述是按这个格式生成的，
// 新写的描述要和它们风格一致，向量推荐才准。主演只取前 5 位，后面的演员有变化也不必重写描述。
func (m Media) SemanticInput() string {
	year := ""
	if m.Year > 0 {
		year = strconv.Itoa(m.Year)
	}
	return fmt.Sprintf("标题：%s\n原名：%s\n年份：%s\n地区：%s\n类型：%s\n导演：%s\n主演：%s\n剧情：%s",
		m.Title, m.OriginalTitle, year, strings.Join(m.Countries, ","), strings.Join(m.Genres, ","),
		strings.Join(m.Directors, ","), strings.Join(m.Actors[:min(len(m.Actors), 5)], ","), m.Summary)
}

// InputHash 是 SemanticInput 的哈希。和保存的 SemanticHash 不同时，才需要重新调用 AI。
func (m Media) InputHash() string {
	sum := sha256.Sum256([]byte(m.SemanticInput()))
	return hex.EncodeToString(sum[:16])
}

// TypeLabel 返回这部作品类型的中文名。
func (m Media) TypeLabel() string { return TypeLabel(m.Type) }

// IsSeries 表示这部作品是不是分集的。
func (m Media) IsSeries() bool { return m.Type != TypeMovie }

// IsShort 表示这部作品是不是短剧。
func (m Media) IsShort() bool { return m.Type == TypeShort }

// Rating 返回用于展示的评分：优先豆瓣，没有就用 TMDB；短剧用红果的评分。
func (m Media) Rating() float64 {
	if m.RatingDouban > 0 {
		return m.RatingDouban
	}
	if m.RatingTMDB > 0 {
		return m.RatingTMDB
	}
	return m.RatingHongguo
}

// Form 返回短剧的剧种（真人剧、漫剧、AI剧），不知道时返回空字符串。
func (m Media) Form() string {
	for _, genre := range m.Genres {
		if IsShortForm(genre) {
			return genre
		}
	}
	return ""
}

// Topics 返回去掉剧种以后的题材。
func (m Media) Topics() []string {
	topics := make([]string, 0, len(m.Genres))
	for _, genre := range m.Genres {
		if !IsShortForm(genre) {
			topics = append(topics, genre)
		}
	}
	return topics
}

// Backdrop 返回第一张剧照，没有时用海报代替。
func (m Media) Backdrop() string {
	if len(m.Backdrops) > 0 {
		return m.Backdrops[0]
	}
	return m.Poster
}

// Card 是列表里一张卡片需要的资料。
type Card struct {
	ID           int64
	Type         string
	Title        string
	Year         int
	Poster       string
	Backdrop     string
	Rating       float64
	Season       int
	EpisodeCount int
	Summary      string
	Genres       []string
}

// TypeLabel 返回类型的中文名。
func (c Card) TypeLabel() string { return TypeLabel(c.Type) }

// IsShort 表示这张卡片是不是短剧。
func (c Card) IsShort() bool { return c.Type == TypeShort }

// Unit 是一集的展示信息。
type Unit struct {
	Season         int
	Episode        int
	Title          string
	AirDate        *time.Time
	RuntimeMinutes int
}

// Patch 是某个来源提供的一组字段。键是 media 表的列名，值的类型必须和列一致：
// 文本列用 string，整数列用 int（heat 用 int64），评分用 float64，数组列用 []string。
// 一般不用写 series_key：写 title 时会按片名算出来；明确给了 series_key 时以给的为准（短剧按红果的系列 ID 归组）。
// 用 Set 写入可以自动跳过空值。
type Patch map[string]any

// Set 写入一个字段；空字符串、0 和空数组会被忽略，这样来源缺失的字段不会清掉已有数据。
func (p Patch) Set(field string, value any) {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return
		}
	case int:
		if typed == 0 {
			return
		}
	case int64:
		if typed == 0 {
			return
		}
	case float64:
		if typed == 0 {
			return
		}
	case []string:
		if len(typed) == 0 {
			return
		}
	}
	p[field] = value
}

// patchFields 是允许通过 Patch 写入的列，按固定顺序处理。
var patchFields = []string{
	"media_type", "title", "original_title", "year", "season", "episode_count", "poster", "backdrops",
	"summary", "genres", "countries", "directors", "actors", "duration_minutes",
	"rating_douban", "rating_tmdb", "votes_tmdb", "series_status",
	"rating_hongguo", "heat", "series_key",
}

// Candidate 是浏览器发来的一条待匹配信息。只有片名这类信息，没有任何源信息。
type Candidate struct {
	DoubanID string `json:"douban_id"`
	Title    string `json:"title"`
	Year     int    `json:"year"`
	Type     string `json:"type"`
}

// Resolution 是一条待匹配信息的匹配结果。
type Resolution struct {
	// Status 取值：matched（唯一匹配）、ambiguous（多个候选，需要用户选）、unknown（作品库里没有）。
	Status  string  `json:"status"`
	Media   *Brief  `json:"media,omitempty"`
	Options []Brief `json:"options,omitempty"`
}

// Brief 是匹配结果里的作品摘要。
type Brief struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	Year         int    `json:"year"`
	Type         string `json:"type"`
	Season       int    `json:"season"`
	EpisodeCount int    `json:"episode_count"`
	Poster       string `json:"poster"`
}

// Today 返回本地时区"今天"对应的日期（UTC 零点），用来和数据库里的日期列比较。
// 不能用 time.Now().Truncate(24h)：那是按 UTC 截断，本地时间的凌晨会被算成前一天。
func Today() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

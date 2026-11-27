package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// NeoDB 是 NeoDB（neodb.social，开源的书影音标记站，收录了大量豆瓣条目，带着豆瓣链接）的只读客户端。
// 作品还没从豆瓣同步时，用它补中文简介、又名、类型、导演和主演，排在 TMDB 前面。
//
// 只用搜索接口（/api/catalog/search）找它已经收录的条目，不用 /api/catalog/fetch：
// 那个接口碰上它没收录的条目会替我们去抓豆瓣，等于把我们的请求转嫁到这个志愿者维护的站点上。
// 评分是 NeoDB 自己用户打的分，不是豆瓣评分，不取。
type NeoDB struct {
	HTTP      *http.Client
	Base      string // 比如 https://neodb.social，不带结尾的 /；为空表示不用 NeoDB
	UserAgent string
	Pace      *Pacer
}

// neodbInterval 是两次请求 NeoDB 之间至少隔多久。它是志愿者维护的站点，慢一点。
const neodbInterval = 2 * time.Second

// NewNeoDB 创建 NeoDB 客户端。base 为空时不用 NeoDB。
func NewNeoDB(client *http.Client, base, userAgent string) *NeoDB {
	pace := NewPacer(neodbInterval)
	pace.Name = "NeoDB"
	return &NeoDB{HTTP: client, Base: strings.TrimRight(base, "/"), UserAgent: userAgent, Pace: pace}
}

// Enabled 表示是否配置了 NeoDB。
func (n *NeoDB) Enabled() bool { return n != nil && n.Base != "" }

// NeoDBQuery 是要在 NeoDB 上找的作品。
type NeoDBQuery struct {
	DoubanID string
	IMDbID   string
	Title    string // 用来搜索
	TV       bool
	Season   int // 剧集是第几季，用来核对 TMDB 链接
}

// NeoDBItem 是 NeoDB 上和作品对上了的条目里有用的资料。
type NeoDBItem struct {
	Title         string // 简体中文片名
	OriginalTitle string
	AKA           []string // 其余的中文片名
	Summary       string   // 中文简介，没有中文的不取
	Genres        []string // 换成豆瓣用的中文类型名，认不出的不取
	Directors     []string
	Actors        []string
	IMDbID        string
	TMDBID        string // 带类型前缀，如 "movie/278"、"tv/64197"；对不上这一季或者有好几个时为空
}

type neodbLocalized struct {
	Lang string `json:"lang"`
	Text string `json:"text"`
}

// neodbPerson 兼容 NeoDB 演职员字段的两种格式：字符串（如 director）和 {"name": "..."} 对象（如 actor）。
type neodbPerson string

func (p *neodbPerson) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		*p = neodbPerson(strings.TrimSpace(name))
		return nil
	}
	var obj struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*p = neodbPerson(strings.TrimSpace(obj.Name))
	return nil
}

func neodbNames(people []neodbPerson) []string {
	names := make([]string, 0, len(people))
	for _, person := range people {
		if name := string(person); name != "" {
			names = append(names, name)
		}
	}
	return names
}

type neodbEntry struct {
	Type                 string           `json:"type"`
	OrigTitle            string           `json:"orig_title"`
	Description          string           `json:"description"`
	LocalizedTitle       []neodbLocalized `json:"localized_title"`
	LocalizedDescription []neodbLocalized `json:"localized_description"`
	Genre                []string         `json:"genre"`
	Director             []neodbPerson    `json:"director"`
	Actor                []neodbPerson    `json:"actor"`
	ExternalResources    []struct {
		URL string `json:"url"`
	} `json:"external_resources"`
}

var (
	neodbDoubanLink = regexp.MustCompile(`douban\.com/subject/([0-9]+)`)
	neodbIMDbLink   = regexp.MustCompile(`imdb\.com/title/(tt[0-9]+)`)
	neodbTMDBLink   = regexp.MustCompile(`themoviedb\.org/(movie|tv)/([0-9]+)(?:/season/([0-9]+))?`)
)

// Find 按片名搜索，返回和作品对上的条目。对上的条件：条目带着同一个豆瓣链接；电影也可以是同一个 IMDb 编号、没挂豆瓣链接
// （剧集的 IMDb 编号是整部剧的，分不出第几季，只认豆瓣链接，豆瓣每一季是一个条目）。没对上时返回 ErrNotFound。
func (n *NeoDB) Find(ctx context.Context, query NeoDBQuery) (*NeoDBItem, error) {
	if strings.TrimSpace(query.Title) == "" {
		return nil, ErrNotFound
	}
	category := "movie"
	if query.TV {
		category = "tv"
	}
	values := url.Values{"query": {query.Title}, "category": {category}, "page": {"1"}}
	var response struct {
		Data []neodbEntry `json:"data"`
	}
	if err := n.get(ctx, "/api/catalog/search?"+values.Encode(), &response); err != nil {
		return nil, err
	}
	// 先找带着同一个豆瓣链接的，没有再按 IMDb 编号找没挂豆瓣链接的条目：同一个 IMDb 编号可能挂在好几个条目上，
	// 挂着别的豆瓣链接的是豆瓣上的另一个条目。
	for _, byIMDb := range []bool{false, true} {
		if byIMDb && (query.TV || query.IMDbID == "") {
			break
		}
		for _, entry := range response.Data {
			links := neodbLinks(entry)
			if (!byIMDb && query.DoubanID != "" && slices.Contains(links.douban, query.DoubanID)) ||
				(byIMDb && len(links.douban) == 0 && slices.Contains(links.imdb, query.IMDbID)) {
				return entry.item(links, query), nil
			}
		}
	}
	return nil, ErrNotFound
}

func (n *NeoDB) get(ctx context.Context, path string, destination any) error {
	if n.Pace != nil {
		if err := n.Pace.Wait(ctx); err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, n.Base+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", n.UserAgent)
	err = doJSON(n.HTTP, request, "NeoDB", destination)
	switch {
	case n.Pace == nil:
	case err == nil:
		n.Pace.Succeeded()
	case errors.Is(err, ErrThrottled):
		if until := n.Pace.Throttled(); !until.IsZero() {
			return &PausedError{Name: n.Pace.Name, Until: until, Cause: err}
		}
	}
	return err
}

type neodbLinkSet struct {
	douban, imdb []string
	tmdb         []neodbTMDB
}

type neodbTMDB struct {
	kind, id string
	season   int // -1 表示整剧链接，0 是特别篇
}

func neodbLinks(entry neodbEntry) neodbLinkSet {
	var links neodbLinkSet
	for _, resource := range entry.ExternalResources {
		if match := neodbDoubanLink.FindStringSubmatch(resource.URL); match != nil {
			links.douban = append(links.douban, match[1])
		}
		if match := neodbIMDbLink.FindStringSubmatch(resource.URL); match != nil {
			links.imdb = append(links.imdb, match[1])
		}
		if match := neodbTMDBLink.FindStringSubmatch(resource.URL); match != nil {
			season := -1
			if match[3] != "" {
				var err error
				season, err = strconv.Atoi(match[3])
				if err != nil {
					continue
				}
			}
			links.tmdb = append(links.tmdb, neodbTMDB{kind: match[1], id: match[2], season: season})
		}
	}
	return links
}

// item 整理出有用的资料。TMDB 链接只认这一季的那一个：一部剧的续作有时同时挂着前作的"第二季"和自己的"第一季"。
func (entry neodbEntry) item(links neodbLinkSet, query NeoDBQuery) *NeoDBItem {
	item := &NeoDBItem{OriginalTitle: strings.TrimSpace(entry.OrigTitle), Directors: neodbNames(entry.Director), Actors: neodbNames(entry.Actor)}
	for _, title := range entry.LocalizedTitle {
		text := strings.TrimSpace(title.Text)
		if !strings.HasPrefix(title.Lang, "zh") || text == "" {
			continue
		}
		if item.Title == "" && simplifiedChinese(title.Lang) {
			item.Title = text
		} else if text != item.Title && !slices.Contains(item.AKA, text) {
			item.AKA = append(item.AKA, text)
		}
	}
	for _, description := range entry.LocalizedDescription {
		if simplifiedChinese(description.Lang) && hasHan(description.Text) {
			item.Summary = strings.TrimSpace(description.Text)
			break
		}
	}
	if item.Summary == "" && hasHan(entry.Description) {
		item.Summary = strings.TrimSpace(entry.Description)
	}
	for _, genre := range entry.Genre {
		if name := doubanGenre(genre); name != "" && !slices.Contains(item.Genres, name) {
			item.Genres = append(item.Genres, name)
		}
	}
	if len(links.imdb) > 0 {
		item.IMDbID = links.imdb[0]
	}
	season := max(query.Season, 1)
	var tmdb, wholeShow []string
	for _, link := range links.tmdb {
		id := link.kind + "/" + link.id
		switch {
		case !query.TV && link.kind == "movie", query.TV && link.kind == "tv" && link.season == season:
			if !slices.Contains(tmdb, id) {
				tmdb = append(tmdb, id)
			}
		case query.TV && link.kind == "tv" && season == 1 && link.season == -1:
			if !slices.Contains(wholeShow, id) {
				wholeShow = append(wholeShow, id)
			}
		}
	}
	if len(tmdb) == 0 {
		tmdb = wholeShow
	}
	if len(tmdb) == 1 {
		item.TMDBID = tmdb[0]
	}
	return item
}

func simplifiedChinese(lang string) bool { return lang == "zh-cn" || lang == "zh-hans" || lang == "zh" }

func hasHan(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// englishGenres 是 NeoDB 从 TMDB、IMDb 带来的英文类型名对应的豆瓣类型名。
var englishGenres = map[string]string{
	"action": "动作", "adventure": "冒险", "animation": "动画", "biography": "传记", "comedy": "喜剧", "crime": "犯罪",
	"documentary": "纪录片", "drama": "剧情", "family": "家庭", "fantasy": "奇幻", "history": "历史", "horror": "恐怖",
	"music": "音乐", "musical": "歌舞", "mystery": "悬疑", "romance": "爱情", "science fiction": "科幻", "sci-fi": "科幻",
	"thriller": "惊悚", "war": "战争", "western": "西部", "sport": "运动", "short": "短片", "reality": "真人秀", "talk": "脱口秀", "disaster": "灾难",
}

// doubanGenre 把 NeoDB 的类型名换成豆瓣的：中文的原样用，英文的查表，认不出的返回空。
func doubanGenre(genre string) string {
	genre = strings.TrimSpace(genre)
	if hasHan(genre) {
		return genre
	}
	return englishGenres[strings.ToLower(genre)]
}

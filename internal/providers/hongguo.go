package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Hongguo 是红果短剧官网（hongguoduanju.com）的客户端，短剧资料的唯一来源。
//
// 只读官网公开给搜索引擎的页面：站点地图、详情页、榜单、分类，robots.txt 都允许抓。
// 这些页面是服务端渲染的，数据就在页面里的 _ROUTER_DATA 这段 JSON 里，按 JSON 读，不解析 HTML。
// 不碰站内搜索和播放相关的页面和接口：搜索的结果要经过字节的风控签名，播放不是元数据。
type Hongguo struct {
	HTTP *http.Client
	Base string // 默认 https://hongguoduanju.com
	Pace *Pacer
}

// NewHongguo 创建红果客户端，默认每秒最多一次请求。
func NewHongguo(client *http.Client) *Hongguo {
	pace := NewPacer(time.Second)
	pace.Name = "红果"
	return &Hongguo{HTTP: client, Base: "https://hongguoduanju.com", Pace: pace}
}

// HongguoForms 是剧种和它在官网分类页、榜单页地址里的写法。
var HongguoForms = []struct{ Name, Slug string }{
	{"真人剧", "real"}, {"漫剧", "comic"}, {"AI剧", "ai"},
}

// HongguoSeries 是一部短剧的资料。它也是 cmd/hongguo-seed 快照文件里每一行的内容，所以带 JSON 标签。
type HongguoSeries struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Intro    string    `json:"intro,omitempty"`
	Cover    string    `json:"cover,omitempty"`
	Tags     []string  `json:"tags,omitempty"`
	Actors   []string  `json:"actors,omitempty"`
	Episodes int       `json:"episodes"`           // 已经上线的集数
	Total    int       `json:"total,omitempty"`    // 总集数；连载中的可能还不知道
	Ended    bool      `json:"ended"`              // 已完结
	OnlineAt time.Time `json:"online_at"`          // 上架时间
	GroupID  string    `json:"group_id,omitempty"` // 同一部剧的各季共用这个 ID
	Rating   float64   `json:"rating,omitempty"`   // 10 分制，评分的人少时没有
	Heat     int64     `json:"heat,omitempty"`
}

// HongguoCard 是榜单或分类页上的一部短剧，只取后面用得到的：ID 和剧种。资料以详情页为准。
type HongguoCard struct {
	ID    string
	Title string
	Form  string // 剧种，不知道时为空
}

// ErrNotModified 表示站点地图和上次读的时候相比没有变化（HTTP 304）。
var ErrNotModified = errors.New("内容没有变化")

// Series 读取一部短剧的详情。下架或不存在时返回 ErrNotFound。
func (h *Hongguo) Series(ctx context.Context, id string) (*HongguoSeries, error) {
	body, _, err := h.get(ctx, "/detail?series_id="+id, "")
	if err != nil {
		return nil, err
	}
	var page struct {
		SeriesDetail *hongguoDetail `json:"seriesDetail"`
		Social       hongguoSocial  `json:"seriesSocialInfo"`
	}
	if err := routerData(body, "detail_page", &page); err != nil {
		return nil, err
	}
	detail := page.SeriesDetail
	if detail == nil || detail.SeriesID != id || strings.TrimSpace(detail.SeriesName) == "" {
		return nil, ErrNotFound
	}
	series := &HongguoSeries{
		ID: detail.SeriesID, Title: strings.TrimSpace(detail.SeriesName), Intro: strings.TrimSpace(detail.SeriesIntro),
		Cover: httpsOnly(detail.SeriesCover), Tags: cleanTags(detail.Tags), Actors: celebrityNames(detail.Celebrities),
		Episodes: int(detail.EpisodeInfo.Count.Int()), Total: int(detail.EpisodeInfo.Total.Int()),
		GroupID: string(detail.Season.GroupID), Rating: page.Social.Rating.Float(), Heat: page.Social.Hot.Score.Int(),
	}
	if series.Episodes == 0 {
		series.Episodes = int(detail.EpisodeCount.Int())
	}
	if seconds := detail.FirstVisible.Int(); seconds > 0 {
		series.OnlineAt = time.Unix(seconds, 0).UTC()
	}
	series.Ended = ended(detail.EpisodeText, series.Episodes, series.Total)
	return series, nil
}

// ended 判断是否已完结。页面上的"全 80 集""更新至 30 集"最可靠，没有时按集数判断。
func ended(text string, episodes, total int) bool {
	text = strings.ReplaceAll(text, " ", "")
	switch {
	case strings.HasPrefix(text, "全"):
		return true
	case strings.Contains(text, "更新"):
		return false
	}
	return total > 0 && episodes >= total
}

// Rank 读取一页榜单。kind 是榜单页地址里的名字，如 "hot-drama"（热播榜）、"new-drama"（新剧榜）、"hot-comic-drama"。
// 还没上线、只能预约的剧不要。
func (h *Hongguo) Rank(ctx context.Context, kind string, page int) ([]HongguoCard, error) {
	path := "/rank/" + kind
	if page > 1 {
		path += "?page=" + strconv.Itoa(page)
	}
	body, _, err := h.get(ctx, path, "")
	if err != nil {
		return nil, err
	}
	var data struct {
		Content struct {
			RankList []struct {
				SeriesID        string   `json:"seriesId"`
				Title           string   `json:"title"`
				Tags            []string `json:"tags"`
				ReservationOnly bool     `json:"reservationOnly"`
			} `json:"rankList"`
		} `json:"content"`
	}
	if err := routerData(body, "rank_"+kind+"/page", &data); err != nil {
		return nil, err
	}
	cards := []HongguoCard{}
	for _, item := range data.Content.RankList {
		if item.ReservationOnly || item.SeriesID == "" {
			continue
		}
		card := HongguoCard{ID: item.SeriesID, Title: item.Title}
		for _, tag := range item.Tags {
			if isForm(tag) {
				card.Form = tag
			}
		}
		cards = append(cards, card)
	}
	return cards, nil
}

// Category 读取一页分类（只按剧种，不加其他筛选条件：带筛选条件的分类页 robots.txt 不让抓）。
// 每个剧种官网最多给 800 部，分 34 页。返回这一页的剧和总页数。
func (h *Hongguo) Category(ctx context.Context, form string, page int) ([]HongguoCard, int, error) {
	slug := ""
	for _, item := range HongguoForms {
		if item.Name == form {
			slug = item.Slug
		}
	}
	if slug == "" {
		return nil, 0, fmt.Errorf("未知的剧种: %q", form)
	}
	path := "/category/" + slug + "-drama"
	if page > 1 {
		path += "?page=" + strconv.Itoa(page)
	}
	body, _, err := h.get(ctx, path, "")
	if err != nil {
		return nil, 0, err
	}
	var data struct {
		RecommendList []struct {
			SeriesID   string `json:"series_id"`
			SeriesName string `json:"series_name"`
		} `json:"recommendList"`
		Pagination struct {
			TotalPages int `json:"totalPages"`
		} `json:"pagination"`
	}
	if err := routerData(body, "category_$", &data); err != nil {
		return nil, 0, err
	}
	cards := []HongguoCard{}
	for _, item := range data.RecommendList {
		if item.SeriesID != "" {
			cards = append(cards, HongguoCard{ID: item.SeriesID, Title: item.SeriesName, Form: form})
		}
	}
	return cards, data.Pagination.TotalPages, nil
}

// Sitemaps 返回站点地图索引里列出的各个子站点地图的地址。
func (h *Hongguo) Sitemaps(ctx context.Context) ([]string, error) {
	body, _, err := h.get(ctx, "/sitemap/hongguoduanju/index.xml", "")
	if err != nil {
		return nil, err
	}
	urls := []string{}
	for _, match := range sitemapLoc.FindAllSubmatch(body, -1) {
		if loc := string(match[1]); strings.HasPrefix(loc, h.Base+"/") {
			urls = append(urls, strings.TrimPrefix(loc, h.Base))
		}
	}
	return urls, nil
}

// ImportantSitemap 是官网列出的重点剧目（约 2000 部热门短剧）。
const ImportantSitemap = "/seo-important.xml"

// SitemapIDs 读取一个站点地图（path 是站内路径，比如 ImportantSitemap 或 Sitemaps 返回的地址），
// 返回里面详情页的 series_id（去重，保持顺序）和这次的 ETag。
// etag 是上次读到的 ETag，没有变化时返回 ErrNotModified，不用重新下载。
func (h *Hongguo) SitemapIDs(ctx context.Context, path, etag string) ([]string, string, error) {
	body, newETag, err := h.get(ctx, path, etag)
	if err != nil {
		return nil, "", err
	}
	seen := map[string]bool{}
	ids := []string{}
	for _, match := range sitemapDetail.FindAllSubmatch(body, -1) {
		id := string(match[1])
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, newETag, nil
}

var (
	sitemapLoc    = regexp.MustCompile(`<loc>\s*([^<\s]+)\s*</loc>`)
	sitemapDetail = regexp.MustCompile(`/detail\?series_id=([1-9][0-9]{18})\b`)
)

const hongguoUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"

// get 请求一个站内页面。etag 非空时带上 If-None-Match。
func (h *Hongguo) get(ctx context.Context, path, etag string) ([]byte, string, error) {
	if h.Pace != nil {
		if err := h.Pace.Wait(ctx); err != nil {
			return nil, "", err
		}
	}
	body, newETag, err := h.fetch(ctx, path, etag)
	if h.Pace != nil {
		switch {
		case errors.Is(err, ErrThrottled):
			h.Pace.Throttled()
		case err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotModified):
			h.Pace.Succeeded()
		}
	}
	return body, newETag, err
}

func (h *Hongguo) fetch(ctx context.Context, path, etag string) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, h.Base+path, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("User-Agent", hongguoUA)
	request.Header.Set("Accept", "text/html,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := h.HTTP.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("请求红果: %w", err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return nil, etag, ErrNotModified
	case http.StatusNotFound, http.StatusGone:
		return nil, "", ErrNotFound
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusTeapot:
		return nil, "", fmt.Errorf("红果返回 HTTP %d: %w", response.StatusCode, ErrThrottled)
	default:
		return nil, "", fmt.Errorf("红果返回 HTTP %d", response.StatusCode)
	}
	// 子站点地图解压后有 5 MB 左右，详情页 500 KB 左右。
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, "", fmt.Errorf("读取红果响应: %w", err)
	}
	return body, response.Header.Get("ETag"), nil
}

var routerMarker = []byte("_ROUTER_DATA = ")

// routerData 从页面里取出 _ROUTER_DATA.loaderData[route]，解析到 destination。
// 页面里没有这段数据，多半是被风控拦下、换成了验证页：按被限流处理，放慢再试。
func routerData(body []byte, route string, destination any) error {
	start := bytes.Index(body, routerMarker)
	if start < 0 {
		return fmt.Errorf("红果返回的页面里没有数据，可能是人机验证页: %w", ErrThrottled)
	}
	var data struct {
		LoaderData map[string]json.RawMessage `json:"loaderData"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body[start+len(routerMarker):]))
	if err := decoder.Decode(&data); err != nil {
		return fmt.Errorf("解析红果页面数据: %w", err)
	}
	raw, ok := data.LoaderData[route]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ErrNotFound
	}
	if err := json.Unmarshal(raw, destination); err != nil {
		return fmt.Errorf("解析红果页面数据: %w", err)
	}
	return nil
}

type hongguoDetail struct {
	SeriesID     string            `json:"series_id"`
	SeriesName   string            `json:"series_name"`
	SeriesIntro  string            `json:"series_intro"`
	SeriesCover  string            `json:"series_cover"`
	Tags         []string          `json:"tags"`
	Celebrities  []json.RawMessage `json:"celebrities"`
	EpisodeCount flexNumber        `json:"episode_cnt"`
	EpisodeText  string            `json:"episode_right_text"`
	FirstVisible flexNumber        `json:"first_visible_time"`
	EpisodeInfo  struct {
		Count flexNumber `json:"episode_cnt"`
		Total flexNumber `json:"episode_total_cnt"`
	} `json:"series_episode_info"`
	Season struct {
		GroupID flexString `json:"series_group_id"`
	} `json:"season_info"`
}

type hongguoSocial struct {
	Rating flexNumber `json:"rating"`
	Hot    struct {
		Score flexNumber `json:"score"`
	} `json:"hot_score_data"`
}

// flexNumber 是可能写成数字、也可能写成字符串的数（红果的时间戳、点赞数都是字符串）。
type flexNumber string

func (n *flexNumber) UnmarshalJSON(data []byte) error {
	*n = flexNumber(strings.Trim(string(data), `"`))
	return nil
}

func (n flexNumber) Int() int64 {
	value, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil {
		f, _ := strconv.ParseFloat(string(n), 64)
		return int64(f)
	}
	return value
}

func (n flexNumber) Float() float64 {
	value, _ := strconv.ParseFloat(string(n), 64)
	return value
}

// flexString 是可能写成字符串、也可能写成数字的 ID。
type flexString string

func (s *flexString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	*s = flexString(strings.Trim(string(data), `"`))
	return nil
}

func isForm(tag string) bool {
	for _, item := range HongguoForms {
		if item.Name == tag {
			return true
		}
	}
	return false
}

// cleanTags 去掉标签里的剧种和角色名（榜单上的"赵乾志 · 陈芸"），去重。
func cleanTags(tags []string) []string {
	seen := map[string]bool{}
	list := []string{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] || isForm(tag) || strings.Contains(tag, "·") || len([]rune(tag)) > 10 {
			continue
		}
		seen[tag] = true
		list = append(list, tag)
	}
	return list
}

// celebrityNames 取演员名。官网的演员列表大多是空的，格式也没见过几种：字符串，或者带 name 字段的对象。
func celebrityNames(items []json.RawMessage) []string {
	names := []string{}
	for _, raw := range items {
		var name string
		if json.Unmarshal(raw, &name) != nil {
			var object map[string]any
			if json.Unmarshal(raw, &object) != nil {
				continue
			}
			for _, key := range []string{"name", "celebrity_name", "nickname", "actor_name"} {
				if value, ok := object[key].(string); ok && value != "" {
					name = value
					break
				}
			}
		}
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func httpsOnly(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "http://") {
		raw = "https://" + strings.TrimPrefix(raw, "http://")
	}
	if !strings.HasPrefix(raw, "https://") {
		return ""
	}
	return raw
}

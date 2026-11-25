// Package providers 是各个外部元数据来源的 HTTP 客户端：豆瓣、TMDB、Wikidata、向量模型。
// 这里只负责请求和解析，不碰数据库。服务器不会通过这个包访问任何资源站。
package providers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound 表示来源明确回答"没有这个条目"。
var ErrNotFound = errors.New("来源里没有这个条目")

// 请求豆瓣时扮成普通浏览器：手机版接口（m.douban.com）用 iPhone Safari，网页版（movie.douban.com、www.douban.com）用电脑上的 Chrome。
// 域名和 UA 对不上（手机 UA 去请求电脑网页的接口）本身就是个特征。
const (
	mobileUA       = "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Mobile/15E148 Safari/604.1"
	desktopUA      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	acceptLanguage = "zh-CN,zh;q=0.9,en;q=0.8"
)

// Pacer 保证两次请求之间至少间隔一段时间，避免把对方打挂或触发风控。
//
// 间隔会自适应：对方开始限流（403 / 418 / 429）时间隔翻倍（最多 1 分钟），
// 连续被限流就整体暂停 10 分钟；之后每成功一次，间隔慢慢缩回原来的值。
// 被限流时最该做的是慢下来，而不是换个 IP 接着打。
//
// 间隔还带随机抖动：每次在当前间隔上再多等 0~50%，平均每 40 次请求再歇一会儿（5~15 个基准间隔），
// 像人翻几页停下来看一看。分毫不差的固定间隔一看就是程序。抖动只往上加，两次请求之间不会短于当前间隔。
type Pacer struct {
	// Name 是对方的名字，写在错误信息里（"豆瓣限流，暂停到 15:04"）。不填时是"对方"。
	Name     string
	mu       sync.Mutex
	base     time.Duration
	interval time.Duration
	next     time.Time
	strikes  int       // 连续被限流的次数
	paused   time.Time // 整体暂停到什么时候
}

const (
	maxPaceInterval = time.Minute
	pauseAfter      = 3
	pauseFor        = 10 * time.Minute
)

// ErrThrottled 表示被对方限流了。
var ErrThrottled = errors.New("被限流了")

// ErrRefused 表示对方拒绝了这一个地址（豆瓣对个别条目返回 403），还不能算是在限流我们。
var ErrRefused = errors.New("对方拒绝了这个地址")

// NewPacer 创建 Pacer。
func NewPacer(interval time.Duration) *Pacer { return &Pacer{base: interval, interval: interval} }

// Wait 等到可以发下一次请求。整体暂停期间直接返回 ErrThrottled（不干等：任务会稍后重试，页面请求也不会卡住）。
func (p *Pacer) Wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	if p.paused.After(now) {
		until := p.paused
		p.mu.Unlock()
		name := p.Name
		if name == "" {
			name = "对方"
		}
		return fmt.Errorf("%s限流，暂停到 %s: %w", name, until.Format("15:04"), ErrThrottled)
	}
	wait := p.next.Sub(now)
	if wait < 0 {
		wait = 0
	}
	p.next = now.Add(wait + p.gap())
	p.mu.Unlock()
	if wait == 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

const (
	jitterFraction = 2  // 每次多等 0 ~ 间隔的 1/jitterFraction
	restEvery      = 40 // 平均每多少次请求歇一次
	restMin        = 5  // 歇的时间是基准间隔的 restMin ~ restMax 倍
	restMax        = 15
)

// gap 是这一次请求和下一次请求之间要隔多久。调用时要持有锁。
func (p *Pacer) gap() time.Duration {
	gap := p.interval
	if extra := int64(p.interval) / jitterFraction; extra > 0 {
		gap += time.Duration(mathrand.Int64N(extra + 1))
	}
	// 歇的时间按基准间隔算：被限流放慢到 1 分钟时，不该一歇就是一刻钟。
	if mathrand.IntN(restEvery) == 0 {
		gap += p.base * time.Duration(restMin+mathrand.IntN(restMax-restMin+1))
	}
	return gap
}

// Throttled 记一次被限流：放慢，连续多次就暂停一阵。
func (p *Pacer) Throttled() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strikes++
	p.interval = min(p.interval*2, maxPaceInterval)
	if p.strikes >= pauseAfter {
		p.paused = time.Now().Add(pauseFor)
		p.strikes = 0
	}
}

// Succeeded 记一次成功：间隔每次缩回一点，直到原来的值。
func (p *Pacer) Succeeded() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strikes = 0
	if p.interval > p.base {
		p.interval = max(p.base, p.interval*9/10)
	}
}

// Status 返回当前的间隔和暂停到的时间（没有暂停时为零值）。
func (p *Pacer) Status() (interval time.Duration, pausedUntil time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused.After(time.Now()) {
		pausedUntil = p.paused
	}
	return p.interval, pausedUntil
}

// Douban 是豆瓣客户端。
type Douban struct {
	HTTP       *http.Client
	MobileBase string // 默认 https://m.douban.com
	WebBase    string // 默认 https://movie.douban.com
	FeedBase   string // 默认 https://www.douban.com，用户标记的 RSS
	Pace       *Pacer
	// PagePace 是详情网页（SubjectPage）的节奏。它是详情接口的兜底，豆瓣分接口限流，
	// 所以单独控制：详情接口被限流、Pace 放慢甚至暂停的时候，网页还能按自己的节奏打开。
	PagePace *Pacer

	refusals refusalState
}

// refusalState 按接口类别记着：这一类接口上一次成功之后，豆瓣拒绝了哪些不同的对象（403 或限流）。
//
// 403 不一定是限流：有的条目、不公开的用户主页，豆瓣对谁都返回 403。所以单个对象的 403 只算它自己失败，
// 不能让整条通道停下；但同一类接口接连有几个不同的对象被拒绝、中间一次成功都没有，就是在拒绝我们了。
//
// 以前用"打开一部热门电影试试"来判断，可豆瓣拒绝我们时那部电影有时照样能打开，
// 结果每个 403 都被当成个别条目失败，任务白白用掉重试次数，请求也不放慢。
//
// 按类别分开记，是因为豆瓣分接口限流（限流的错误码叫 subject_ip_rate_limit）：条目详情被拒绝时，
// 用户的标记列表可能还能打开。不分开的话，正在同步的用户每读一页成功就把计数清零，条目的 403 又全成了个别失败。
type refusalState struct {
	mu      sync.Mutex
	refused map[string]map[string]bool // 接口类别 → 被拒绝的对象
}

// refusedLimit 是同一类接口上一次成功之后，有几个不同的对象被拒绝就算限流。
const refusedLimit = 3

// refuse 记一次被拒绝，返回这一类接口是不是已经算限流了。
func (r *refusalState) refuse(class, object string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refused == nil {
		r.refused = map[string]map[string]bool{}
	}
	if r.refused[class] == nil {
		r.refused[class] = map[string]bool{}
	}
	r.refused[class][object] = true
	return len(r.refused[class]) >= refusedLimit
}

// succeed 记一次这一类接口的正常回答（包括"没有这个条目"），清零这一类的计数。
func (r *refusalState) succeed(class string) {
	r.mu.Lock()
	delete(r.refused, class)
	r.mu.Unlock()
}

var (
	subjectPath  = regexp.MustCompile(`/(?:movie|tv)/([0-9]+)$`) // 榜单的 /movie/hot_gaia 不是条目
	interestPath = regexp.MustCompile(`/user/([^/]+)/interests$`)
	feedPath     = regexp.MustCompile(`/feed/people/([^/]+)/interests$`)
	pagePath     = regexp.MustCompile(`^/subject/([0-9]+)$`)
)

// refusalKey 从请求地址里分出接口类别和对象。/movie/123 和 /tv/123 是同一个条目：
// 先试电影接口再试剧集接口时不能算成两个。认不出的接口，整个路径既是类别也是对象。
func refusalKey(endpoint string) (class, object string) {
	path := endpoint
	if parsed, err := url.Parse(endpoint); err == nil {
		path = parsed.Path
	}
	path = strings.TrimSuffix(path, "/")
	if match := subjectPath.FindStringSubmatch(path); match != nil {
		return "subject", match[1]
	}
	if match := interestPath.FindStringSubmatch(path); match != nil {
		return "interests", match[1]
	}
	if match := feedPath.FindStringSubmatch(path); match != nil {
		return "feed", match[1]
	}
	if match := pagePath.FindStringSubmatch(path); match != nil {
		return "page", match[1]
	}
	return path, path
}

// NewDouban 创建豆瓣客户端。接口的基准间隔是 0.8 秒，加上随机抖动和偶尔歇一下，平均大约 1.2 秒一次；
// 详情网页是兜底，基准间隔 3 秒。
func NewDouban(client *http.Client) *Douban {
	pace := NewPacer(800 * time.Millisecond)
	pace.Name = "豆瓣"
	pagePace := NewPacer(3 * time.Second)
	pagePace.Name = "豆瓣网页"
	return &Douban{HTTP: client, MobileBase: "https://m.douban.com", WebBase: "https://movie.douban.com", FeedBase: "https://www.douban.com",
		Pace: pace, PagePace: pagePace}
}

// DoubanSubject 是豆瓣的一个影视条目。
type DoubanSubject struct {
	ID              string
	Kind            string // 详情接口的类型：movie 或 tv
	Title           string
	OriginalTitle   string
	AKA             []string
	Year            int
	Intro           string
	Poster          string
	Genres          []string
	Countries       []string
	Directors       []string
	Actors          []string
	DurationMinutes int
	Rating          float64
	EpisodesCount   int
	IsShow          bool
}

type rexxarSubject struct {
	ID            string   `json:"id"`
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	OriginalTitle string   `json:"original_title"`
	AKA           []string `json:"aka"`
	Year          string   `json:"year"`
	Intro         string   `json:"intro"`
	CoverURL      string   `json:"cover_url"`
	Genres        []string `json:"genres"`
	Countries     []string `json:"countries"`
	Durations     []string `json:"durations"`
	EpisodesCount int      `json:"episodes_count"`
	IsShow        bool     `json:"is_show"`
	Rating        struct {
		Value float64 `json:"value"`
	} `json:"rating"`
	Pic struct {
		Large  string `json:"large"`
		Normal string `json:"normal"`
	} `json:"pic"`
	Directors []struct {
		Name string `json:"name"`
	} `json:"directors"`
	Actors []struct {
		Name string `json:"name"`
	} `json:"actors"`
}

var leadingNumber = regexp.MustCompile(`[0-9]+`)

// Subject 读取一个条目的详情。豆瓣按电影和剧集分了两个接口，依次尝试。
// first 是已经知道的类型（movie 或 tv），先试它对应的接口：猜对了就只发一次请求。
//
// 用电影接口查剧集时，豆瓣会跳转到剧集接口并返回剧集的资料，所以类型以返回内容里的 type 为准，
// 不能以请求的是哪个接口为准。
func (d *Douban) Subject(ctx context.Context, id, first string) (*DoubanSubject, error) {
	var lastErr error
	notFound := 0
	kinds := []string{"movie", "tv"}
	if first == "tv" {
		kinds = []string{"tv", "movie"}
	}
	for _, kind := range kinds {
		var response rexxarSubject
		endpoint := fmt.Sprintf("%s/rexxar/api/v2/%s/%s?ck=&for_mobile=1", d.MobileBase, kind, url.PathEscape(id))
		err := d.getJSON(ctx, endpoint, "https://m.douban.com/", &response)
		if errors.Is(err, ErrNotFound) {
			notFound++
			continue
		}
		if errors.Is(err, ErrThrottled) {
			return nil, err // 被限流了就别再试另一个接口
		}
		if err != nil {
			lastErr = err
			continue
		}
		if response.ID == "" || response.Title == "" {
			notFound++
			continue
		}
		if response.Type == "movie" || response.Type == "tv" {
			kind = response.Type
		}
		subject := &DoubanSubject{
			ID: response.ID, Kind: kind, Title: response.Title, OriginalTitle: response.OriginalTitle, AKA: response.AKA,
			Intro: strings.TrimSpace(response.Intro), Poster: response.Pic.Large, Genres: response.Genres, Countries: response.Countries,
			Rating: response.Rating.Value, EpisodesCount: response.EpisodesCount, IsShow: response.IsShow,
		}
		if subject.Poster == "" {
			subject.Poster = response.CoverURL
		}
		subject.Year, _ = strconv.Atoi(leadingNumber.FindString(response.Year))
		if len(response.Durations) > 0 {
			subject.DurationMinutes, _ = strconv.Atoi(leadingNumber.FindString(response.Durations[0]))
		}
		for _, person := range response.Directors {
			subject.Directors = append(subject.Directors, person.Name)
		}
		for _, person := range response.Actors {
			subject.Actors = append(subject.Actors, person.Name)
		}
		return subject, nil
	}
	if notFound == 2 {
		return nil, ErrNotFound
	}
	return nil, lastErr
}

// DoubanItem 是榜单或联想里的一条结果。
type DoubanItem struct {
	ID     string
	Title  string
	Year   int
	Poster string
	Kind   string // movie 或 tv，可能为空
	Rating float64
}

// Suggest 调用豆瓣的搜索联想接口。
func (d *Douban) Suggest(ctx context.Context, keyword string) ([]DoubanItem, error) {
	var response []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Type  string `json:"type"`
		Year  string `json:"year"`
		Img   string `json:"img"`
	}
	endpoint := d.WebBase + "/j/subject_suggest?q=" + url.QueryEscape(keyword)
	if err := d.get(ctx, doubanCall{endpoint: endpoint, referer: "https://movie.douban.com/", desktop: true}, &response); err != nil {
		return nil, err
	}
	items := make([]DoubanItem, 0, len(response))
	for _, entry := range response {
		// 联想结果里还有影人等其他类型，只保留影视条目。
		if entry.Type != "movie" && entry.Type != "tv" {
			continue
		}
		year, _ := strconv.Atoi(entry.Year)
		items = append(items, DoubanItem{ID: entry.ID, Title: entry.Title, Year: year, Poster: entry.Img, Kind: entry.Type})
	}
	return items, nil
}

// popularEndpoints 是各个榜单对应的接口。第二个返回值表示响应是不是"合集"格式。
func popularEndpoints(kind string) ([]string, bool) {
	const prefix = "/rexxar/api/v2/"
	switch kind {
	case "movie":
		return []string{prefix + "movie/hot_gaia?area=%E5%85%A8%E9%83%A8&sort=recommend&playable=0&loc_id=0&start=0&count=50&for_mobile=1"}, false
	case "tv":
		return []string{
			prefix + "subject_collection/tv_domestic/items?items_only=1&start=0&count=25&for_mobile=1",
			prefix + "subject_collection/tv_american/items?items_only=1&start=0&count=25&for_mobile=1",
		}, true
	case "show":
		return []string{prefix + "subject_collection/tv_variety_show/items?items_only=1&start=0&count=50&for_mobile=1"}, true
	case "anime":
		return []string{prefix + "subject_collection/tv_animation/items?items_only=1&start=0&count=50&for_mobile=1"}, true
	}
	return nil, false
}

// webPopularQueries 是网页版热门榜（movie.douban.com/j/search_subjects）各类型的查询参数。
var webPopularQueries = map[string]string{
	"movie": "type=movie&tag=" + url.QueryEscape("热门"),
	"tv":    "type=tv&tag=" + url.QueryEscape("热门"),
	"show":  "type=tv&tag=" + url.QueryEscape("综艺"),
	"anime": "type=tv&tag=" + url.QueryEscape("日本动画"),
}

// Popular 读取某个类型的热门榜。kind 取 movie / tv / show / anime。
//
// 先问网页版的热门榜，取不到（出错或者一条都没有）再用手机版接口兜底。两边是不同的域名、不同的接口，
// 豆瓣分接口限流，一边被拒绝时另一边常常还能打开。两边都取不到时报错，调用方保留原来的榜单。
func (d *Douban) Popular(ctx context.Context, kind string) ([]DoubanItem, error) {
	query, ok := webPopularQueries[kind]
	if !ok {
		return nil, fmt.Errorf("不支持的榜单类型 %q", kind)
	}
	items, webErr := d.webPopular(ctx, kind, query)
	if webErr == nil && len(items) > 0 {
		return items, nil
	}
	// 整体暂停期间 Pace.Wait 直接返回 ErrThrottled，兜底这一步不会真的发出请求。
	items, err := d.rexxarPopular(ctx, kind)
	if err != nil && webErr != nil {
		return nil, fmt.Errorf("网页版热门榜: %v；手机版热门榜: %w", webErr, err)
	}
	return items, err
}

// webPopular 读网页版热门榜。这个接口不给年份和类型，类型按查询参数算，年份等抓详情时补上。
func (d *Douban) webPopular(ctx context.Context, kind, query string) ([]DoubanItem, error) {
	var response struct {
		Subjects []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Rate  string `json:"rate"`
			Cover string `json:"cover"`
		} `json:"subjects"`
	}
	endpoint := d.WebBase + "/j/search_subjects?" + query + "&page_limit=50&page_start=0"
	if err := d.get(ctx, doubanCall{endpoint: endpoint, referer: "https://movie.douban.com/", desktop: true}, &response); err != nil {
		return nil, err
	}
	itemKind := "tv"
	if kind == "movie" {
		itemKind = "movie"
	}
	items := []DoubanItem{}
	seen := map[string]bool{}
	for _, subject := range response.Subjects {
		if subject.ID == "" || seen[subject.ID] {
			continue
		}
		seen[subject.ID] = true
		rating, _ := strconv.ParseFloat(strings.TrimSpace(subject.Rate), 64)
		items = append(items, DoubanItem{ID: subject.ID, Title: subject.Title, Poster: subject.Cover, Kind: itemKind, Rating: rating})
	}
	return items, nil
}

// rexxarPopular 读手机版接口的热门榜。一个类型对应多个接口时交替合并；部分接口失败仍返回其余结果。
func (d *Douban) rexxarPopular(ctx context.Context, kind string) ([]DoubanItem, error) {
	endpoints, collection := popularEndpoints(kind)
	if endpoints == nil {
		return nil, fmt.Errorf("不支持的榜单类型 %q", kind)
	}
	type entry struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Year  string `json:"year"`
		Type  string `json:"type"`
		Pic   struct {
			Normal string `json:"normal"`
			Large  string `json:"large"`
		} `json:"pic"`
		Rating struct {
			Value float64 `json:"value"`
		} `json:"rating"`
	}
	var lists [][]entry
	var lastErr error
	for _, endpoint := range endpoints {
		var payload struct {
			Collection []entry `json:"subject_collection_items"`
			Items      []entry `json:"items"`
		}
		if err := d.getJSON(ctx, d.MobileBase+endpoint, "https://m.douban.com/", &payload); err != nil {
			lastErr = err
			continue
		}
		if collection {
			lists = append(lists, payload.Collection)
		} else {
			lists = append(lists, payload.Items)
		}
	}
	items := []DoubanItem{}
	seen := map[string]bool{}
	for index := 0; ; index++ {
		added := false
		for _, list := range lists {
			if index >= len(list) {
				continue
			}
			added = true
			value := list[index]
			if value.ID == "" || seen[value.ID] {
				continue
			}
			seen[value.ID] = true
			poster := value.Pic.Large
			if poster == "" {
				poster = value.Pic.Normal
			}
			year, _ := strconv.Atoi(value.Year)
			itemKind := value.Type
			if itemKind != "movie" && itemKind != "tv" {
				itemKind = ""
			}
			items = append(items, DoubanItem{ID: value.ID, Title: value.Title, Year: year, Poster: poster, Kind: itemKind, Rating: value.Rating.Value})
		}
		if !added {
			break
		}
	}
	if len(items) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return items, nil
}

// DoubanInterest 是用户在豆瓣上的一条标记。
type DoubanInterest struct {
	Item      DoubanItem
	Rating    int // 1–5，没有评分为 0
	Comment   string
	CreatedAt time.Time
}

// Interests 读取某个豆瓣用户的标记。status 取 mark（想看）或 done（看过）。返回这一页的标记和总数。
func (d *Douban) Interests(ctx context.Context, userID, status string, start, count int) ([]DoubanInterest, int, error) {
	var response struct {
		Total     int `json:"total"`
		Interests []struct {
			Comment    string `json:"comment"`
			CreateTime string `json:"create_time"`
			Rating     *struct {
				Value float64 `json:"value"`
			} `json:"rating"`
			Subject struct {
				ID    json.Number `json:"id"`
				Title string      `json:"title"`
				Type  string      `json:"type"`
				Year  string      `json:"year"`
				Cover string      `json:"cover_url"`
				Pic   struct {
					Large string `json:"large"`
				} `json:"pic"`
				Rating *struct {
					Value float64 `json:"value"`
				} `json:"rating"`
			} `json:"subject"`
		} `json:"interests"`
	}
	endpoint := fmt.Sprintf("%s/rexxar/api/v2/user/%s/interests?type=movie&status=%s&start=%d&count=%d&ck=&for_mobile=1",
		d.MobileBase, url.PathEscape(userID), url.QueryEscape(status), start, count)
	if err := d.getJSON(ctx, endpoint, "https://m.douban.com/", &response); err != nil {
		return nil, 0, err
	}
	interests := make([]DoubanInterest, 0, len(response.Interests))
	for _, entry := range response.Interests {
		poster := entry.Subject.Pic.Large
		if poster == "" {
			poster = entry.Subject.Cover
		}
		year, _ := strconv.Atoi(leadingNumber.FindString(entry.Subject.Year))
		interest := DoubanInterest{
			Item:    DoubanItem{ID: entry.Subject.ID.String(), Title: entry.Subject.Title, Year: year, Poster: poster, Kind: entry.Subject.Type},
			Comment: strings.TrimSpace(entry.Comment),
		}
		if entry.Subject.Rating != nil {
			interest.Item.Rating = entry.Subject.Rating.Value // 豆瓣的评分，不是用户自己打的分
		}
		if entry.Rating != nil {
			interest.Rating = int(entry.Rating.Value + 0.5)
		}
		interest.CreatedAt, _ = time.ParseInLocation("2006-01-02 15:04:05", entry.CreateTime, time.Local)
		interests = append(interests, interest)
	}
	return interests, response.Total, nil
}

// DoubanRecent 是用户 RSS 里的一条最近动态。
type DoubanRecent struct {
	ID     string    // 影视条目的豆瓣 ID；书、音乐等其他动态为空
	Status string    // done（看过）或 mark（想看）；在看、书、音乐等其他动态为空
	At     time.Time // 动态的时间；解析不出来时为零值
}

// rssSubjectPath 从 RSS 的链接里认出影视条目：https://movie.douban.com/subject/1292052/
var rssSubjectPath = regexp.MustCompile(`^/subject/([0-9]+)/?$`)

// RecentInterests 读取用户标记的 RSS（www.douban.com/feed/people/<id>/interests），按时间倒序，只有最近的十来条。
// 它在网页版域名下，和手机版的标记列表不是同一个接口：每天的增量同步先看它，没有新动态就不用再去翻标记列表。
func (d *Douban) RecentInterests(ctx context.Context, userID string) ([]DoubanRecent, error) {
	var feed struct {
		Channel struct {
			Items []struct {
				Title   string `xml:"title"`
				Link    string `xml:"link"`
				PubDate string `xml:"pubDate"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	endpoint := d.FeedBase + "/feed/people/" + url.PathEscape(userID) + "/interests"
	if err := d.get(ctx, doubanCall{endpoint: endpoint, referer: "https://www.douban.com/people/" + url.PathEscape(userID) + "/", desktop: true, xml: true}, &feed); err != nil {
		return nil, err
	}
	recent := make([]DoubanRecent, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		entry := DoubanRecent{At: parseFeedTime(item.PubDate)}
		if link, err := url.Parse(strings.TrimSpace(item.Link)); err == nil && link.Host == "movie.douban.com" {
			if match := rssSubjectPath.FindStringSubmatch(link.Path); match != nil {
				entry.ID = match[1]
			}
		}
		title := strings.TrimSpace(item.Title)
		switch {
		case entry.ID == "":
		case strings.HasPrefix(title, "看过"):
			entry.Status = "done"
		case strings.HasPrefix(title, "想看"):
			entry.Status = "mark"
		}
		recent = append(recent, entry)
	}
	return recent, nil
}

func parseFeedTime(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC1123Z, time.RFC1123} {
		if at, err := time.Parse(layout, value); err == nil {
			return at
		}
	}
	return time.Time{}
}

// doubanCall 是一次豆瓣请求。desktop 表示网页版的接口（movie.douban.com、www.douban.com），请求头扮成电脑上的 Chrome；
// 否则是手机版接口，扮成 iPhone Safari。xml 表示响应是 RSS，否则是 JSON。
// page 表示要的是网页本身（详情网页兜底），destination 是 *[]byte，走 PagePace。
type doubanCall struct {
	endpoint, referer  string
	desktop, xml, page bool
}

func (d *Douban) getJSON(ctx context.Context, endpoint, referer string, destination any) error {
	return d.get(ctx, doubanCall{endpoint: endpoint, referer: referer}, destination)
}

// get 是所有豆瓣请求的出口：等节奏放行、发请求、按结果调节奏。
func (d *Douban) get(ctx context.Context, call doubanCall, destination any) error {
	pace := d.Pace
	if call.page {
		pace = d.PagePace
	}
	if pace != nil {
		if err := pace.Wait(ctx); err != nil {
			return err
		}
	}
	request, err := d.newRequest(ctx, call)
	if err != nil {
		return err
	}
	err = d.do(request, call, destination)
	class, object := refusalKey(call.endpoint)
	if err == nil || errors.Is(err, ErrNotFound) {
		d.refusals.succeed(class)
		if pace != nil {
			pace.Succeeded()
		}
		return err
	}
	if !errors.Is(err, ErrThrottled) {
		return err // 网络错误、解析失败这些和限流无关，也不算成功，不改节奏
	}
	tooMany := d.refusals.refuse(class, object)
	var status *throttledStatus
	if errors.As(err, &status) && status.code == http.StatusForbidden && !tooMany {
		return fmt.Errorf("豆瓣对这个地址返回 HTTP 403，暂时只算它自己失败（同类接口上一次成功之后接连 %d 个不同对象被拒绝才算限流）: %w", refusedLimit, ErrRefused)
	}
	// 连着代理时，旧连接可能一直走着被拒绝的那个出口；断开它，下一次请求重新连，代理有机会换一个。
	d.HTTP.CloseIdleConnections()
	if pace != nil {
		pace.Throttled()
	}
	if status != nil && status.code == http.StatusForbidden {
		return fmt.Errorf("豆瓣接连拒绝了 %d 个不同的对象（HTTP 403）: %w", refusedLimit, ErrThrottled)
	}
	return err
}

func (d *Douban) newRequest(ctx context.Context, call doubanCall) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, call.endpoint, nil)
	if err != nil {
		return nil, err
	}
	switch {
	case call.page:
		request.Header.Set("User-Agent", desktopUA)
		request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
		request.Header.Set("Upgrade-Insecure-Requests", "1")
	case call.xml:
		request.Header.Set("User-Agent", desktopUA)
		request.Header.Set("Accept", "application/rss+xml,application/xml;q=0.9,*/*;q=0.8")
	case call.desktop:
		// 网页上的 /j/ 接口是页面里的脚本发的 Ajax 请求。
		request.Header.Set("User-Agent", desktopUA)
		request.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
		request.Header.Set("X-Requested-With", "XMLHttpRequest")
	default:
		request.Header.Set("User-Agent", mobileUA)
		request.Header.Set("Accept", "application/json")
	}
	request.Header.Set("Referer", call.referer)
	request.Header.Set("Accept-Language", acceptLanguage)
	// bid 是豆瓣给每个浏览器发的标识，ll 是选过的城市（108288 是北京），真实的浏览器两个都有。
	request.Header.Set("Cookie", `ll="108288"; bid=`+randomBid())
	return request, nil
}

// bidAlphabet 是豆瓣 bid 用的字符：大小写字母和数字，11 位，比如 KbVLyVSe9PI。
const bidAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomBid 每次生成一个新的 bid。固定值容易被风控；只用十六进制字符的 bid 一眼就能认出来不是豆瓣发的。
func randomBid() string {
	buffer := make([]byte, 11)
	rand.Read(buffer)
	for index, value := range buffer {
		buffer[index] = bidAlphabet[int(value)%len(bidAlphabet)]
	}
	return string(buffer)
}

// errVerification 表示请求被重定向到了豆瓣的验证页。
var errVerification = errors.New("被重定向到豆瓣的验证页")

// isVerificationPage 认出豆瓣拦人时跳过去的页面：sec.douban.com 的滑块验证，以及 /misc/sorry 的"检测到有异常请求"。
func isVerificationPage(target *url.URL) bool {
	return target.Hostname() == "sec.douban.com" || strings.HasPrefix(target.Path, "/misc/sorry")
}

// do 发请求并解析响应。被拦下的几种样子都按限流算：
// 重定向到验证页（Go 默认会跟着跳，跳过去就是一个 200 的网页），或者本该是数据的地方返回了网页。
// 以前这些只会报"解析失败"，不算限流，请求也不放慢。
func (d *Douban) do(request *http.Request, call doubanCall, destination any) error {
	client := *d.HTTP
	next := client.CheckRedirect
	client.CheckRedirect = func(redirect *http.Request, via []*http.Request) error {
		if isVerificationPage(redirect.URL) {
			return errVerification
		}
		if next != nil {
			return next(redirect, via)
		}
		if len(via) >= 10 {
			return errors.New("重定向次数太多")
		}
		return nil
	}
	response, err := client.Do(request)
	if errors.Is(err, errVerification) {
		return fmt.Errorf("豆瓣把请求转到了验证页: %w", ErrThrottled)
	}
	if err != nil {
		return fmt.Errorf("请求豆瓣: %w", err)
	}
	defer response.Body.Close()
	if err := statusError(response, "豆瓣"); err != nil {
		return err
	}
	if call.page {
		page, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		if err != nil {
			return fmt.Errorf("读取豆瓣网页: %w", err)
		}
		if pageBlocked(page) {
			return fmt.Errorf("豆瓣返回的不是详情网页（多半是验证页）: %w", ErrThrottled)
		}
		*destination.(*[]byte) = page
		return nil
	}
	body := bufio.NewReader(io.LimitReader(response.Body, 32<<20))
	head, _ := body.Peek(512)
	if looksLikeWebPage(head, call.xml) {
		return fmt.Errorf("豆瓣返回了网页而不是数据（多半是验证页）: %w", ErrThrottled)
	}
	if call.xml {
		if err := xml.NewDecoder(body).Decode(destination); err != nil {
			return fmt.Errorf("解析豆瓣响应: %w", err)
		}
		return nil
	}
	decoder := json.NewDecoder(body)
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("解析豆瓣响应: %w", err)
	}
	return nil
}

// looksLikeWebPage 看响应开头是不是一个 HTML 网页。JSON 不会以 < 开头；RSS 是 XML，但不会出现 <html。
func looksLikeWebPage(head []byte, isXML bool) bool {
	head = bytes.ToLower(bytes.TrimSpace(head))
	if !isXML {
		return bytes.HasPrefix(head, []byte("<"))
	}
	return bytes.Contains(head, []byte("<html")) || bytes.Contains(head, []byte("<!doctype html"))
}

// throttledStatus 是表示限流的 HTTP 状态码（429、403、418）。豆瓣还要再分辨 403 是不是真的限流。
type throttledStatus struct {
	name string
	code int
}

func (e *throttledStatus) Error() string {
	return fmt.Sprintf("%s返回 HTTP %d: %v", e.name, e.code, ErrThrottled)
}

func (e *throttledStatus) Unwrap() error { return ErrThrottled }

// doJSON 发请求并解析 JSON。404 统一转成 ErrNotFound。
func doJSON(client *http.Client, request *http.Request, name string, destination any) error {
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("请求%s: %w", name, err)
	}
	defer response.Body.Close()
	if err := statusError(response, name); err != nil {
		return err
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 32<<20))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("解析%s响应: %w", name, err)
	}
	return nil
}

// statusError 把不是 200 的响应转成错误：404 是 ErrNotFound，429 / 403 / 418 是限流。
func statusError(response *http.Response, name string) error {
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTeapot {
		return &throttledStatus{name: name, code: response.StatusCode}
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 300))
		// 豆瓣限流有时不用 429，而是返回 400，内容里写着 subject_ip_rate_limit 之类。
		if strings.Contains(string(body), "rate_limit") {
			return fmt.Errorf("%s返回 HTTP %d（限流）: %w", name, response.StatusCode, ErrThrottled)
		}
		return fmt.Errorf("%s返回 HTTP %d: %s", name, response.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

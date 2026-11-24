// Package providers 是各个外部元数据来源的 HTTP 客户端：豆瓣、TMDB、Wikidata、向量模型。
// 这里只负责请求和解析，不碰数据库。服务器不会通过这个包访问任何资源站。
package providers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const mobileUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Mobile/15E148 Safari/604.1"

// Pacer 保证两次请求之间至少间隔一段时间，避免把对方打挂或触发风控。
//
// 间隔会自适应：对方开始限流（403 / 418 / 429）时间隔翻倍（最多 1 分钟），
// 连续被限流就整体暂停 10 分钟；之后每成功一次，间隔慢慢缩回原来的值。
// 被限流时最该做的是慢下来，而不是换个 IP 接着打。
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
	p.next = now.Add(wait + p.interval)
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
	Pace       *Pacer

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
	return path, path
}

// NewDouban 创建豆瓣客户端，默认每秒最多一次请求。
func NewDouban(client *http.Client) *Douban {
	pace := NewPacer(time.Second)
	pace.Name = "豆瓣"
	return &Douban{HTTP: client, MobileBase: "https://m.douban.com", WebBase: "https://movie.douban.com", Pace: pace}
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
	if err := d.getJSON(ctx, endpoint, "https://movie.douban.com/", &response); err != nil {
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

// Popular 读取某个类型的热门榜。kind 取 movie / tv / show / anime。
// 一个类型对应多个接口时交替合并；部分接口失败仍返回其余结果。
func (d *Douban) Popular(ctx context.Context, kind string) ([]DoubanItem, error) {
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
		if entry.Rating != nil {
			interest.Rating = int(entry.Rating.Value + 0.5)
		}
		interest.CreatedAt, _ = time.ParseInLocation("2006-01-02 15:04:05", entry.CreateTime, time.Local)
		interests = append(interests, interest)
	}
	return interests, response.Total, nil
}

func (d *Douban) getJSON(ctx context.Context, endpoint, referer string, destination any) error {
	if d.Pace != nil {
		if err := d.Pace.Wait(ctx); err != nil {
			return err
		}
	}
	request, err := d.newRequest(ctx, endpoint, referer)
	if err != nil {
		return err
	}
	err = doJSON(d.HTTP, request, "豆瓣", destination)
	class, object := refusalKey(endpoint)
	if err == nil || errors.Is(err, ErrNotFound) {
		d.refusals.succeed(class)
		if d.Pace != nil {
			d.Pace.Succeeded()
		}
		return err
	}
	if !errors.Is(err, ErrThrottled) {
		return err // 网络错误、解析失败这些和限流无关，也不算成功，不改节奏
	}
	tooMany := d.refusals.refuse(class, object)
	var status *throttledStatus
	if errors.As(err, &status) && status.code == http.StatusForbidden && !tooMany {
		return fmt.Errorf("豆瓣对这个地址返回 HTTP 403，暂时只算它自己失败（同类接口上一次成功之后接连 %d 个不同对象被拒绝才算限流）", refusedLimit)
	}
	// 连着代理时，旧连接可能一直走着被拒绝的那个出口；断开它，下一次请求重新连，代理有机会换一个。
	d.HTTP.CloseIdleConnections()
	if d.Pace != nil {
		d.Pace.Throttled()
	}
	if status != nil && status.code == http.StatusForbidden {
		return fmt.Errorf("豆瓣接连拒绝了 %d 个不同的对象（HTTP 403）: %w", refusedLimit, ErrThrottled)
	}
	return err
}

func (d *Douban) newRequest(ctx context.Context, endpoint, referer string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", mobileUA)
	request.Header.Set("Referer", referer)
	request.Header.Set("Accept", "application/json")
	// 豆瓣用 bid 标识浏览器，固定值容易被风控。
	bid := make([]byte, 6)
	rand.Read(bid)
	request.Header.Set("Cookie", "bid="+hex.EncodeToString(bid)[:11])
	return request, nil
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
	decoder := json.NewDecoder(io.LimitReader(response.Body, 32<<20))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("解析%s响应: %w", name, err)
	}
	return nil
}

// Package providers 是各个外部元数据来源的 HTTP 客户端：豆瓣、TMDB、Wikidata、向量模型。
// 这里只负责请求和解析，不碰数据库。服务器不会通过这个包访问任何资源站。
package providers

import (
	"bufio"
	"bytes"
	"context"
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
// 域名和 UA 对不上（手机 UA 去请求电脑网页的接口）本身就是个特征。这是没配 DOUBAN_MOBILE_UA、DOUBAN_DESKTOP_UA 时用的。
const (
	DefaultMobileUA  = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1"
	DefaultDesktopUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/158.0.0.0 Safari/537.36"
	acceptLanguage   = "zh-CN,zh;q=0.9,en;q=0.8"
)

// Pacer 保证两次请求之间至少间隔一段时间，避免把对方打挂或触发风控。
//
// 间隔会自适应：对方开始限流（403 / 418 / 429）时间隔翻倍（最多 1 分钟），
// 连续被限流就整体暂停 10 分钟；之后每成功一次，间隔慢慢缩回原来的值。
// 对方明说"这个 IP 被标记了"（豆瓣的 subject_ip_rate_limit）时不等连续几次，立刻暂停几个小时，见 Flagged。
// 被限流时最该做的是慢下来，而不是换个 IP 接着打。
//
// 间隔还带随机抖动：每次在当前间隔上再多等 0~50%，平均每 40 次请求再歇一会儿（5~15 个基准间隔），
// 像人翻几页停下来看一看。分毫不差的固定间隔一看就是程序。抖动只往上加，两次请求之间不会短于当前间隔。
type Pacer struct {
	// Name 是对方的名字，写在错误信息里（"豆瓣限流，暂停到 15:04"）。不填时是"对方"。
	Name        string
	mu          sync.Mutex
	base        time.Duration
	interval    time.Duration
	next        time.Time
	strikes     int           // 连续被限流的次数
	paused      time.Time     // 整体暂停到什么时候
	pausedCause error         // 这次暂停的原因（比如连续 need_permission），让后续排队的请求也能认出原因
	flagged     time.Duration // 上一次因为 IP 被标记暂停了多久，成功一次后清零
}

const (
	maxPaceInterval = time.Minute
	pauseAfter      = 3
	pauseFor        = 10 * time.Minute
	// IP 被标记时第一次暂停 2 小时；恢复后的第一个请求又被标记，说明还没解除，下一次翻倍，最多 12 小时。
	// 豆瓣的标记要安静一阵才会解除：以前每 10 分钟去试一次，正好让它一直解不开。
	flaggedPause    = 2 * time.Hour
	maxFlaggedPause = 12 * time.Hour
)

// ErrThrottled 表示被对方限流了。
var ErrThrottled = errors.New("被限流了")

// ErrRefused 表示对方拒绝了这一个地址（豆瓣对个别条目返回 403），还不能算是在限流我们。
var ErrRefused = errors.New("对方拒绝了这个地址")

// ErrPermission 表示豆瓣要求登录后才能访问这个条目（HTTP 403 need_permission，code 1000）。
// 这是条目本身的访问权限要求，不代表请求被限流，也不计入通道熔断。
var ErrPermission = errors.New("豆瓣要求登录后才能访问这个条目")

// PausedError 表示一条通道正在暂停，Until 之前不会再发请求。它算 ErrThrottled。
// Cause 是让通道暂停的那个回答；因连续 need_permission 暂停时，暂停期间直接拒绝的请求也会带上它，避免调用方改打网页兜底。
// 任务队列用 RetryAfter 把任务推迟到恢复的时候，不用每分钟取出来再失败一次。
type PausedError struct {
	Name  string
	Until time.Time
	Cause error
}

func (e *PausedError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%v；%s暂停到 %s", e.Cause, nameOf(e.Name), clockLabel(e.Until))
	}
	return fmt.Sprintf("%s限流，暂停到 %s", nameOf(e.Name), clockLabel(e.Until))
}

// nameOf 是错误信息里一条通道的名字，没起名字时是"对方"。
func nameOf(name string) string {
	if name == "" {
		return "对方"
	}
	return name
}

func (e *PausedError) Unwrap() []error { return []error{ErrThrottled, e.Cause} }

// RetryAfter 是还要暂停多久。
func (e *PausedError) RetryAfter() time.Duration { return max(time.Until(e.Until), 0) }

// clockLabel 把暂停到的时间写成"15:04"。暂停几个小时可能跨过半夜，不是今天时带上日期。
func clockLabel(at time.Time) string {
	now := time.Now()
	if at.Year() == now.Year() && at.YearDay() == now.YearDay() {
		return at.Format("15:04")
	}
	return at.Format("01-02 15:04")
}

// NewPacer 创建 Pacer。
func NewPacer(interval time.Duration) *Pacer { return &Pacer{base: interval, interval: interval} }

// Wait 等到可以发下一次请求。整体暂停期间直接返回 *PausedError（不干等：任务会稍后重试，页面请求也不会卡住）。
// 排着队等的时候通道被暂停了（前面的请求刚被拒绝），醒来后也不再发出去。
//
// 每次调用占一个时段，后面的请求排在它后面。ctx 的期限等不到轮到自己时（搜索页只等几秒）直接返回，不占时段；
// 等到一半 ctx 取消了，后面还没人排上的话把时段还回去，免得一个没发出去的请求把后面的都往后推。
func (p *Pacer) Wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	if p.paused.After(now) {
		err := &PausedError{Name: p.Name, Until: p.paused, Cause: p.pausedCause}
		p.mu.Unlock()
		return err
	}
	wait := max(p.next.Sub(now), 0)
	if deadline, ok := ctx.Deadline(); ok && wait > 0 && now.Add(wait).After(deadline) {
		p.mu.Unlock()
		return fmt.Errorf("%s还要 %s 才轮到，等不到: %w", nameOf(p.Name), wait.Round(100*time.Millisecond), context.DeadlineExceeded)
	}
	slot := now.Add(wait)
	reserved := slot.Add(p.gap())
	p.next = reserved
	p.mu.Unlock()
	if wait == 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		p.mu.Lock()
		if p.next.Equal(reserved) {
			p.next = slot
		}
		p.mu.Unlock()
		return ctx.Err()
	case <-time.After(wait):
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused.After(time.Now()) {
		return &PausedError{Name: p.Name, Until: p.paused, Cause: p.pausedCause}
	}
	return nil
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

// Throttled 记一次被限流：放慢，连续多次就暂停一阵。这一次让通道暂停了时返回暂停到什么时候，否则返回零值。
// 已经在更长的暂停里（IP 被标记）时不会把暂停缩短。
func (p *Pacer) Throttled() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strikes++
	p.interval = min(p.interval*2, maxPaceInterval)
	if p.strikes < pauseAfter {
		return time.Time{}
	}
	p.strikes = 0
	if until := time.Now().Add(pauseFor); until.After(p.paused) {
		p.paused = until
		p.pausedCause = nil
	}
	return p.paused
}

// Pause 直接把通道暂停指定时长并记住暂停原因；已经在更长的暂停里时不缩短。
func (p *Pacer) Pause(duration time.Duration, cause error) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strikes = 0
	p.interval = min(p.interval*2, maxPaceInterval)
	if until := time.Now().Add(duration); until.After(p.paused) {
		p.paused = until
		p.pausedCause = cause
	}
	return p.paused
}

// Flagged 记一次"对方标记了这个 IP"：不等连续几次，立刻暂停几个小时，恢复后也放慢一些。
// 恢复后的第一个请求要是又被标记，暂停的时间翻倍（最多 12 小时）；成功一次后回到 2 小时。
// 同时发出去的几个请求先后被标记回来时，已经在这一轮暂停里，不再翻倍。返回暂停到什么时候。
func (p *Pacer) Flagged() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.flag()
}

// FlaggedExits 记一次"换了几个出口都说 IP 被标记了"（出口会轮换的代理，见 Douban.Rotating）。
// 一个出口被标记不代表别的出口也是，所以先和普通的限流一样放慢、记一次；
// 接连 pauseAfter 次（中间没有成功）说明出口大多被标记了，才和 Flagged 一样停几个小时。
// 停了时返回暂停到什么时候，否则返回零值。
func (p *Pacer) FlaggedExits() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.strikes+1 < pauseAfter {
		p.strikes++
		p.interval = min(p.interval*2, maxPaceInterval)
		return time.Time{}
	}
	return p.flag()
}

// flag 让通道停几个小时，见 Flagged。调用时要持有锁。
func (p *Pacer) flag() time.Time {
	now := time.Now()
	if p.flagged > 0 && p.paused.After(now) {
		return p.paused
	}
	if p.flagged == 0 {
		p.flagged = flaggedPause
	} else {
		p.flagged = min(p.flagged*2, maxFlaggedPause)
	}
	p.strikes = 0
	p.interval = min(p.interval*2, maxPaceInterval)
	p.paused = now.Add(p.flagged)
	p.pausedCause = nil
	return p.paused
}

// Succeeded 记一次成功：间隔每次缩回一点，直到原来的值。
func (p *Pacer) Succeeded() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strikes = 0
	p.flagged = 0
	p.pausedCause = nil
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
	// Pace 是详情以外的接口（标记列表、榜单、搜索联想、RSS）共用的节奏。
	Pace *Pacer
	// MoviePace、TVPace 是电影详情（/rexxar/api/v2/movie/<id>）和剧集详情（/tv/<id>）各自的节奏。
	// 豆瓣分接口限流：电影详情被标记、返回 subject_ip_rate_limit 的时候，同一个 IP 打开剧集详情照样是 200。
	// 所以各自放慢、各自暂停，电影详情停几个小时的时候，剧集详情、标记同步和榜单照常。
	MoviePace *Pacer
	TVPace    *Pacer
	// PagePace 是详情网页（SubjectPage）的节奏。它是详情接口的兜底，豆瓣分接口限流，
	// 所以单独控制：详情接口被限流、放慢甚至暂停的时候，网页还能按自己的节奏打开。
	PagePace *Pacer
	// Rotating 表示请求经过会轮换出口的代理（配了 DOUBAN_PROXY，比如 mihomo 的轮询负载均衡），
	// HTTP 不复用连接，每个请求都可能从另一个 IP 出去。这时一个出口被拒绝、被标记或者连不上，
	// 不代表别的出口也不行：换一个出口再试，最多 exitTries 次，都不行才记限流，见 get。
	Rotating bool
	// Cookie 是每个豆瓣请求都带的 Cookie（DOUBAN_COOKIE）：用浏览器从服务器的出口打开过豆瓣、过了验证后拿到的那一整串，
	// 所有请求共用同一个 bid，像同一个浏览器。为空时不带 Cookie。
	Cookie string
	// MobileUA、DesktopUA 是手机版接口和网页版用的 UA，NewDouban 填默认值。
	MobileUA, DesktopUA string

	refusals    refusalState
	permissions permissionState
}

// exitTries 是出口会轮换时，一个请求最多换几个出口去试。每次都按节奏排队，不会因此请求得更快。
const exitTries = 3

// paceFor 是这次请求走哪条节奏：详情网页、电影详情、剧集详情各走各的，其余接口共用 Pace。
// class 是 refusalKey 分出来的接口类别。
func (d *Douban) paceFor(call doubanCall, class string) *Pacer {
	switch {
	case call.page:
		return d.PagePace
	case class == "movie":
		return d.MoviePace
	case class == "tv":
		return d.TVPace
	}
	return d.Pace
}

// Paused 返回豆瓣的接口还要全部暂停多久：电影详情、剧集详情和其余接口都在暂停时，是最早恢复的那条还要等多久；
// 有一条能用就是 0。任务队列用它决定整条豆瓣通道要不要先别取任务；只停了一部分时照常取，
// 碰上暂停中的接口的任务按 PausedError.RetryAfter 推迟。
func (d *Douban) Paused() time.Duration {
	wait := time.Duration(-1)
	for _, pace := range []*Pacer{d.Pace, d.MoviePace, d.TVPace} {
		if pace == nil {
			return 0
		}
		_, until := pace.Status()
		if until.IsZero() {
			return 0
		}
		if left := time.Until(until); wait < 0 || left < wait {
			wait = left
		}
	}
	return max(wait, 0)
}

// refusalState 按接口类别记着：这一类接口上一次成功之后，豆瓣拒绝了哪些不同的对象（403 或限流）。
//
// 403 不一定是限流：有的条目、不公开的用户主页，豆瓣对谁都返回 403。所以单个对象的 403 只算它自己失败，
// 不能让整条通道停下；但同一类接口接连有几个不同的对象被拒绝、中间一次成功都没有，就是在拒绝我们了。
//
// 以前用"打开一部热门电影试试"来判断，可豆瓣拒绝我们时那部电影有时照样能打开，
// 结果每个 403 都被当成个别条目失败，任务白白用掉重试次数，请求也不放慢。
//
// 按类别分开记，是因为豆瓣分接口限流：条目详情被拒绝时，用户的标记列表可能还能打开，
// 电影详情被拒绝时剧集详情也可能还能打开。不分开的话，正在同步的用户每读一页成功、
// 或者一部剧集的详情打开了，就把计数清零，电影详情的 403 又全成了个别失败。
type refusalState struct {
	mu      sync.Mutex
	refused map[string]map[string]bool // 接口类别 → 被拒绝的对象
}

// refusedLimit 是同一类接口上一次成功之后，有几个不同的对象被拒绝就算限流。
const (
	refusedLimit     = 3
	permissionLimit  = 5
	permissionWindow = 15 * time.Minute
)

// permissionState 在短时间窗口内分别统计电影、剧集等接口收到 need_permission（code 1000）的不同条目，
// 并记住最近一次真正拿到 200 详情的时间。只有 200 成功才清零，404 和跳转不清零。
type permissionState struct {
	mu          sync.Mutex
	denied      map[string]map[string]time.Time
	lastSuccess map[string]time.Time
}

func (p *permissionState) succeed(class string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastSuccess == nil {
		p.lastSuccess = map[string]time.Time{}
	}
	p.lastSuccess[class] = time.Now()
	delete(p.denied, class)
}

func (p *permissionState) deny(class, object string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-permissionWindow)
	if p.denied == nil {
		p.denied = map[string]map[string]time.Time{}
	}
	seen := p.denied[class]
	if seen == nil {
		seen = map[string]time.Time{}
		p.denied[class] = seen
	}
	for id, at := range seen {
		if !at.After(cutoff) {
			delete(seen, id)
		}
	}
	if _, exists := seen[object]; !exists {
		seen[object] = now
	}
	if len(seen) >= permissionLimit {
		// 已经因为连续权限拒绝触发接口级暂停，清掉之前记录的成功时间，等真正再次 200 成功后再恢复确认。
		delete(p.lastSuccess, class)
		return true
	}
	return false
}

func (p *permissionState) lastSuccessAt(class string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSuccess[class]
}

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
	subjectPath  = regexp.MustCompile(`/(movie|tv)/([0-9]+)$`) // 榜单的 /movie/hot_gaia 不是条目
	interestPath = regexp.MustCompile(`/user/([^/]+)/interests$`)
	feedPath     = regexp.MustCompile(`/feed/people/([^/]+)/interests$`)
	pagePath     = regexp.MustCompile(`^/subject/([0-9]+)$`)
)

// refusalKey 从请求地址里分出接口类别和对象。电影详情（/movie/123）和剧集详情（/tv/123）是两类：
// 豆瓣对它们分开限流。真实的豆瓣用剧集接口查电影返回 404，用电影接口查剧集会跳到剧集接口，
// 所以一个条目先试电影接口再试剧集接口，不会在同一类里算成两次。认不出的接口，整个路径既是类别也是对象。
func refusalKey(endpoint string) (class, object string) {
	path := endpoint
	if parsed, err := url.Parse(endpoint); err == nil {
		path = parsed.Path
	}
	path = strings.TrimSuffix(path, "/")
	if match := subjectPath.FindStringSubmatch(path); match != nil {
		return match[1], match[2]
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

// NewDouban 创建豆瓣客户端。四条节奏（电影详情、剧集详情、其余接口、详情网页）各自放慢、各自暂停，
// 基准间隔都是 doubanInterval。
func NewDouban(client *http.Client) *Douban {
	pace := NewPacer(doubanInterval)
	pace.Name = "豆瓣"
	moviePace := NewPacer(doubanInterval)
	moviePace.Name = "豆瓣电影详情"
	tvPace := NewPacer(doubanInterval)
	tvPace.Name = "豆瓣剧集详情"
	pagePace := NewPacer(doubanInterval)
	pagePace.Name = "豆瓣网页"
	return &Douban{HTTP: client, MobileBase: "https://m.douban.com", WebBase: "https://movie.douban.com", FeedBase: "https://www.douban.com",
		Pace: pace, MoviePace: moviePace, TVPace: tvPace, PagePace: pagePace, MobileUA: DefaultMobileUA, DesktopUA: DefaultDesktopUA}
}

// doubanInterval 是每条豆瓣节奏的基准间隔。加上随机抖动和偶尔歇一下，平均大约 7.5 秒一次（每分钟 8 次左右）。
// 以前按 0.8 秒的间隔请求，批量刷新时四分钟请求了将近两百次，豆瓣就标记了这个 IP，电影详情一直返回 subject_ip_rate_limit。
const doubanInterval = 5 * time.Second

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

// SubjectSucceededSince 判断在 since 之后，这个错误对应的那类详情接口（电影或剧集）有没有真正成功取到过详情。
func (d *Douban) SubjectSucceededSince(err error, since time.Time) bool {
	var de *doubanError
	if !errors.As(err, &de) {
		return false
	}
	class, _ := refusalKey(de.path)
	last := d.permissions.lastSuccessAt(class)
	return !last.IsZero() && last.After(since)
}

// Subject 读取一个条目的详情。豆瓣按电影和剧集分了两个接口，依次尝试。
// first 是已经知道的类型（movie 或 tv），先试它对应的接口：猜对了就只发一次请求。
// 知道类型时，这个接口被限流就不再试另一个：另一个多半是 404，白白多一次请求。
// first 为空表示不知道类型（只凭豆瓣 ID 收进来的条目），先试电影接口；电影接口被限流时接着试剧集接口，
// 是剧集的话不用跟着电影接口一起等。
//
// 用电影接口查剧集时，豆瓣会跳转到剧集接口。不跟着跳，改按剧集接口自己的节奏去查，见 movedError。
// 类型以返回内容里的 type 为准，不能以请求的是哪个接口为准。
func (d *Douban) Subject(ctx context.Context, id, first string) (*DoubanSubject, error) {
	var lastErr, throttled, permission error
	notFound := 0
	kinds := []string{"movie", "tv"}
	if first == "tv" {
		kinds = []string{"tv", "movie"}
	}
	for _, kind := range kinds {
		var response rexxarSubject
		endpoint := fmt.Sprintf("%s/rexxar/api/v2/%s/%s?ck=&for_mobile=1", d.MobileBase, kind, url.PathEscape(id))
		// 手机版的条目页里，页面脚本请求这个接口时 Referer 就是条目页本身。
		err := d.getJSON(ctx, endpoint, "https://m.douban.com/movie/subject/"+url.PathEscape(id)+"/", &response)
		var moved *movedError
		if errors.Is(err, ErrNotFound) || errors.As(err, &moved) {
			notFound++ // 跳到了另一个接口，也是这个接口下没有它
			continue
		}
		if errors.Is(err, ErrThrottled) {
			if first != "" {
				return nil, err
			}
			if throttled == nil {
				throttled = err
			}
			continue
		}
		if errors.Is(err, ErrPermission) {
			if first != "" {
				return nil, err
			}
			if permission == nil {
				permission = err
			}
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}
		if response.ID == "" || response.Title == "" {
			notFound++
			continue
		}
		return response.subject(kind), nil
	}
	if throttled != nil {
		return nil, throttled // 另一个接口也没查到，等被限流的那个恢复了再查
	}
	if permission != nil {
		return nil, permission
	}
	if notFound == 2 {
		return nil, ErrNotFound
	}
	return nil, lastErr
}

// movedError 表示一类详情接口跳到了另一类：用电影接口查剧集时，豆瓣 301 到剧集接口。
// 不跟着跳：两类接口各有各的节奏，剧集接口可能正暂停着，跟着跳就绕过了暂停；
// 跳过去被限流了，也该记在剧集接口上，而不是电影接口上。Subject 收到它就按剧集接口自己的节奏去查。
type movedError struct{ kind string }

func (e *movedError) Error() string { return "豆瓣详情跳到了 " + e.kind + " 接口" }

// detailClass 表示这一类接口是电影详情或剧集详情。
func detailClass(class string) bool { return class == "movie" || class == "tv" }

// subject 把详情接口的回答整理成 DoubanSubject。kind 是请求的哪个接口；回答里写了类型时以回答为准。
func (r rexxarSubject) subject(kind string) *DoubanSubject {
	if r.Type == "movie" || r.Type == "tv" {
		kind = r.Type
	}
	subject := &DoubanSubject{
		ID: r.ID, Kind: kind, Title: r.Title, OriginalTitle: r.OriginalTitle, AKA: r.AKA,
		Intro: strings.TrimSpace(r.Intro), Poster: r.Pic.Large, Genres: r.Genres, Countries: r.Countries,
		Rating: r.Rating.Value, EpisodesCount: r.EpisodesCount, IsShow: r.IsShow,
	}
	if subject.Poster == "" {
		subject.Poster = r.CoverURL
	}
	subject.Year, _ = strconv.Atoi(leadingNumber.FindString(r.Year))
	if len(r.Durations) > 0 {
		subject.DurationMinutes, _ = strconv.Atoi(leadingNumber.FindString(r.Durations[0]))
	}
	for _, person := range r.Directors {
		subject.Directors = append(subject.Directors, person.Name)
	}
	for _, person := range r.Actors {
		subject.Actors = append(subject.Actors, person.Name)
	}
	return subject
}

// DecodeSubject 解析详情接口（/rexxar/api/v2/movie/<id> 或 /tv/<id>）的原始回答。
// 浏览器插件在用户的浏览器里请求这个接口，把回答原样交给服务器，解析和服务器自己请求时完全一样。
// 回答里没有 ID 或片名时返回 ErrNotFound。
func DecodeSubject(body []byte, kind string) (*DoubanSubject, error) {
	var response rexxarSubject
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("解析豆瓣详情: %w", err)
	}
	if response.ID == "" || response.Title == "" {
		return nil, ErrNotFound
	}
	return response.subject(kind), nil
}

// ParseSubjectPage 从详情网页（movie.douban.com/subject/<id>/）的 HTML 里取出资料，和 SubjectPage 的解析一样。
// 浏览器插件在用户打开的详情页上摘出资料所在的几块交给服务器。
func ParseSubjectPage(id string, page []byte) (*DoubanSubject, error) {
	if pageBlocked(page) {
		return nil, errors.New("不是豆瓣的详情网页")
	}
	return parseSubjectPage(id, page)
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
	// 两边共用 Pace：网页版让 Pace 暂停了的话，Wait 直接返回 ErrThrottled，兜底这一步不会真的发出请求。
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

// rexxarInterests 是标记列表接口（/rexxar/api/v2/user/<id>/interests）的回答。
type rexxarInterests struct {
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

// Interests 读取某个豆瓣用户的标记。status 取 mark（想看）或 done（看过）。返回这一页的标记和总数。
func (d *Douban) Interests(ctx context.Context, userID, status string, start, count int) ([]DoubanInterest, int, error) {
	var response rexxarInterests
	endpoint := fmt.Sprintf("%s/rexxar/api/v2/user/%s/interests?type=movie&status=%s&start=%d&count=%d&ck=&for_mobile=1",
		d.MobileBase, url.PathEscape(userID), url.QueryEscape(status), start, count)
	if err := d.getJSON(ctx, endpoint, "https://m.douban.com/", &response); err != nil {
		return nil, 0, err
	}
	return response.interests(), response.Total, nil
}

// DecodeInterests 解析标记列表接口的原始回答，返回这一页的标记和总数。
// 浏览器插件用用户自己的登录状态读这个接口（标记不公开的账号也能读），把回答原样交给服务器。
func DecodeInterests(body []byte) ([]DoubanInterest, int, error) {
	var response rexxarInterests
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, 0, fmt.Errorf("解析豆瓣标记: %w", err)
	}
	return response.interests(), response.Total, nil
}

func (response rexxarInterests) interests() []DoubanInterest {
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
	return interests
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
// 让通道暂停了的错误是 *PausedError，带着暂停到什么时候。
//
// 出口会轮换时（Rotating），被拒绝、被标记、连不上都先换个出口再试，最多 exitTries 次。
// 2026-11-26 实测一个机场订阅：同一时刻四个节点里，一个能打开电影详情，一个 403 need_permission，
// 一个 subject_ip_rate_limit，一个连不上。以前第一个出口被标记就整条停 2 小时，好的出口也用不上。
func (d *Douban) get(ctx context.Context, call doubanCall, destination any) error {
	class, object := refusalKey(call.endpoint)
	pace := d.paceFor(call, class)
	var err, answer error // answer 是最近一次豆瓣真的回答了的拒绝，连不上的出口不算
	for try := 0; ; try++ {
		if pace != nil {
			if waitErr := pace.Wait(ctx); waitErr != nil {
				if err == nil {
					return waitErr
				}
				break // 换出口再试时轮不上了（通道刚被别的请求停下，或者等不到）：按上一次的结果算
			}
		}
		request, requestErr := d.newRequest(ctx, call)
		if requestErr != nil {
			return requestErr
		}
		err = d.do(request, call, destination)
		if errors.Is(err, ErrThrottled) || errors.Is(err, ErrPermission) {
			answer = err
		}
		if !d.Rotating || try+1 >= exitTries || !exitFailed(ctx, err) {
			break
		}
	}
	if answer != nil && !errors.Is(err, ErrThrottled) && !errors.Is(err, ErrPermission) && exitFailed(ctx, err) {
		err = answer // 最后一个出口连不上：按前面豆瓣的回答算
	}
	return d.settle(class, object, pace, err)
}

// exitFailed 表示这次失败可能只是这个出口的问题：豆瓣拒绝了（限流、标记、403），或者连不上。
// 404、跳到另一个接口、解析失败这些和出口无关。
func exitFailed(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var transport *url.Error // http.Client 连接、TLS、代理出错都是 *url.Error；跳到验证页、跳到别的接口在前面已经认出来了
	return errors.Is(err, ErrThrottled) || errors.Is(err, ErrPermission) || errors.As(err, &transport)
}

type throttledPermissionError struct {
	message string
	cause   error
}

func (e *throttledPermissionError) Error() string   { return e.message }
func (e *throttledPermissionError) Unwrap() []error { return []error{ErrThrottled, e.cause} }

// settle 按请求的结果调节奏，返回给调用方的错误。让通道暂停了的是 *PausedError。
func (d *Douban) settle(class, object string, pace *Pacer, err error) error {
	var moved *movedError
	if err == nil {
		d.refusals.succeed(class)
		d.permissions.succeed(class)
		if pace != nil {
			pace.Succeeded()
		}
		return nil
	}
	if errors.Is(err, ErrNotFound) || errors.As(err, &moved) { // 跳到另一类接口或 404 不清零 need_permission 计数
		d.refusals.succeed(class)
		if pace != nil {
			pace.Succeeded()
		}
		return err
	}
	if errors.Is(err, ErrPermission) {
		if !d.permissions.deny(class, object) {
			return err
		}
		d.HTTP.CloseIdleConnections()
		wrapped := &throttledPermissionError{
			message: fmt.Sprintf("豆瓣 15 分钟内连续 %d 个不同条目返回需要登录，最近一个: %v", permissionLimit, err),
			cause:   err,
		}
		if pace != nil {
			return &PausedError{Name: pace.Name, Until: pace.Pause(pauseFor, wrapped), Cause: wrapped}
		}
		return wrapped
	}
	if !errors.Is(err, ErrThrottled) {
		return err // 网络错误、解析失败这些和限流无关，也不算成功，不改节奏
	}
	// 连着代理时，旧连接可能一直走着被拒绝的那个出口；断开它，下一次请求重新连，代理有机会换一个。
	d.HTTP.CloseIdleConnections()
	var refusal *doubanError
	errors.As(err, &refusal)
	if refusal != nil && refusal.flagged() {
		// IP 被标记了：再试多少次都一样，这一类接口直接停几个小时。不记进 refusals，那是给分辨不清的 403 用的。
		if pace == nil {
			return err
		}
		if d.Rotating {
			// 出口会轮换：换了几个出口都被标记才记一次，接连几次这样才停几个小时。
			if until := pace.FlaggedExits(); !until.IsZero() {
				return &PausedError{Name: pace.Name, Until: until, Cause: err}
			}
			return err
		}
		return &PausedError{Name: pace.Name, Until: pace.Flagged(), Cause: err}
	}
	tooMany := d.refusals.refuse(class, object)
	forbidden := refusal != nil && refusal.ambiguous()
	if forbidden && !tooMany {
		// 不用 %w 包住 refusal：它算 ErrThrottled，这里只算这一个对象失败。
		return fmt.Errorf("%v，暂时只算它自己失败（同一类接口上一次成功之后接连 %d 个不同对象被拒绝才算限流）: %w", refusal, refusedLimit, ErrRefused)
	}
	if forbidden {
		err = fmt.Errorf("豆瓣接连拒绝了 %d 个不同的对象，最近一个: %w", refusedLimit, err)
	}
	if pace != nil {
		if until := pace.Throttled(); !until.IsZero() {
			return &PausedError{Name: pace.Name, Until: until, Cause: err}
		}
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
		request.Header.Set("User-Agent", d.DesktopUA)
		request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
		request.Header.Set("Upgrade-Insecure-Requests", "1")
	case call.xml:
		request.Header.Set("User-Agent", d.DesktopUA)
		request.Header.Set("Accept", "application/rss+xml,application/xml;q=0.9,*/*;q=0.8")
	case call.desktop:
		// 网页上的 /j/ 接口是页面里的脚本发的 Ajax 请求。
		request.Header.Set("User-Agent", d.DesktopUA)
		request.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
		request.Header.Set("X-Requested-With", "XMLHttpRequest")
	default:
		request.Header.Set("User-Agent", d.MobileUA)
		request.Header.Set("Accept", "application/json")
	}
	request.Header.Set("Referer", call.referer)
	request.Header.Set("Accept-Language", acceptLanguage)
	if d.Cookie != "" {
		request.Header.Set("Cookie", d.Cookie)
	}
	return request, nil
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
	from, _ := refusalKey(call.endpoint)
	client.CheckRedirect = func(redirect *http.Request, via []*http.Request) error {
		if isVerificationPage(redirect.URL) {
			return errVerification
		}
		if to, _ := refusalKey(redirect.URL.String()); detailClass(from) && detailClass(to) && to != from {
			return &movedError{kind: to}
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
		return fmt.Errorf("豆瓣把 %s 转到了验证页: %w", request.URL.Path, ErrThrottled)
	}
	if moved := (*movedError)(nil); errors.As(err, &moved) {
		return moved
	}
	if err != nil {
		return fmt.Errorf("请求豆瓣: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return readDoubanError(response)
	}
	if call.page {
		page, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		if err != nil {
			return fmt.Errorf("读取豆瓣网页: %w", err)
		}
		if pageBlocked(page) {
			return fmt.Errorf("豆瓣 %s 返回的不是详情网页（多半是验证页）: %w", request.URL.Path, ErrThrottled)
		}
		*destination.(*[]byte) = page
		return nil
	}
	body := bufio.NewReader(io.LimitReader(response.Body, 32<<20))
	head, _ := body.Peek(512)
	if looksLikeWebPage(head, call.xml) {
		return fmt.Errorf("豆瓣 %s 返回了网页而不是数据（多半是验证页）: %w", request.URL.Path, ErrThrottled)
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

// doubanError 是豆瓣接口不是 200 也不是 404 的回答。豆瓣的错误回答是一段 JSON，比如
//
//	{"request": "GET /v2/movie/1889152", "msg": "subject_ip_rate_limit", "code": 1309, "localized_message": "您所在的网络存在异常，请登录后重试。"}
//
// msg 是错误名，code 是错误码，localized_message 是给人看的说明。错误信息里带上请求的接口和这几项，
// 一眼就能看出是哪个接口、为什么被拒绝。不是这种 JSON 时（网关自己回的 429 之类）带上回答开头的一段原文。
type doubanError struct {
	path    string // 请求的接口，跟着跳转之后的那个，比如 /rexxar/api/v2/movie/1889152
	status  int
	code    int
	msg     string
	message string
	body    string
}

// readDoubanError 读出豆瓣的错误回答。
func readDoubanError(response *http.Response) *doubanError {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	e := &doubanError{path: response.Request.URL.Path, status: response.StatusCode}
	var answer struct {
		Msg     string `json:"msg"`
		Code    int    `json:"code"`
		Message string `json:"localized_message"`
	}
	if json.Unmarshal(raw, &answer) == nil && (answer.Msg != "" || answer.Code != 0) {
		e.msg, e.code, e.message = answer.Msg, answer.Code, answer.Message
		return e
	}
	body := strings.TrimSpace(string(raw))
	if len(body) > 200 {
		body = strings.ToValidUTF8(body[:200], "") + "…"
	}
	e.body = body
	return e
}

func (e *doubanError) Error() string {
	text := fmt.Sprintf("豆瓣 %s 返回 HTTP %d", e.path, e.status)
	if e.msg != "" {
		text += " " + e.msg
	}
	var detail []string
	if e.code != 0 {
		detail = append(detail, "code "+strconv.Itoa(e.code))
	}
	if e.message != "" {
		detail = append(detail, e.message)
	}
	if len(detail) > 0 {
		text += "（" + strings.Join(detail, "，") + "）"
	}
	if e.body != "" {
		text += ": " + e.body
	}
	return text
}

// flagged 表示豆瓣标记了这个 IP：subject_ip_rate_limit（code 1309），说明是"您所在的网络存在异常，请登录后重试"。
// 它按接口算：电影详情被标记时剧集详情还能打开。标记期间请求头、Cookie 换成什么样都一样被拒绝（2026-11 实测），
// 再试只会让标记一直解不开。
func (e *doubanError) flagged() bool {
	return e.code == 1309 || strings.Contains(e.msg+" "+e.body, "ip_rate_limit")
}

// permissionRequired 表示豆瓣要求登录后才能访问这个条目（HTTP 403 need_permission，code 1000）。
func (e *doubanError) permissionRequired() bool {
	return e.status == http.StatusForbidden && (e.code == 1000 || e.msg == "need_permission")
}

// throttled 表示这个回答是在限流：IP 被标记，429、418、其他 403（还要再分辨是不是个别对象，见 ambiguous），
// 或者错误名里带 rate_limit（豆瓣限流常常用 400）。need_permission 不算限流。
func (e *doubanError) throttled() bool {
	if e.flagged() || e.saysRateLimit() {
		return true
	}
	if e.permissionRequired() {
		return false
	}
	switch e.status {
	case http.StatusTooManyRequests, http.StatusTeapot, http.StatusForbidden:
		return true
	}
	return false
}

// ambiguous 表示分不清是在限流我们，还是这个对象本身就不让看：非 need_permission 的 403，又没说是限流。
// 这种按 refusalState 的办法分辨；明说了 rate_limit 的 403 直接算限流。
func (e *doubanError) ambiguous() bool {
	return e.status == http.StatusForbidden && !e.saysRateLimit() && !e.flagged() && !e.permissionRequired()
}

func (e *doubanError) saysRateLimit() bool { return strings.Contains(e.msg+" "+e.body, "rate_limit") }

func (e *doubanError) Unwrap() error {
	if e.permissionRequired() {
		return ErrPermission
	}
	if e.throttled() {
		return ErrThrottled
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

// statusError 把不是 200 的响应转成错误：404 是 ErrNotFound，429 / 403 / 418 是限流。豆瓣不用它，见 doubanError。
func statusError(response *http.Response, name string) error {
	switch response.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusTooManyRequests, http.StatusForbidden, http.StatusTeapot:
		return fmt.Errorf("%s返回 HTTP %d: %w", name, response.StatusCode, ErrThrottled)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 300))
	return fmt.Errorf("%s返回 HTTP %d: %s", name, response.StatusCode, strings.TrimSpace(string(body)))
}

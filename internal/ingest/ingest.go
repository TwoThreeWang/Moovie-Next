// Package ingest 把外部来源的元数据采集进作品库：豆瓣列表、Wikidata、NeoDB、TMDB 先建档，豆瓣详情按需补齐。
// 它不接触任何资源站；资源站的元数据不会进入作品库。
//
// 豆瓣详情的请求很少（被限流时一小时只有几十次），所以不让建档等它。一部作品收进来时同时排两条路：
//
//	建档：Wikidata、NeoDB、TMDB（media.enrich）→ AI 写语义描述（media.semantic）→ 算向量（media.embed）
//	豆瓣详情（media.refresh），成功后再排一次 media.enrich，按豆瓣的资料补齐
//
// 建档时 NeoDB 优先、TMDB 补空；豆瓣详情给出的值可以覆盖。
// 每一步是一个单独的任务，成功了马上保存、再排下一步；失败了只重试这一步：TMDB 出错不会让豆瓣再被请求一次，
// 向量出错也不会让 AI 再被调用一次。
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"moovie/internal/jobs"
	"moovie/internal/media"
	"moovie/internal/providers"
)

// 任务类型。subject 是作品 ID 的那几种组成一条链，见包注释。
const (
	KindRefresh  = "media.refresh"
	KindEnrich   = "media.enrich"
	KindSemantic = "media.semantic"
	KindEmbed    = "media.embed"
	KindCharts   = "charts.refresh"
	KindSweep    = "media.sweep"
	// KindRelease 给加上上映日期之前就入库的作品补日期，不在那条链上，subject 是作品 ID。
	KindRelease = "media.release"
	// KindTMDBSeries 独立刷新长剧的季状态和分集。
	KindTMDBSeries = "media.tmdb_series"
	// KindPopularity 每天用 TMDB 的导出文件更新全部作品的近期关注度。
	KindPopularity = "tmdb.popularity"
)

// maxSemanticRunes 是语义描述最多保留的字数，和老系统一致（向量模型一次读不了更多）。
const maxSemanticRunes = 450

// 全站后台任务共用的优先级（豆瓣同步 doubansync 也用这几档）。
//
// 豆瓣、红果允许的请求数有限，被限流时一小时只有几十次，排不完的总是最低的那几档，所以按这几条排：
//  1. 有人正盯着屏幕等结果的最先。没登录的访客（包括爬虫）不算：他们打开作品页只会排建档，不会去请求豆瓣。
//  2. 一次只有一个、只花几次请求、所有访客都看得到的周期任务（首页榜单）其次：先做几乎不占预算，晚了大家都看到旧榜单。
//  3. 用户自己的数据（每天的豆瓣增量同步）排在补资料前面：没有新标记的用户每人每天只花两次请求，
//     而且它不做，用户新标记的作品就根本不会出现；补资料不做，作品也已经有片名和海报能用。
//  4. 只是让资料更新鲜的排最后：限流时它们一直排不上也不影响使用。
//
// 优先级只决定空出来的执行位先拿哪个，不会打断正在跑的任务。
const (
	PriorityViewing   = 20 // 登录用户正开着这部作品的页面
	PriorityManual    = 10 // 有人点了按钮在等：管理员手动刷新、用户点"同步豆瓣"
	PriorityLists     = 8  // 首页的豆瓣热门榜、短剧频道的红果榜单
	PriorityDailySync = 6  // 每天给每个绑定了豆瓣的用户做一次增量同步
	PriorityDiscover  = 5  // 榜单、搜索带来的新作品：卡片上已有片名和海报，有人点开时会升到 PriorityViewing
	PriorityImport    = 0  // 用户导入豆瓣标记带来的新作品：列表里已经有片名和海报，晚点补全不影响使用
	PriorityRoutine   = -1 // 定期重新同步已有资料的作品
	PriorityBackfill  = -2 // 巡检补漏：AI 描述、向量、上映日期（各有各的通道或不用豆瓣，排在最后也不挡别人）
)

// ChartKinds 是首页用到的榜单，和豆瓣的榜单类型一一对应。
var ChartKinds = []string{media.TypeMovie, media.TypeTV, media.TypeShow, media.TypeAnime}

// Service 是采集流程。Wikidata、NeoDB、TMDB、AI、Embedder 可以为 nil 或未配置，对应步骤会自动跳过。
// 没配 AI 时不写语义描述，新作品也就没有向量（相似推荐改按类型重合度）。
type Service struct {
	Media    *media.Store
	Queue    *jobs.Queue
	Douban   *providers.Douban
	Wikidata *providers.Wikidata
	NeoDB    *providers.NeoDB
	TMDB     *providers.TMDB
	AI       *providers.OpenAI  // 写语义描述
	Embedder *providers.OpenAI  // 算向量
	Hongguo  *providers.Hongguo // 短剧，为 nil 时不处理短剧（见 hongguo.go）
	// ShortSync 为 true 时定期从红果抓榜单、重点剧目、分类和新剧；为 false 时只在有人打开时刷新单部短剧。
	ShortSync bool
	Log       *slog.Logger

	short shortState

	probeMu sync.Mutex
	probed  map[string]time.Time // 最近去豆瓣查过联想的关键词。只为少发重复请求，重启后清空也没关系。
}

// claimProbe 登记一次"去豆瓣查这个关键词"。冷却期内已经查过时返回 false。
func (s *Service) claimProbe(keyword string, cooldown time.Duration) bool {
	normalized := media.Normalize(keyword)
	if normalized == "" {
		return false
	}
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if s.probed == nil || len(s.probed) > 50000 {
		s.probed = map[string]time.Time{}
	}
	if at, ok := s.probed[normalized]; ok && time.Since(at) < cooldown {
		return false
	}
	s.probed[normalized] = time.Now()
	return true
}

// Step 是链上一步的参数。Force 表示这一轮是管理员手动发起的：后面每一步都不管冷却期，整条重跑。
type Step struct {
	Force bool `json:"force,omitempty"`
}

// next 排链上的下一步：优先级跟着上一步走（有人在等的作品，每一步都优先）。
func (s *Service) next(ctx context.Context, kind string, id int64, job jobs.Job) error {
	var step Step
	json.Unmarshal(job.Payload, &step)
	_, err := s.Queue.Enqueue(ctx, kind, strconv.FormatInt(id, 10), jobs.Options{Payload: step, Priority: job.Priority, Force: step.Force})
	return err
}

// Register 把任务处理函数和周期任务注册到 Worker。
func (s *Service) Register(worker *jobs.Worker) {
	for kind, run := range map[string]func(context.Context, int64, jobs.Job) error{
		KindRefresh: s.Refresh, KindEnrich: s.Enrich, KindSemantic: s.Semantic, KindEmbed: s.Embed, KindRelease: s.Release,
		KindTMDBSeries: s.RefreshTMDBSeries,
	} {
		worker.Handle(kind, func(ctx context.Context, job jobs.Job) error {
			id, err := strconv.ParseInt(job.Subject, 10, 64)
			if err != nil {
				return jobs.ErrSkip
			}
			return run(ctx, id, job)
		})
	}
	worker.Handle(KindCharts, func(ctx context.Context, _ jobs.Job) error { return s.RefreshCharts(ctx) })
	worker.Handle(KindSweep, func(ctx context.Context, _ jobs.Job) error { return s.Sweep(ctx) })
	worker.Every(KindCharts, 6*time.Hour, PriorityLists)
	worker.Every(KindSweep, time.Hour, 0)
	worker.Handle(KindPopularity, func(ctx context.Context, _ jobs.Job) error { return s.RefreshPopularity(ctx) })
	worker.Every(KindPopularity, 24*time.Hour, 0)
	s.registerShort(worker)
}

// EnqueueRefresh 让后台重新同步一部作品的元数据（短剧找红果，其余找豆瓣）。这部作品处在失败冷却期时不会入队。
func (s *Service) EnqueueRefresh(ctx context.Context, id int64, priority int) error {
	kind, err := s.refreshKind(ctx, id)
	if err != nil {
		return err
	}
	_, err = s.Queue.Enqueue(ctx, kind, strconv.FormatInt(id, 10), jobs.Options{Priority: priority})
	return err
}

// RefreshNow 是管理员手动刷新：不管冷却期，整条重跑。
func (s *Service) RefreshNow(ctx context.Context, id int64) error {
	kind, err := s.refreshKind(ctx, id)
	if err != nil {
		return err
	}
	_, err = s.Queue.Enqueue(ctx, kind, strconv.FormatInt(id, 10),
		jobs.Options{Payload: Step{Force: true}, Priority: PriorityManual, Force: true})
	return err
}

// EnsureDouban 保证一个豆瓣条目在作品库里有一行。新建的作品马上排建档（不用豆瓣），同时按 priority 排豆瓣详情。
// 列表里带着片名时马上按屏蔽词检查一遍，命中的不等资料补齐就隐藏。
// chartType 非空时作为作品类型的初始值（榜单类型比联想接口给的类型更细）。
func (s *Service) EnsureDouban(ctx context.Context, item providers.DoubanItem, chartType string, priority int) (int64, error) {
	return s.ensureDouban(ctx, item, chartType, priority, true)
}

// EnsureLegacyDouban 从旧链接收录只带豆瓣 ID 的作品，立即排建档，详情留给登录访问或巡检。
func (s *Service) EnsureLegacyDouban(ctx context.Context, doubanID string) (int64, error) {
	return s.ensureDouban(ctx, providers.DoubanItem{ID: doubanID}, "", PriorityBackfill, false)
}

func (s *Service) ensureDouban(ctx context.Context, item providers.DoubanItem, chartType string, priority int, refresh bool) (int64, error) {
	seed := media.Patch{}
	seed.Set("title", item.Title)
	seed.Set("year", item.Year)
	seed.Set("poster", item.Poster)
	seed.Set("rating_douban", item.Rating)
	switch {
	case media.ValidType(chartType):
		seed.Set("media_type", chartType)
	case item.Kind == "tv":
		seed.Set("media_type", media.TypeTV)
	}
	if _, season := media.SplitSeason(item.Title); season > 1 {
		seed.Set("season", season)
	}
	id, created, err := s.Media.EnsureDouban(ctx, item.ID, seed)
	if err != nil {
		return 0, err
	}
	if !created {
		// 已有的作品：列表里带的评分、年份、海报顺手更新，省掉一次详情请求。
		if err := s.Media.ApplyListing(ctx, id, item.Rating, item.Year, item.Poster); err != nil {
			return id, err
		}
	}
	if !created {
		return id, nil
	}
	if item.Title != "" {
		if err := s.Media.ApplyBlockedKeywords(ctx, id); err != nil {
			return id, err
		}
	}
	subject := strconv.FormatInt(id, 10)
	if _, err := s.Queue.Enqueue(ctx, KindEnrich, subject, jobs.Options{Payload: Step{}, Priority: priority}); err != nil {
		return id, err
	}
	if refresh {
		return id, s.EnqueueRefresh(ctx, id, priority)
	}
	return id, nil
}

// Viewed 在有人打开作品页时调用：资料还没补过的，马上排建档；登录用户打开的、豆瓣详情还没同步过的，
// 豆瓣详情提到最前面（PriorityViewing）。没登录的访客（包括爬虫）不触发豆瓣请求：他们看到的是建档的资料。
func (s *Service) Viewed(ctx context.Context, m media.Media, signedIn bool) error {
	if m.IsShort() {
		// 短剧只有红果一个来源，不经过豆瓣。
		if m.MetadataSyncedAt == nil {
			return s.EnqueueRefresh(ctx, m.ID, PriorityViewing)
		}
		return nil
	}
	if m.MetadataStatus == "not_found" {
		return nil
	}
	if m.EnrichedAt == nil {
		if _, err := s.Queue.Enqueue(ctx, KindEnrich, strconv.FormatInt(m.ID, 10), jobs.Options{Payload: Step{}, Priority: PriorityViewing}); err != nil {
			return err
		}
	}
	if signedIn && m.MetadataSyncedAt == nil {
		return s.EnqueueRefresh(ctx, m.ID, PriorityViewing)
	}
	return nil
}

// Discover 用关键词去豆瓣查联想，把查到的条目收进作品库。同一个关键词一天内只查一次。
func (s *Service) Discover(ctx context.Context, keyword string) error {
	if !s.claimProbe(keyword, 24*time.Hour) {
		return nil
	}
	items, err := s.Douban.Suggest(ctx, keyword)
	if err != nil {
		return err
	}
	for _, item := range items {
		if _, err := s.EnsureDouban(ctx, item, "", PriorityDiscover); err != nil {
			return err
		}
	}
	return nil
}

// DiscoverDoubanID 把一个还不在作品库里的豆瓣 ID 收进来。条目是否真的存在由后台的 Refresh 向豆瓣确认。
func (s *Service) DiscoverDoubanID(ctx context.Context, doubanID string) error {
	_, err := s.EnsureDouban(ctx, providers.DoubanItem{ID: doubanID}, "", PriorityDiscover)
	return err
}

// doubanType 根据豆瓣详情判断作品类型。
func doubanType(subject *providers.DoubanSubject) string {
	if subject.Kind == "movie" {
		return media.TypeMovie
	}
	if subject.IsShow {
		return media.TypeShow
	}
	for _, genre := range subject.Genres {
		switch genre {
		case "真人秀", "脱口秀", "综艺":
			return media.TypeShow
		case "动画":
			return media.TypeAnime
		}
	}
	return media.TypeTV
}

// Refresh 从豆瓣读详情。成功后按豆瓣的资料再补一轮（media.enrich）。
// 请求之前按 media.Store.DoubanDue 再确认一次还要不要去：任务可能排了很久，期间插件交来了资料、或者已经不需要了。
// 管理员手动刷新（Force）不管这些。
func (s *Service) Refresh(ctx context.Context, id int64, job jobs.Job) error {
	m, err := s.Media.Get(ctx, id)
	if errors.Is(err, media.ErrNotFound) {
		return jobs.ErrSkip
	}
	if err != nil {
		return err
	}
	if m.HongguoID != "" {
		// 短剧不走豆瓣。正常不会排到这里（EnqueueRefresh 会排 hongguo.refresh），以防万一。
		if s.Hongguo == nil {
			return jobs.ErrSkip
		}
		return s.RefreshShort(ctx, id)
	}
	var step Step
	json.Unmarshal(job.Payload, &step)
	if !step.Force {
		due, err := s.Media.DoubanDue(ctx, id)
		if err != nil {
			return err
		}
		if !due {
			return jobs.ErrSkip
		}
	}
	if m.DoubanID != "" {
		first := ""
		switch {
		case m.Type != media.TypeMovie:
			first = "tv"
		case m.Title != "" || m.MetadataStatus == "complete":
			first = "movie"
		}
		// 只有 ID、还没片名时可能是占位的电影类型，允许在电影接口暂停时尝试剧集。
		subject, err := s.Douban.Subject(ctx, m.DoubanID, first)
		if err != nil && job.Priority >= PriorityManual && (errors.Is(err, providers.ErrThrottled) || errors.Is(err, providers.ErrRefused)) {
			// 有人在等（登录用户开着作品页、管理员手动刷新）：详情接口被限流或拒绝了这个条目，改打开详情网页。
			// 只给有人等的用，后台批量的任务也来抢的话，网页这条路很快也会被限流。网页说没有这个条目时不算数，接口的结论还没出来。
			page, pageErr := s.Douban.SubjectPage(ctx, m.DoubanID)
			if pageErr == nil {
				subject, err = page, nil
			} else {
				s.Log.Info("豆瓣详情网页兜底也没成功", "douban", m.DoubanID, "error", pageErr)
			}
		}
		if errors.Is(err, providers.ErrNotFound) {
			// 豆瓣上没有这个条目：标记后不再自动同步，也不在站内展示。
			if err := s.Media.MarkSynced(ctx, id, "not_found"); err != nil {
				return err
			}
			return s.Media.SetHidden(ctx, id, true)
		}
		if err != nil {
			return fmt.Errorf("豆瓣详情: %w", err)
		}
		if err := s.saveDouban(ctx, id, subject); err != nil {
			return err
		}
	}
	if err := s.Media.MarkSynced(ctx, id, "complete"); err != nil {
		return err
	}
	if err := s.Media.ApplyBlockedKeywords(ctx, id); err != nil {
		return err
	}
	return s.afterDouban(ctx, id, job)
}

// afterDouban 在豆瓣的资料到了以后，按新资料再补一轮。补资料这一步正处在失败冷却期、排不上时，
// 直接往下写描述：豆瓣的资料已经够了，不用等冷却期过去。
func (s *Service) afterDouban(ctx context.Context, id int64, job jobs.Job) error {
	if err := s.next(ctx, KindEnrich, id, job); err != nil {
		return err
	}
	last, found, err := s.Queue.Latest(ctx, KindEnrich, strconv.FormatInt(id, 10))
	if err != nil || !found || last.Status != "failed" {
		return err
	}
	return s.nextSemantic(ctx, id, job)
}

// ApplyContributed 写入浏览器插件在用户的浏览器里取回的豆瓣详情（见 internal/app/extension.go），
// 之后的步骤和服务器自己抓到时一样，按 priority 排补充资料。
// 还排着的豆瓣任务随之取消：资料已经有了，不用服务器再去请求一次。
func (s *Service) ApplyContributed(ctx context.Context, id int64, subject *providers.DoubanSubject, priority int) error {
	if err := s.saveDouban(ctx, id, subject); err != nil {
		return err
	}
	if err := s.Media.MarkSynced(ctx, id, "complete"); err != nil {
		return err
	}
	if err := s.Media.ApplyBlockedKeywords(ctx, id); err != nil {
		return err
	}
	if err := s.Queue.Drop(ctx, KindRefresh, strconv.FormatInt(id, 10)); err != nil {
		return err
	}
	return s.afterDouban(ctx, id, jobs.Job{Payload: []byte("{}"), Priority: priority})
}

// saveDouban 把豆瓣的详情写进作品库。豆瓣给的值总是写入。
func (s *Service) saveDouban(ctx context.Context, id int64, subject *providers.DoubanSubject) error {
	patch := media.Patch{}
	patch.Set("media_type", doubanType(subject))
	patch.Set("title", subject.Title)
	patch.Set("original_title", subject.OriginalTitle)
	patch.Set("year", subject.Year)
	if base, season := media.SplitSeason(subject.Title); base != strings.TrimSpace(subject.Title) {
		patch.Set("season", season)
	} else if base, season := media.SplitSeason(subject.OriginalTitle); base != strings.TrimSpace(subject.OriginalTitle) {
		patch.Set("season", season)
	}
	patch.Set("episode_count", subject.EpisodesCount)
	patch.Set("poster", subject.Poster)
	patch.Set("summary", subject.Intro)
	patch.Set("genres", subject.Genres)
	patch.Set("countries", subject.Countries)
	patch.Set("directors", subject.Directors)
	patch.Set("actors", limit(subject.Actors, 12))
	patch.Set("duration_minutes", subject.DurationMinutes)
	patch.Set("rating_douban", subject.Rating)
	if err := s.Media.ApplyPatch(ctx, id, patch); err != nil {
		return err
	}
	return s.Media.AddAliases(ctx, id, subject.AKA...)
}

// Enrich 按 Wikidata、NeoDB、TMDB 的顺序查询。Wikidata 提供外部编号、别名和查找用的片名，
// NeoDB 的中文资料优先，TMDB 补齐这一轮剩余的字段，并更新剧照、集数等。
// 豆瓣详情同步过以后不再查 NeoDB。
//
// Wikidata、TMDB 各自的资料拿到就保存；任何一个出错（"没有这个条目"不算出错）这一步就算失败，稍后重试。
// NeoDB 只是补充，出错只记日志：不为它让 Wikidata、TMDB 再请求一遍。
// 最后一次重试也失败时，照样往下排：AI 用现有的资料写描述，不必干等。
func (s *Service) Enrich(ctx context.Context, id int64, job jobs.Job) error {
	started := time.Now()
	m, err := s.Media.Get(ctx, id)
	if errors.Is(err, media.ErrNotFound) {
		return jobs.ErrSkip
	}
	if err != nil {
		return err
	}
	var failures []error
	patch := media.Patch{}
	wikidataTitle := ""
	if s.Wikidata != nil && m.DoubanID != "" {
		wikidataTitle, err = s.enrichWikidata(ctx, &m, patch)
		if err != nil && !errors.Is(err, providers.ErrNotFound) {
			failures = append(failures, fmt.Errorf("Wikidata: %w", err))
		}
	}
	if s.NeoDB.Enabled() && m.MetadataStatus != "complete" {
		searchTitle := m.Title
		if searchTitle == "" {
			searchTitle = wikidataTitle
		}
		if err := s.enrichNeoDB(ctx, &m, searchTitle, patch); err != nil && !errors.Is(err, providers.ErrNotFound) {
			s.Log.Info("NeoDB 补资料没成功", "media", id, "error", err)
		}
	}
	if _, hasTitle := patch["title"]; !hasTitle {
		patch.Set("title", wikidataTitle)
	}
	if m.Title == "" {
		m.Title, _ = patch["title"].(string)
	}
	if s.TMDB.Enabled() {
		if err := s.enrichTMDB(ctx, m, started, patch); err != nil && !errors.Is(err, providers.ErrNotFound) {
			failures = append(failures, fmt.Errorf("TMDB: %w", err))
		}
	}
	if err := s.Media.FillMetadata(ctx, id, patch); err != nil {
		failures = append(failures, err)
	}
	// 片名可能是这一步才有的（只凭豆瓣 ID 收进来的作品），屏蔽词现在就查，不等豆瓣。
	if err := s.Media.ApplyBlockedKeywords(ctx, id); err != nil {
		failures = append(failures, err)
	}
	if err := errors.Join(failures...); err != nil {
		if job.Attempts >= jobs.MaxAttempts {
			if nextErr := s.nextSemantic(ctx, id, job); nextErr != nil {
				return errors.Join(err, nextErr)
			}
		}
		return err
	}
	if err := s.Media.MarkEnriched(ctx, id, started); err != nil {
		return err
	}
	return s.nextSemantic(ctx, id, job)
}

// nextSemantic 在资料够写语义描述时排下一步（见 media.Media.SemanticReady）。豆瓣还没同步、建档又不久的作品先不写：
// 豆瓣的资料到了还得再写一遍。到时候由豆瓣那一步之后的补资料、或者巡检排上。
func (s *Service) nextSemantic(ctx context.Context, id int64, job jobs.Job) error {
	m, err := s.Media.Get(ctx, id)
	if err != nil {
		return err
	}
	if !m.SemanticReady() {
		return nil
	}
	return s.next(ctx, KindSemantic, id, job)
}

func (s *Service) enrichWikidata(ctx context.Context, m *media.Media, patch media.Patch) (string, error) {
	entity, err := s.Wikidata.ByDoubanID(ctx, m.DoubanID)
	if err != nil {
		return "", err
	}
	tmdbID := ""
	switch {
	case entity.TMDBMovieID != "":
		tmdbID = "movie/" + entity.TMDBMovieID
	case entity.TMDBTVID != "":
		tmdbID = "tv/" + entity.TMDBTVID
	}
	idErr := s.recordExternalIDs(ctx, m, tmdbID, entity.IMDbID)
	if m.Type == media.TypeMovie && m.MetadataStatus == "partial" && strings.HasPrefix(m.TMDBID, "tv/") {
		m.Type = media.TypeTV
		patch.Set("media_type", media.TypeTV)
	}
	return entity.Title, errors.Join(
		idErr,
		s.Media.SetExternalID(ctx, m.ID, media.ProviderWikidata, entity.QID),
		s.Media.AddAliases(ctx, m.ID, append(entity.Labels, entity.Title)...),
	)
}

// enrichNeoDB 对上条目后，提供中文资料和补充编号。评分是 NeoDB 用户打的分，不取。
func (s *Service) enrichNeoDB(ctx context.Context, m *media.Media, searchTitle string, patch media.Patch) error {
	item, err := s.NeoDB.Find(ctx, providers.NeoDBQuery{DoubanID: m.DoubanID, IMDbID: m.IMDbID, Title: searchTitle, TV: m.IsSeries(), Season: m.Season})
	if err != nil {
		return err
	}
	patch.Set("title", item.Title)
	patch.Set("original_title", item.OriginalTitle)
	patch.Set("summary", item.Summary)
	patch.Set("genres", item.Genres)
	patch.Set("directors", item.Directors)
	patch.Set("actors", limit(item.Actors, 12))
	return errors.Join(s.recordExternalIDs(ctx, m, item.TMDBID, item.IMDbID), s.Media.AddAliases(ctx, m.ID, item.AKA...))
}

func (s *Service) recordExternalIDs(ctx context.Context, m *media.Media, tmdbID, imdbID string) error {
	tmdbErr := s.Media.SetExternalID(ctx, m.ID, media.ProviderTMDB, tmdbID)
	imdbErr := s.Media.SetExternalID(ctx, m.ID, media.ProviderIMDb, imdbID)
	if m.TMDBID == "" && tmdbErr == nil {
		m.TMDBID = tmdbID
	}
	if m.IMDbID == "" && imdbErr == nil {
		m.IMDbID = imdbID
	}
	return errors.Join(tmdbErr, imdbErr)
}

// Semantic 让 AI 把资料写成一段语义描述，写好马上保存，再排算向量的任务。
// 资料和上次写描述时一样（哈希相同）就不再调用 AI。
func (s *Service) Semantic(ctx context.Context, id int64, job jobs.Job) error {
	if !s.AI.Enabled() {
		return jobs.ErrSkip
	}
	m, err := s.Media.Get(ctx, id)
	if errors.Is(err, media.ErrNotFound) {
		return jobs.ErrSkip
	}
	if err != nil {
		return err
	}
	if !m.SemanticReady() {
		return jobs.ErrSkip
	}
	changed := false
	if hash := m.InputHash(); m.SemanticText == "" || m.SemanticHash != hash {
		text, err := s.AI.Chat(ctx, semanticSystem, fmt.Sprintf(semanticPrompt, m.SemanticInput()))
		if err != nil {
			return fmt.Errorf("AI 写语义描述: %w", err)
		}
		if runes := []rune(text); len(runes) > maxSemanticRunes {
			text = string(runes[:maxSemanticRunes])
		}
		if err := s.Media.SaveSemantic(ctx, id, hash, text); err != nil {
			return err
		}
		changed = text != m.SemanticText
	}
	if !s.Embedder.Enabled() || (m.HasEmbedding && !changed) {
		return nil
	}
	return s.next(ctx, KindEmbed, id, job)
}

// Embed 是最后一步：用语义描述算向量。没有描述、或者已经有向量了就跳过。
func (s *Service) Embed(ctx context.Context, id int64, _ jobs.Job) error {
	if !s.Embedder.Enabled() {
		return jobs.ErrSkip
	}
	m, err := s.Media.Get(ctx, id)
	if errors.Is(err, media.ErrNotFound) {
		return jobs.ErrSkip
	}
	if err != nil {
		return err
	}
	if m.SemanticText == "" || m.HasEmbedding {
		return jobs.ErrSkip
	}
	vector, err := s.Embedder.Embed(ctx, m.SemanticText)
	if err != nil {
		return fmt.Errorf("算向量: %w", err)
	}
	return s.Media.SetEmbedding(ctx, id, m.SemanticText, vector)
}

// Release 给加上上映日期之前就入库的作品补日期和近期关注度：只按已知的 TMDB 编号读一次详情，别的资料都不动，
// 所以不会引起重新写语义描述。TMDB 上没有这个条目时也记为查过，不再重试。
func (s *Service) Release(ctx context.Context, id int64, _ jobs.Job) error {
	if !s.TMDB.Enabled() {
		return jobs.ErrSkip
	}
	m, err := s.Media.Get(ctx, id)
	if errors.Is(err, media.ErrNotFound) {
		return jobs.ErrSkip
	}
	if err != nil {
		return err
	}
	kind, number, _ := strings.Cut(m.TMDBID, "/")
	tmdbID, _ := strconv.Atoi(number)
	if tmdbID <= 0 || (kind != "movie" && kind != "tv") {
		return s.Media.SetRelease(ctx, m, nil, 0)
	}
	details, err := s.TMDB.Details(ctx, kind, tmdbID)
	if errors.Is(err, providers.ErrNotFound) {
		return s.Media.SetRelease(ctx, m, nil, 0)
	}
	if err != nil {
		return err
	}
	released := details.ReleaseOf(m.Season)
	if kind == "tv" && matchingTMDBSeason(m, details) == nil {
		released = nil
	}
	return s.Media.SetRelease(ctx, m, released, details.Popularity)
}

// RefreshPopularity 用 TMDB 每天的导出文件（全部电影、全部剧集各一个文件）更新作品库里所有作品的近期关注度。
// 当天的文件 UTC 早上才生成，还没有时用前一天的。不占 TMDB 的 API 配额。
func (s *Service) RefreshPopularity(ctx context.Context) error {
	if !s.TMDB.Enabled() {
		return nil
	}
	index, err := s.Media.TMDBIndex(ctx)
	if err != nil || len(index) == 0 {
		return err
	}
	var ids []int64
	var values []float64
	today := time.Now().UTC()
	for _, kind := range []string{"movie", "tv"} {
		for _, day := range []time.Time{today, today.AddDate(0, 0, -1)} {
			err = s.TMDB.Popularity(ctx, kind, day, func(tmdbID int, popularity float64) {
				for _, id := range index[kind+"/"+strconv.Itoa(tmdbID)] {
					ids, values = append(ids, id), append(values, popularity)
				}
			})
			if !errors.Is(err, providers.ErrNotFound) {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("读取 TMDB %s 导出文件: %w", kind, err)
		}
	}
	return s.Media.SetPopularity(ctx, ids, values)
}

// 热门榜的综合：三个来源各自给出一个名次表，每部作品的分数 = Σ 权重 ÷ (chartDamping + 名次)。
// 只用名次、不用各家的评分或热度值，因为那些数没法放在一起比。chartDamping 让前几名之间拉开差距，
// 又不至于让单一来源的第一名压过同时出现在几个来源里的作品。
const (
	weightDouban   = 0.5 // 最贴近国内观众，作为主体
	weightTMDB     = 0.2 // 补海外的新片新剧；只算作品库里已有的，不会因此去请求豆瓣
	weightSite     = 0.3 // 本站最近 7 天看的人数
	chartDamping   = 20
	chartSize      = 50
	minSiteViewers = 20 // 最近 7 天看过东西的人少于这个数时，本站的数据不计入，免得两三个人就能把一部片子顶上榜
)

// blend 按权重综合几个名次表，返回前 size 部作品。分数相同时，先出现在靠前的表里的排前面。
func blend(lists [][]int64, weights []float64, size int) []int64 {
	score := map[int64]float64{}
	var order []int64
	for index, list := range lists {
		if weights[index] <= 0 {
			continue // 不计入的来源，它独有的作品也不该出现在榜上
		}
		for rank, id := range list {
			if _, seen := score[id]; !seen {
				order = append(order, id)
			}
			score[id] += weights[index] / float64(chartDamping+rank+1)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return score[order[a]] > score[order[b]] })
	return order[:min(len(order), size)]
}

// ofType 从候选里挑出某一类的作品 ID，保持原来的名次。
func ofType(items []media.Ranked, kind string) []int64 {
	ids := []int64{}
	for _, item := range items {
		if item.Type == kind {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

// RefreshCharts 重新生成各个热门榜：豆瓣热门为主，综合 TMDB 一周趋势和本站最近 7 天在看的。
// 某一类的豆瓣榜取不到时，这一类保持原样（只靠另外两个来源会和平时差得太多）；TMDB 和本站的数据取不到就不算它们。
func (s *Service) RefreshCharts(ctx context.Context) error {
	// TMDB 只分电影和剧集；剧集趋势里的作品按作品库里的类型分到剧集、综艺、动漫。
	trending := map[string][]media.Ranked{}
	if s.TMDB.Enabled() {
		for _, kind := range []string{"movie", "tv"} {
			ids, err := s.TMDB.Trending(ctx, kind)
			if err != nil {
				s.Log.Warn("TMDB 趋势榜取不到，这次不计入", "kind", kind, "error", err)
				continue
			}
			keys := make([]string, 0, len(ids))
			for _, id := range ids {
				keys = append(keys, kind+"/"+strconv.Itoa(id))
			}
			if trending[kind], err = s.Media.ByTMDB(ctx, keys); err != nil {
				return err
			}
		}
	}
	watching, viewers, err := s.Media.Watching(ctx, 7, 300)
	if err != nil {
		return err
	}
	siteWeight := weightSite
	if viewers < minSiteViewers {
		siteWeight = 0
	}

	var failures []error
	for _, kind := range ChartKinds {
		items, err := s.Douban.Popular(ctx, kind)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", kind, err))
			continue
		}
		douban := make([]int64, 0, len(items))
		for _, item := range items {
			id, err := s.EnsureDouban(ctx, item, kind, PriorityDiscover)
			if err != nil {
				s.Log.Warn("榜单条目入库失败", "douban", item.ID, "error", err)
				continue
			}
			douban = append(douban, id)
		}
		if len(douban) == 0 {
			continue
		}
		tmdbKind := "tv"
		if kind == media.TypeMovie {
			tmdbKind = "movie"
		}
		ids := blend([][]int64{douban, ofType(trending[tmdbKind], kind), ofType(watching, kind)},
			[]float64{weightDouban, weightTMDB, siteWeight}, chartSize)
		if err := s.Media.ReplaceChart(ctx, kind, ids); err != nil {
			return err
		}
	}
	if len(failures) == len(ChartKinds) {
		// 每个错误都包住，不只留文字：都是被限流时，任务队列要靠它们认出限流（ErrThrottled），
		// 推迟到豆瓣恢复的时候（RetryAfter），而不是算一次失败、一分钟后再去打扰。
		verbs := strings.TrimSuffix(strings.Repeat("%w; ", len(failures)), "; ")
		args := make([]any, len(failures))
		for index, failure := range failures {
			args[index] = failure
		}
		return fmt.Errorf("所有榜单都刷新失败: "+verbs, args...)
	}
	return nil
}

// Sweep 是每小时一次的巡检：把漏掉的建档、豆瓣详情（见 media.Store.Stale）、描述和向量补上，短剧把没抓到的、连载中的重新抓一遍。正在排队、运行或处在失败冷却期的作品不会被挑中。
func (s *Service) Sweep(ctx context.Context) error {
	type pick struct {
		kind     string
		enabled  bool
		priority int
		find     func() ([]int64, error)
	}
	for _, item := range []pick{
		{KindRefresh, true, PriorityRoutine, func() ([]int64, error) { return s.Media.Stale(ctx, KindRefresh, 200) }},
		// 建档不用豆瓣，Wikidata、TMDB 给的额度也大，一次多挑一些。
		{KindEnrich, s.Wikidata != nil || s.NeoDB.Enabled() || s.TMDB.Enabled(), PriorityRoutine, func() ([]int64, error) { return s.Media.NeedsEnrich(ctx, KindEnrich, 500) }},
		{KindSemantic, s.AI.Enabled(), PriorityBackfill, func() ([]int64, error) { return s.Media.NeedsSemantic(ctx, KindSemantic, KindEnrich, 100) }},
		{KindEmbed, s.Embedder.Enabled(), PriorityBackfill, func() ([]int64, error) { return s.Media.MissingEmbedding(ctx, KindEmbed, 200) }},
		{KindShortRefresh, s.Hongguo != nil, PriorityRoutine, func() ([]int64, error) { return s.Media.StaleShort(ctx, KindShortRefresh, 300) }},
		{KindTMDBSeries, s.TMDB.Enabled(), PriorityRoutine, func() ([]int64, error) {
			return s.Media.NeedsTMDBSeries(ctx, KindTMDBSeries, KindEnrich, 100)
		}},
		// 补上映日期：每个作品只问 TMDB 一次，补完就不再挑中。
		{KindRelease, s.TMDB.Enabled(), PriorityBackfill, func() ([]int64, error) { return s.Media.NeedsRelease(ctx, KindRelease, 1000) }},
	} {
		if !item.enabled {
			continue
		}
		ids, err := item.find()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := s.Queue.Enqueue(ctx, item.kind, strconv.FormatInt(id, 10), jobs.Options{Priority: item.priority}); err != nil {
				return err
			}
		}
	}
	return nil
}

func limit(values []string, count int) []string {
	if len(values) > count {
		return values[:count]
	}
	return values
}

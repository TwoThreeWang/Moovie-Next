package ingest

// 短剧：资料只来自红果短剧官网，一部短剧只有一步（hongguo.refresh），不补 TMDB、Wikidata，也不写 AI 描述和向量
// （相似推荐按题材重合度排）。
//
// 全量的几十万部由 cmd/hongguo-seed 在本地抓一次、导入作品库；线上只做增量：
//
//	hongguo.lists      每天：热播榜、新剧榜（生成短剧频道的两个榜单）、分剧种的榜单（补剧种）、重点剧目（约 2000 部热门，刷新热度和集数）
//	hongguo.categories 每周：三个剧种的分类页（每个剧种 800 部，补剧种）
//	hongguo.sitemaps   每天：对比站点地图，把新上架的剧收进来，每天最多 maxNewShorts 部
//	巡检（每小时）       没抓到过的、连载中一天没更新的，重新抓详情

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"moovie/internal/jobs"
	"moovie/internal/media"
	"moovie/internal/providers"
)

// 短剧的任务类型。
const (
	KindShortRefresh    = "hongguo.refresh"
	KindShortLists      = "hongguo.lists"
	KindShortCategories = "hongguo.categories"
	KindShortSitemaps   = "hongguo.sitemaps"
)

// ShortKinds 是要访问红果的任务，在 app 里单独走一条通道。
var ShortKinds = []string{KindShortRefresh, KindShortLists, KindShortCategories, KindShortSitemaps}

// 短剧频道的两个榜单（chart_items 的 kind），和红果的榜单一一对应。
const (
	ChartShortHot = "short_hot"
	ChartShortNew = "short_new"
)

const (
	shortChartPages = 3    // 榜单每页 20 部，取前 3 页
	shortChartSize  = 50   // 短剧频道的榜单最多放 50 部
	maxNewShorts    = 3000 // 每天最多从站点地图收进来多少部新剧。导入过快照时每天的新剧远少于这个数；没导入时不会一下子排几十万个任务
)

// shortState 是站点地图的 ETag，只放在内存里：重启以后重新下载一遍（压缩后共十几 MB），不值得为它建表。
type shortState struct {
	mu    sync.Mutex
	etags map[string]string
}

func (s *shortState) get(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.etags[path]
}

func (s *shortState) set(path, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.etags == nil {
		s.etags = map[string]string{}
	}
	s.etags[path] = etag
}

// registerShort 注册短剧的任务。没配红果客户端时不注册；ShortSync 为 false 时只处理单部短剧的刷新，不定期抓列表。
func (s *Service) registerShort(worker *jobs.Worker) {
	if s.Hongguo == nil {
		return
	}
	worker.Handle(KindShortRefresh, func(ctx context.Context, job jobs.Job) error {
		id, err := strconv.ParseInt(job.Subject, 10, 64)
		if err != nil {
			return jobs.ErrSkip
		}
		return s.RefreshShort(ctx, id)
	})
	if !s.ShortSync {
		return
	}
	worker.Handle(KindShortLists, func(ctx context.Context, _ jobs.Job) error { return s.RefreshShortLists(ctx) })
	worker.Handle(KindShortCategories, func(ctx context.Context, _ jobs.Job) error { return s.RefreshShortCategories(ctx) })
	worker.Handle(KindShortSitemaps, func(ctx context.Context, _ jobs.Job) error { return s.DiscoverNewShorts(ctx) })
	worker.Every(KindShortLists, 24*time.Hour, PriorityLists)
	worker.Every(KindShortCategories, 7*24*time.Hour, 0)
	worker.Every(KindShortSitemaps, 24*time.Hour, 0)
}

// refreshKind 返回重新同步这部作品要排的任务：短剧找红果，其余找豆瓣。
func (s *Service) refreshKind(ctx context.Context, id int64) (string, error) {
	m, err := s.Media.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if m.HongguoID != "" {
		return KindShortRefresh, nil
	}
	return KindRefresh, nil
}

// RefreshShort 从红果抓一部短剧的详情。
func (s *Service) RefreshShort(ctx context.Context, id int64) error {
	m, err := s.Media.Get(ctx, id)
	if errors.Is(err, media.ErrNotFound) || (err == nil && m.HongguoID == "") {
		return jobs.ErrSkip
	}
	if err != nil {
		return err
	}
	series, err := s.Hongguo.Series(ctx, m.HongguoID)
	if errors.Is(err, providers.ErrNotFound) {
		return s.markShortMissing(ctx, id)
	}
	if err != nil {
		return fmt.Errorf("红果详情: %w", err)
	}
	return s.SaveShort(ctx, m, series, "")
}

func (s *Service) markShortMissing(ctx context.Context, id int64) error {
	// 下架了或者没有这部剧：不再自动抓，也不在站内展示（和豆瓣没有的条目一样）。
	if err := s.Media.MarkSynced(ctx, id, "not_found"); err != nil {
		return err
	}
	return s.Media.SetHidden(ctx, id, true)
}

// SaveShort 把红果的资料写进作品。form 是另外知道的剧种（分类页、榜单给的），为空时保留原来记的。
// 线上的刷新和导入快照都走这里。
func (s *Service) SaveShort(ctx context.Context, m media.Media, series *providers.HongguoSeries, form string) error {
	if form == "" {
		form = m.Form()
	}
	genres := series.Tags
	if form != "" {
		genres = append([]string{form}, genres...)
	}
	group := series.GroupID
	if group == "" {
		group = series.ID // 没有分组的就是单独一季：用自己的 ID，不和别的剧凑成一个系列
	}
	_, season := media.SplitSeason(series.Title)
	patch := media.Patch{}
	patch.Set("media_type", media.TypeShort)
	patch.Set("title", series.Title)
	patch.Set("series_key", "hg:"+group)
	patch.Set("season", season)
	if !series.OnlineAt.IsZero() {
		patch.Set("year", series.OnlineAt.In(shanghai).Year())
	}
	patch.Set("episode_count", series.Episodes)
	patch.Set("poster", series.Cover)
	patch.Set("summary", series.Intro)
	patch.Set("genres", genres)
	patch.Set("actors", limit(series.Actors, 12))
	patch.Set("rating_hongguo", series.Rating)
	patch.Set("heat", series.Heat)
	if series.Ended {
		patch.Set("series_status", "ended")
	} else {
		patch.Set("series_status", "ongoing")
	}
	if err := s.Media.ApplyPatch(ctx, m.ID, media.ProviderHongguo, patch); err != nil {
		return err
	}
	if err := s.Media.MarkSynced(ctx, m.ID, "complete"); err != nil {
		return err
	}
	return s.Media.ApplyBlockedKeywords(ctx, m.ID)
}

var shanghai = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return location
}()

// ensureShort 保证一部短剧在作品库里有一行。新建的、或者 refresh 为 true 的，排一次刷新。
func (s *Service) ensureShort(ctx context.Context, seriesID, form string, priority int, refresh bool) (int64, error) {
	id, created, err := s.Media.EnsureHongguo(ctx, seriesID)
	if err != nil {
		return 0, err
	}
	if form != "" {
		if err := s.Media.SetShortForm(ctx, id, form); err != nil {
			return id, err
		}
	}
	if created || refresh {
		if _, err := s.Queue.Enqueue(ctx, KindShortRefresh, strconv.FormatInt(id, 10), jobs.Options{Priority: priority}); err != nil {
			return id, err
		}
	}
	return id, nil
}

// RefreshShortLists 是每天一次的短剧榜单和重点剧目刷新。
func (s *Service) RefreshShortLists(ctx context.Context) error {
	var failures []string
	// 1. 热播榜、新剧榜：生成短剧频道的两个榜单，上榜的剧顺便刷新热度。
	for chart, kind := range map[string]string{ChartShortHot: "hot-drama", ChartShortNew: "new-drama"} {
		ids := []int64{}
		for page := 1; page <= shortChartPages; page++ {
			cards, err := s.Hongguo.Rank(ctx, kind, page)
			if err != nil {
				failures = append(failures, kind+": "+err.Error())
				break
			}
			for _, card := range cards {
				id, err := s.ensureShort(ctx, card.ID, card.Form, PriorityDiscover, true)
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			if len(cards) == 0 {
				break
			}
		}
		if len(ids) > 0 {
			if err := s.Media.ReplaceChart(ctx, chart, uniqueIDs(ids, shortChartSize)); err != nil {
				return err
			}
		}
	}
	// 2. 分剧种的榜单：只为了知道这些剧是真人剧、漫剧还是 AI 剧。
	for _, form := range providers.HongguoForms {
		for _, prefix := range []string{"hot-", "new-"} {
			cards, err := s.Hongguo.Rank(ctx, prefix+form.Slug+"-drama", 1)
			if err != nil {
				failures = append(failures, prefix+form.Slug+": "+err.Error())
				continue
			}
			for _, card := range cards {
				if _, err := s.ensureShort(ctx, card.ID, form.Name, PriorityDiscover, false); err != nil {
					return err
				}
			}
		}
	}
	// 3. 重点剧目：官网列出的约 2000 部热门短剧，每天刷新一次热度、评分和集数。
	ids, _, err := s.Hongguo.SitemapIDs(ctx, providers.ImportantSitemap, "")
	if err != nil {
		failures = append(failures, "重点剧目: "+err.Error())
	}
	for _, seriesID := range ids {
		if _, err := s.ensureShort(ctx, seriesID, "", PriorityRoutine, true); err != nil {
			return err
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("部分短剧列表没取到: %s", strings.Join(failures, "; "))
	}
	return nil
}

// RefreshShortCategories 读三个剧种的分类页，给上面的剧记上剧种。
func (s *Service) RefreshShortCategories(ctx context.Context) error {
	for _, form := range providers.HongguoForms {
		for page, pages := 1, 1; page <= pages && page <= 50; page++ {
			cards, total, err := s.Hongguo.Category(ctx, form.Name, page)
			if err != nil {
				return fmt.Errorf("%s第 %d 页: %w", form.Name, page, err)
			}
			pages = total
			for _, card := range cards {
				if _, err := s.ensureShort(ctx, card.ID, form.Name, PriorityRoutine, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// DiscoverNewShorts 对比站点地图，把作品库里还没有的短剧收进来。最新上架的优先（红果的 ID 随时间递增），
// 每天最多 maxNewShorts 部。一个子站点地图里的新剧全部收进来以后才记下它的 ETag，
// 没收完的下次照样重新读，不会因为它没变化就漏掉。
func (s *Service) DiscoverNewShorts(ctx context.Context) error {
	maps, err := s.Hongguo.Sitemaps(ctx)
	if err != nil {
		return err
	}
	type found struct {
		path, etag string
		ids        []string
	}
	var files []found
	total := 0
	for _, path := range maps {
		ids, etag, err := s.Hongguo.SitemapIDs(ctx, path, s.short.get(path))
		if errors.Is(err, providers.ErrNotModified) {
			continue
		}
		if err != nil {
			return fmt.Errorf("站点地图 %s: %w", path, err)
		}
		unknown := []string{}
		for start := 0; start < len(ids); start += 5000 {
			batch, err := s.Media.UnknownHongguo(ctx, ids[start:min(len(ids), start+5000)])
			if err != nil {
				return err
			}
			unknown = append(unknown, batch...)
		}
		files = append(files, found{path, etag, unknown})
		total += len(unknown)
	}
	// 全部新 ID 按从新到旧排，取前 maxNewShorts 个。
	all := make([]string, 0, total)
	for _, file := range files {
		all = append(all, file.ids...)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(all)))
	taken := map[string]bool{}
	for _, seriesID := range all[:min(len(all), maxNewShorts)] {
		if !media.ValidHongguoID(seriesID) || taken[seriesID] {
			continue
		}
		taken[seriesID] = true
		if _, err := s.ensureShort(ctx, seriesID, "", PriorityRoutine, false); err != nil {
			return err
		}
	}
	for _, file := range files {
		complete := true
		for _, seriesID := range file.ids {
			if !taken[seriesID] {
				complete = false
				break
			}
		}
		if complete && file.etag != "" {
			s.short.set(file.path, file.etag)
		}
	}
	if len(all) > maxNewShorts {
		s.Log.Info("站点地图里的新短剧超过每天的上限，剩下的明天再收", "found", len(all), "taken", maxNewShorts)
	}
	return nil
}

// ShortRecord 是 cmd/hongguo-seed 快照文件里的一行：一部短剧的资料，或者一个确认不存在的 ID。
type ShortRecord struct {
	ID      string                   `json:"id"`
	Missing bool                     `json:"missing,omitempty"`
	Form    string                   `json:"form,omitempty"`
	Series  *providers.HongguoSeries `json:"series,omitempty"`
}

// ImportShort 把快照里的一行写进作品库：已有的更新，没有的新建。不存在的 ID 也建一行并标记 not_found，
// 这样线上对比站点地图时不会把它们当成新剧再抓一遍。
func (s *Service) ImportShort(ctx context.Context, record ShortRecord) error {
	id, _, err := s.Media.EnsureHongguo(ctx, record.ID)
	if err != nil {
		return err
	}
	if record.Missing || record.Series == nil {
		return s.markShortMissing(ctx, id)
	}
	m, err := s.Media.Get(ctx, id)
	if err != nil {
		return err
	}
	return s.SaveShort(ctx, m, record.Series, record.Form)
}

func uniqueIDs(ids []int64, size int) []int64 {
	seen := map[int64]bool{}
	list := []int64{}
	for _, id := range ids {
		if !seen[id] && len(list) < size {
			seen[id] = true
			list = append(list, id)
		}
	}
	return list
}

package ingest

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"moovie/internal/jobs"
	"moovie/internal/media"
	"moovie/internal/providers"
)

func (s *Service) enrichTMDB(ctx context.Context, m media.Media, started time.Time, patch media.Patch) error {
	kind := "tv"
	if m.Type == media.TypeMovie {
		kind = "movie"
	}
	tmdbID := 0
	if storedKind, storedID, ok := strings.Cut(m.TMDBID, "/"); ok {
		tmdbID, _ = strconv.Atoi(storedID)
		kind = storedKind
	}
	if tmdbID <= 0 && m.IMDbID != "" {
		foundID, foundKind, err := s.TMDB.FindByIMDb(ctx, m.IMDbID)
		if err == nil {
			tmdbID, kind = foundID, foundKind
		} else if !errors.Is(err, providers.ErrNotFound) {
			return err
		}
	}
	if tmdbID <= 0 && m.Title == "" {
		return nil // 只有豆瓣 ID，暂时没有能查 TMDB 的资料。
	}
	searchedByTitle := tmdbID <= 0
	if searchedByTitle {
		base, _ := media.SplitSeason(m.Title)
		year := m.Year
		if kind == "tv" && m.Season > 1 {
			year = 0 // 搜索接口的年份是整部剧的首播年。
		}
		results, err := s.TMDB.Search(ctx, kind, base, year)
		if err != nil {
			return err
		}
		for _, result := range results {
			if result.ID <= 0 || !matchesTMDBTitle(m, result.Title, result.OriginalTitle) {
				continue
			}
			if tmdbID > 0 && tmdbID != result.ID {
				return providers.ErrNotFound // 同名候选不唯一，不建立关联。
			}
			tmdbID = result.ID
		}
		if tmdbID <= 0 {
			return providers.ErrNotFound
		}
	}
	details, err := s.TMDB.Details(ctx, kind, tmdbID)
	if err != nil {
		return err
	}
	if m.IMDbID != "" && details.IMDbID != "" && m.IMDbID != details.IMDbID {
		return providers.ErrNotFound
	}
	season := matchingTMDBSeason(m, details)
	released := details.ReleaseOf(m.Season)
	if kind == "tv" && season == nil {
		released = nil
	}
	if searchedByTitle {
		if !matchesTMDBTitle(m, details.Title, details.OriginalTitle) || (kind == "tv" && season == nil) {
			return providers.ErrNotFound
		}
		if m.Year > 0 && (released == nil || released.Year() != m.Year) {
			return providers.ErrNotFound
		}
	}
	m.TMDBID = kind + "/" + strconv.Itoa(tmdbID)
	if err := errors.Join(s.Media.SetExternalID(ctx, m.ID, media.ProviderTMDB, m.TMDBID),
		s.Media.SetExternalID(ctx, m.ID, media.ProviderIMDb, details.IMDbID)); err != nil {
		return err
	}

	if kind == "tv" {
		patch.Set("media_type", media.TypeTV)
	}
	if m.Title == "" && (kind == "movie" || season != nil) {
		title := details.Title
		if title != "" && kind == "tv" && m.Season > 1 {
			title += " 第" + strconv.Itoa(m.Season) + "季"
		}
		patch.Set("title", title)
	}
	if released != nil {
		patch.Set("year", released.Year())
	}
	if _, supplied := patch["original_title"]; !supplied {
		patch.Set("original_title", details.OriginalTitle)
	}
	if _, supplied := patch["summary"]; !supplied {
		patch.Set("summary", details.Overview)
	}
	patch.Set("backdrops", details.Backdrops)
	patch.Set("rating_tmdb", details.Rating)
	patch.Set("votes_tmdb", details.Votes)
	patch.Set("duration_minutes", details.Runtime)
	patch.Set("poster", details.Poster)
	if kind == "tv" {
		if err := s.saveTMDBSeason(ctx, m, started, details, season); err != nil {
			return err
		}
	}
	if err := s.Media.SetRelease(ctx, m, released, details.Popularity); err != nil {
		return err
	}
	return s.Media.AddAliases(ctx, m.ID, details.Title, details.OriginalTitle)
}

func matchesTMDBTitle(m media.Media, candidates ...string) bool {
	for _, title := range []string{m.Title, m.OriginalTitle} {
		base, _ := media.SplitSeason(title)
		name := media.Normalize(base)
		for _, candidate := range candidates {
			if name != "" && name == media.Normalize(candidate) {
				return true
			}
		}
	}
	return false
}

func matchingTMDBSeason(m media.Media, details *providers.TMDBDetails) *providers.TMDBSeason {
	if details.Kind != "tv" || m.Season <= 0 {
		return nil
	}
	for i := range details.Seasons {
		season := &details.Seasons[i]
		if season.Number != m.Season {
			continue
		}
		released := details.ReleaseOf(m.Season)
		if released != nil && m.Year > 0 && released.Year() != m.Year {
			return nil
		}
		// 综艺和动漫常有不同的分季方式。除了季号，还需要当季的片名或首播年与剧名互相印证。
		if m.Type == media.TypeShow || m.Type == media.TypeAnime {
			exactSeasonTitle := m.Title != "" && media.Normalize(m.Title) == media.Normalize(season.Name)
			sameYearAndShow := released != nil && released.Year() == m.Year &&
				matchesTMDBTitle(m, details.Title, details.OriginalTitle)
			for _, other := range details.Seasons {
				if other.Number > 0 && other.Number != m.Season && other.AirDate != nil && other.AirDate.Year() == m.Year {
					sameYearAndShow = false // 同年多季时要靠明确的季名区分。
					break
				}
			}
			if !exactSeasonTitle && !sameYearAndShow {
				return nil
			}
		}
		return season
	}
	return nil
}

// RefreshTMDBSeries 检查已到期的季。建档期间已经查过的，会在出队后跳过。
func (s *Service) RefreshTMDBSeries(ctx context.Context, id int64, _ jobs.Job) error {
	if !s.TMDB.Enabled() {
		return jobs.ErrSkip
	}
	started := time.Now()
	m, err := s.Media.Get(ctx, id)
	if errors.Is(err, media.ErrNotFound) {
		return jobs.ErrSkip
	}
	if err != nil {
		return err
	}
	kind, number, _ := strings.Cut(m.TMDBID, "/")
	tmdbID, _ := strconv.Atoi(number)
	if kind != "tv" || tmdbID <= 0 || !m.IsSeries() || m.IsShort() || m.Hidden ||
		m.MetadataStatus == "not_found" || m.SeriesStatus == "ended" ||
		!m.TMDBSeasonCheckDue(started) {
		return jobs.ErrSkip
	}
	details, err := s.TMDB.Details(ctx, kind, tmdbID)
	if errors.Is(err, providers.ErrNotFound) {
		return s.Media.SaveTMDBSeason(ctx, m, started, media.TMDBSeasonUpdate{Unmatched: true})
	}
	if err != nil {
		return err
	}
	var season *providers.TMDBSeason
	if m.IMDbID == "" || details.IMDbID == "" || m.IMDbID == details.IMDbID {
		season = matchingTMDBSeason(m, details)
	}
	return s.saveTMDBSeason(ctx, m, started, details, season)
}

func (s *Service) saveTMDBSeason(ctx context.Context, m media.Media, started time.Time, details *providers.TMDBDetails, season *providers.TMDBSeason) error {
	update := media.TMDBSeasonUpdate{Unmatched: true}
	if season != nil {
		episodes, err := s.TMDB.Season(ctx, details.ID, m.Season)
		if err != nil && !errors.Is(err, providers.ErrNotFound) {
			return err
		}
		if err == nil {
			update.Unmatched = false
			update.EpisodeCount = season.EpisodeCount
			update.Status = tmdbSeasonStatus(details, *season, episodes)
			for _, episode := range episodes {
				update.Units = append(update.Units, media.Unit{Season: m.Season, Episode: episode.Number,
					Title: episode.Title, AirDate: episode.AirDate, RuntimeMinutes: episode.Runtime})
			}
		}
	}
	return s.Media.SaveTMDBSeason(ctx, m, started, update)
}

func tmdbSeasonStatus(details *providers.TMDBDetails, season providers.TMDBSeason, episodes []providers.TMDBEpisode) string {
	if details.Ended {
		return "ended"
	}
	today := media.Today()
	for _, later := range details.Seasons {
		if later.Number > season.Number && later.AirDate != nil && !later.AirDate.After(today) {
			return "ended"
		}
	}
	// 整部剧还在播时，明确写作季终且已经播出的最后一集也可以收尾。
	var last providers.TMDBEpisode
	for _, episode := range episodes {
		if episode.Number > last.Number {
			last = episode
		}
	}
	if last.Type == "finale" && last.Number >= season.EpisodeCount &&
		last.AirDate != nil && !last.AirDate.After(today) {
		return "ended"
	}
	return "ongoing"
}

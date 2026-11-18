package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// TMDB 是 TMDB 客户端。没有配置 Token 时所有功能自动关闭。
type TMDB struct {
	HTTP  *http.Client
	Base  string // 默认 https://api.themoviedb.org
	Token string
}

// NewTMDB 创建 TMDB 客户端。
func NewTMDB(client *http.Client, token string) *TMDB {
	return &TMDB{HTTP: client, Base: "https://api.themoviedb.org", Token: token}
}

// Enabled 表示是否配置了 Token。
func (t *TMDB) Enabled() bool { return t != nil && t.Token != "" }

const tmdbImageBase = "https://image.tmdb.org/t/p/original"

// TMDBDetails 是 TMDB 上一部电影或一部剧的资料。
type TMDBDetails struct {
	ID            int
	Kind          string // movie 或 tv
	Title         string
	OriginalTitle string
	Overview      string
	Poster        string
	Backdrops     []string
	Rating        float64
	Votes         int
	Runtime       int
	Ended         bool
	IMDbID        string
	Seasons       []TMDBSeason
}

// TMDBSeason 是一季的概况。
type TMDBSeason struct {
	Number       int
	EpisodeCount int
}

// TMDBEpisode 是一集的资料。
type TMDBEpisode struct {
	Number  int
	Title   string
	AirDate *time.Time
	Runtime int
}

// FindByIMDb 用 IMDb ID 找到 TMDB 上的条目。
func (t *TMDB) FindByIMDb(ctx context.Context, imdbID string) (id int, kind string, err error) {
	var response struct {
		Movies []struct {
			ID int `json:"id"`
		} `json:"movie_results"`
		TV []struct {
			ID int `json:"id"`
		} `json:"tv_results"`
	}
	if err := t.getJSON(ctx, fmt.Sprintf("/3/find/%s?external_source=imdb_id&language=zh-CN", url.PathEscape(imdbID)), &response); err != nil {
		return 0, "", err
	}
	switch {
	case len(response.Movies) > 0:
		return response.Movies[0].ID, "movie", nil
	case len(response.TV) > 0:
		return response.TV[0].ID, "tv", nil
	}
	return 0, "", ErrNotFound
}

// Search 按片名和年份搜索，返回第一个结果。kind 取 movie 或 tv。
func (t *TMDB) Search(ctx context.Context, kind, title string, year int) (int, error) {
	query := url.Values{"query": {title}, "language": {"zh-CN"}}
	if year > 0 {
		if kind == "movie" {
			query.Set("year", strconv.Itoa(year))
		} else {
			query.Set("first_air_date_year", strconv.Itoa(year))
		}
	}
	var response struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := t.getJSON(ctx, "/3/search/"+kind+"?"+query.Encode(), &response); err != nil {
		return 0, err
	}
	if len(response.Results) == 0 {
		return 0, ErrNotFound
	}
	return response.Results[0].ID, nil
}

// Details 读取一个条目的资料，连同剧照和 IMDb ID。
func (t *TMDB) Details(ctx context.Context, kind string, id int) (*TMDBDetails, error) {
	var response struct {
		Title         string  `json:"title"`
		Name          string  `json:"name"`
		OriginalTitle string  `json:"original_title"`
		OriginalName  string  `json:"original_name"`
		Overview      string  `json:"overview"`
		PosterPath    string  `json:"poster_path"`
		VoteAverage   float64 `json:"vote_average"`
		VoteCount     int     `json:"vote_count"`
		Runtime       int     `json:"runtime"`
		EpisodeRun    []int   `json:"episode_run_time"`
		Status        string  `json:"status"`
		IMDbID        string  `json:"imdb_id"`
		Seasons       []struct {
			Number       int `json:"season_number"`
			EpisodeCount int `json:"episode_count"`
		} `json:"seasons"`
		Images struct {
			Backdrops []struct {
				Path string `json:"file_path"`
			} `json:"backdrops"`
		} `json:"images"`
		ExternalIDs struct {
			IMDbID string `json:"imdb_id"`
		} `json:"external_ids"`
	}
	endpoint := fmt.Sprintf("/3/%s/%d?language=zh-CN&append_to_response=images,external_ids&include_image_language=zh,en,null", kind, id)
	if err := t.getJSON(ctx, endpoint, &response); err != nil {
		return nil, err
	}
	details := &TMDBDetails{
		ID: id, Kind: kind, Title: response.Title, OriginalTitle: response.OriginalTitle, Overview: response.Overview,
		Rating: response.VoteAverage, Votes: response.VoteCount, Runtime: response.Runtime, IMDbID: response.IMDbID,
		Ended: response.Status == "Ended" || response.Status == "Canceled" || response.Status == "Released",
	}
	if kind == "tv" {
		details.Title, details.OriginalTitle = response.Name, response.OriginalName
		if len(response.EpisodeRun) > 0 {
			details.Runtime = response.EpisodeRun[0]
		}
	}
	if details.IMDbID == "" {
		details.IMDbID = response.ExternalIDs.IMDbID
	}
	if response.PosterPath != "" {
		details.Poster = tmdbImageBase + response.PosterPath
	}
	for index, image := range response.Images.Backdrops {
		if index >= 8 {
			break
		}
		details.Backdrops = append(details.Backdrops, tmdbImageBase+image.Path)
	}
	for _, season := range response.Seasons {
		details.Seasons = append(details.Seasons, TMDBSeason{Number: season.Number, EpisodeCount: season.EpisodeCount})
	}
	return details, nil
}

// Season 读取一季的分集列表。
func (t *TMDB) Season(ctx context.Context, id, season int) ([]TMDBEpisode, error) {
	var response struct {
		Episodes []struct {
			Number  int    `json:"episode_number"`
			Name    string `json:"name"`
			AirDate string `json:"air_date"`
			Runtime int    `json:"runtime"`
		} `json:"episodes"`
	}
	if err := t.getJSON(ctx, fmt.Sprintf("/3/tv/%d/season/%d?language=zh-CN", id, season), &response); err != nil {
		return nil, err
	}
	episodes := make([]TMDBEpisode, 0, len(response.Episodes))
	for _, entry := range response.Episodes {
		episode := TMDBEpisode{Number: entry.Number, Title: entry.Name, Runtime: entry.Runtime}
		if parsed, err := time.Parse("2006-01-02", entry.AirDate); err == nil {
			episode.AirDate = &parsed
		}
		episodes = append(episodes, episode)
	}
	return episodes, nil
}

// Trending 读取 TMDB 一周的趋势榜（第一页，20 条），按名次返回条目编号。kind 取 movie 或 tv。
func (t *TMDB) Trending(ctx context.Context, kind string) ([]int, error) {
	var response struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := t.getJSON(ctx, "/3/trending/"+kind+"/week?language=zh-CN", &response); err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(response.Results))
	for _, item := range response.Results {
		if item.ID > 0 {
			ids = append(ids, item.ID)
		}
	}
	return ids, nil
}

func (t *TMDB) getJSON(ctx context.Context, path string, destination any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Base+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+t.Token)
	request.Header.Set("Accept", "application/json")
	return doJSON(t.HTTP, request, "TMDB", destination)
}

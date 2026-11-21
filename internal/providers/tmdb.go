package providers

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// TMDB 是 TMDB 客户端。没有配置 Token 时所有功能自动关闭。
type TMDB struct {
	HTTP  *http.Client
	Base  string // 默认 https://api.themoviedb.org
	Files string // 每日导出文件的地址，默认 https://files.tmdb.org
	Token string
}

// NewTMDB 创建 TMDB 客户端。
func NewTMDB(client *http.Client, token string) *TMDB {
	return &TMDB{HTTP: client, Base: "https://api.themoviedb.org", Files: "https://files.tmdb.org", Token: token}
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
	Released      *time.Time // 电影的上映日期，剧集的首播日期
	Popularity    float64    // TMDB 的近期关注度
}

// TMDBSeason 是一季的概况。
type TMDBSeason struct {
	Number       int
	EpisodeCount int
	AirDate      *time.Time
}

// ReleaseOf 返回作品库里一行对应的上映日期：电影就是上映日期；剧集每季一行，取这一季的首播日期，
// 第一季没有单独的日期时用整部剧的首播日期。查不到时返回 nil。
func (d *TMDBDetails) ReleaseOf(season int) *time.Time {
	if d.Kind != "tv" {
		return d.Released
	}
	for _, entry := range d.Seasons {
		if entry.Number == season && entry.AirDate != nil {
			return entry.AirDate
		}
	}
	if season <= 1 {
		return d.Released
	}
	return nil
}

func parseDate(value string) *time.Time {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return nil
	}
	return &parsed
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
		ReleaseDate   string  `json:"release_date"`
		FirstAirDate  string  `json:"first_air_date"`
		Popularity    float64 `json:"popularity"`
		Seasons       []struct {
			Number       int    `json:"season_number"`
			EpisodeCount int    `json:"episode_count"`
			AirDate      string `json:"air_date"`
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
		Ended:      response.Status == "Ended" || response.Status == "Canceled" || response.Status == "Released",
		Released:   parseDate(response.ReleaseDate),
		Popularity: response.Popularity,
	}
	if kind == "tv" {
		details.Title, details.OriginalTitle = response.Name, response.OriginalName
		details.Released = parseDate(response.FirstAirDate)
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
		details.Seasons = append(details.Seasons, TMDBSeason{Number: season.Number, EpisodeCount: season.EpisodeCount, AirDate: parseDate(season.AirDate)})
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

// Popularity 读取 TMDB 某一天的导出文件（电影或剧集的全部条目，每行一个 JSON），对每个条目调用 visit。
// 导出文件每天 UTC 早上生成、公开下载，不占 API 配额；那一天的文件还没生成时返回 ErrNotFound。kind 取 movie 或 tv。
func (t *TMDB) Popularity(ctx context.Context, kind string, day time.Time, visit func(id int, popularity float64)) error {
	prefix := map[string]string{"movie": "movie_ids", "tv": "tv_series_ids"}[kind]
	if prefix == "" {
		return fmt.Errorf("未知的 TMDB 类型: %q", kind)
	}
	day = day.UTC()
	name := fmt.Sprintf("%s_%02d_%02d_%d.json.gz", prefix, day.Month(), day.Day(), day.Year())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Files+"/p/exports/"+name, nil)
	if err != nil {
		return err
	}
	response, err := t.HTTP.Do(request)
	if err != nil {
		return fmt.Errorf("下载 TMDB 导出文件: %w", err)
	}
	defer response.Body.Close()
	// 文件放在 S3 上，不存在时返回 403 或 404。
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusForbidden {
		return ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 TMDB 导出文件 %s: HTTP %d", name, response.StatusCode)
	}
	// 有的服务器会把 .gz 再标成 Content-Encoding: gzip，Go 已经自动解开了；按开头两个字节判断要不要再解一次。
	body := bufio.NewReaderSize(io.LimitReader(response.Body, 1<<30), 64<<10)
	var reader io.Reader = body
	if magic, _ := body.Peek(2); bytes.Equal(magic, []byte{0x1f, 0x8b}) {
		unzipped, err := gzip.NewReader(body)
		if err != nil {
			return fmt.Errorf("解压 TMDB 导出文件: %w", err)
		}
		defer unzipped.Close()
		reader = unzipped
	}
	lines := bufio.NewScanner(reader)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for lines.Scan() {
		var entry struct {
			ID         int     `json:"id"`
			Popularity float64 `json:"popularity"`
		}
		if json.Unmarshal(lines.Bytes(), &entry) == nil && entry.ID > 0 {
			visit(entry.ID, entry.Popularity)
		}
	}
	if err := lines.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("读取 TMDB 导出文件: %w", err)
	}
	return nil
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

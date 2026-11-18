// Package report 生成月度观影报告。
//
// 报告按需生成：当月的报告每次打开都重新计算；过去月份的报告第一次打开时生成并保存。
package report

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"moovie/internal/db"
)

// Report 是一个月的观影报告。
type Report struct {
	YearMonth     string      `json:"year_month"`
	WatchedCount  int         `json:"watched_count"`
	TotalMinutes  int         `json:"total_minutes"`
	AverageRating float64     `json:"average_rating"`
	Genres        []GenreStat `json:"genres"`
	Top           *TopItem    `json:"top,omitempty"`
	ActiveDays    int         `json:"active_days"`
	LongestStreak int         `json:"longest_streak"`
	Percentile    int         `json:"percentile"` // 看得比百分之多少的用户多
	PersonaTitle  string      `json:"persona_title"`
	PersonaLine   string      `json:"persona_line"`
	Posters       []string    `json:"posters"`
}

// GenreStat 是某个类型看了几部。
type GenreStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// TopItem 是当月评分最高的一部。
type TopItem struct {
	MediaID int64  `json:"media_id"`
	Title   string `json:"title"`
	Poster  string `json:"poster"`
	Rating  int    `json:"rating"`
}

// Hours 返回总时长（小时，向下取整）。
func (r Report) Hours() int { return r.TotalMinutes / 60 }

// Label 返回"2026 年 10 月"这样的标题。
func (r Report) Label() string {
	month, err := time.Parse("2006-01", r.YearMonth)
	if err != nil {
		return r.YearMonth
	}
	return month.Format("2006 年 1 月")
}

var monthPattern = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)

// ErrInvalidMonth 表示月份格式不对或者是未来的月份。
var ErrInvalidMonth = errors.New("月份不正确")

// Service 生成和读取报告。
type Service struct {
	pool *db.Pool
	now  func() time.Time
}

// NewService 创建 Service。
func NewService(pool *db.Pool) *Service { return &Service{pool: pool, now: time.Now} }

func (s *Service) bounds(yearMonth string) (start, end time.Time, err error) {
	if !monthPattern.MatchString(yearMonth) {
		return start, end, ErrInvalidMonth
	}
	start, _ = time.ParseInLocation("2006-01", yearMonth, time.Local)
	end = start.AddDate(0, 1, 0)
	if start.After(s.now()) {
		return start, end, ErrInvalidMonth
	}
	return start, end, nil
}

// Get 计算某个月的报告。每次都现算：几条带索引的查询，比存一份再处理"数据改了要重算"简单。
func (s *Service) Get(ctx context.Context, userID int64, yearMonth string) (Report, error) {
	start, end, err := s.bounds(yearMonth)
	if err != nil {
		return Report{}, err
	}
	report := Report{YearMonth: yearMonth, Genres: []GenreStat{}, Posters: []string{}}

	if err := s.pool.QueryRow(ctx, `SELECT count(*),
COALESCE(sum(m.duration_minutes * CASE WHEN m.media_type = 'movie' THEN 1 ELSE GREATEST(m.episode_count, 1) END), 0),
COALESCE(avg(um.rating) FILTER (WHERE um.rating > 0), 0)
FROM user_movies um JOIN media m ON m.id = um.media_id
WHERE um.user_id = $1 AND um.status = 'watched' AND um.updated_at >= $2 AND um.updated_at < $3`, userID, start, end).
		Scan(&report.WatchedCount, &report.TotalMinutes, &report.AverageRating); err != nil {
		return report, err
	}

	rows, err := s.pool.Query(ctx, `SELECT genre, count(*) FROM user_movies um JOIN media m ON m.id = um.media_id, unnest(m.genres) AS genre
WHERE um.user_id = $1 AND um.status = 'watched' AND um.updated_at >= $2 AND um.updated_at < $3
GROUP BY genre ORDER BY count(*) DESC, genre LIMIT 6`, userID, start, end)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var stat GenreStat
		if err := rows.Scan(&stat.Name, &stat.Count); err != nil {
			rows.Close()
			return report, err
		}
		report.Genres = append(report.Genres, stat)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}

	var top TopItem
	err = s.pool.QueryRow(ctx, `SELECT m.id, m.title, m.poster, um.rating FROM user_movies um JOIN media m ON m.id = um.media_id
WHERE um.user_id = $1 AND um.status = 'watched' AND um.rating > 0 AND um.updated_at >= $2 AND um.updated_at < $3
ORDER BY um.rating DESC, um.updated_at DESC LIMIT 1`, userID, start, end).Scan(&top.MediaID, &top.Title, &top.Poster, &top.Rating)
	if err == nil {
		report.Top = &top
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return report, err
	}

	rows, err = s.pool.Query(ctx, `SELECT m.poster FROM user_movies um JOIN media m ON m.id = um.media_id
WHERE um.user_id = $1 AND um.status = 'watched' AND m.poster <> '' AND um.updated_at >= $2 AND um.updated_at < $3
ORDER BY um.rating DESC, um.updated_at DESC LIMIT 12`, userID, start, end)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var poster string
		if err := rows.Scan(&poster); err != nil {
			rows.Close()
			return report, err
		}
		report.Posters = append(report.Posters, poster)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}

	days, err := s.days(ctx, userID, start, end)
	if err != nil {
		return report, err
	}
	report.ActiveDays, report.LongestStreak = len(days), longestStreak(days)

	// 排名：这个月看过的数量超过了百分之多少的活跃用户。
	if report.WatchedCount > 0 {
		var fewer, total int
		if err := s.pool.QueryRow(ctx, `WITH counts AS (
  SELECT user_id, count(*) AS n FROM user_movies
  WHERE status = 'watched' AND updated_at >= $1 AND updated_at < $2 GROUP BY user_id
) SELECT count(*) FILTER (WHERE n < $3), count(*) FROM counts`, start, end, report.WatchedCount).Scan(&fewer, &total); err != nil {
			return report, err
		}
		if total > 1 {
			report.Percentile = fewer * 100 / (total - 1)
		}
	}
	report.PersonaTitle, report.PersonaLine = persona(report)

	return report, nil
}

func (s *Service) days(ctx context.Context, userID int64, start, end time.Time) ([]time.Time, error) {
	rows, err := s.pool.Query(ctx, `SELECT day FROM watch_days WHERE user_id = $1 AND day >= $2::date AND day < $3::date ORDER BY day`,
		userID, start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	days := []time.Time{}
	for rows.Next() {
		var day time.Time
		if err := rows.Scan(&day); err != nil {
			return nil, err
		}
		days = append(days, day)
	}
	return days, rows.Err()
}

// longestStreak 返回最长连续天数。days 必须已按日期升序排列。
func longestStreak(days []time.Time) int {
	longest, current := 0, 0
	for index, day := range days {
		if index > 0 && day.Sub(days[index-1]) == 24*time.Hour {
			current++
		} else {
			current = 1
		}
		longest = max(longest, current)
	}
	return longest
}

var personas = map[string][2]string{
	"悬疑":  {"线索收集者", "别人看剧情，你在找破绽。"},
	"犯罪":  {"线索收集者", "别人看剧情，你在找破绽。"},
	"爱情":  {"浪漫主义者", "你相信每一次对视都有后续。"},
	"喜剧":  {"快乐供应商", "这个月，你把笑声存了不少。"},
	"科幻":  {"星际旅人", "你的片单比现实走得更远。"},
	"动画":  {"次元旅行者", "二维的世界里，你找到了三维的感动。"},
	"剧情":  {"故事品鉴家", "你愿意花两个小时，认真听一个人讲完他的一生。"},
	"动作":  {"肾上腺素玩家", "没有什么是一场追车戏解决不了的。"},
	"恐怖":  {"夜行勇者", "灯都不开，胆子不小。"},
	"纪录片": {"现实观察员", "比起虚构，你更想知道真的发生了什么。"},
	"奇幻":  {"造梦者", "你偏爱那些不讲道理的世界。"},
	"家庭":  {"温度收藏家", "你总能在平常日子里看出味道。"},
}

func persona(report Report) (title, line string) {
	if report.WatchedCount == 0 {
		return "潜水观察员", "这个月你很安静，好片子还在等你。"
	}
	if len(report.Genres) > 0 {
		if found, ok := personas[report.Genres[0].Name]; ok {
			return found[0], found[1]
		}
	}
	if report.WatchedCount >= 20 {
		return "片库吞噬者", "一个月二十部起步，屏幕都要认识你了。"
	}
	return "杂食影迷", "什么都看一点，什么都不错过。"
}

// Months 返回用户有观影记录的月份，新的在前。
func (s *Service) Months(ctx context.Context, userID int64, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT to_char(updated_at, 'YYYY-MM') AS ym FROM user_movies
WHERE user_id = $1 AND status = 'watched' ORDER BY ym DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	months := []string{}
	for rows.Next() {
		var month string
		if err := rows.Scan(&month); err != nil {
			return nil, err
		}
		months = append(months, month)
	}
	return months, rows.Err()
}

package app

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"

	"moovie/internal/media"
	"moovie/internal/playback"
)

// CardView 是海报卡片的展示数据。
type CardView struct {
	media.Card
	Href  string
	Sub   string
	Badge string
}

// WideView 是横版卡片（继续观看）的展示数据。
type WideView struct {
	media.Card
	Href string
	Sub  string
	Pct  int
	At   int64 // 最近一次播放的时间（毫秒时间戳），浏览器用它把本地的加密历史按时间插进来
}

// RowView 是首页一行内容的展示数据。
type RowView struct {
	Title string
	Sub   string
	More  string
	Class string
	Cards []CardView
	Wide  []WideView
}

func mediaHref(id int64) string { return "/m/" + strconv.FormatInt(id, 10) }

func playHref(id int64, season, episode int) string {
	return fmt.Sprintf("/play/%d?s=%d&e=%d", id, season, episode)
}

// defaultSub 是卡片默认的副标题："2026 · 悬疑"；短剧是"短剧 · 甜宠"，和电影、剧集放在一起（搜索结果、片单）时一眼能分出来。
func defaultSub(c media.Card) string {
	if c.IsShort() {
		for _, genre := range c.Genres {
			if !media.IsShortForm(genre) {
				return "短剧 · " + genre
			}
		}
		return "短剧"
	}
	parts := []string{}
	if c.Year > 0 {
		parts = append(parts, strconv.Itoa(c.Year))
	}
	if len(c.Genres) > 0 {
		parts = append(parts, c.Genres[0])
	} else if label := c.TypeLabel(); label != "" {
		parts = append(parts, label)
	}
	return strings.Join(parts, " · ")
}

func cardViews(cards []media.Card) []CardView {
	views := make([]CardView, 0, len(cards))
	for _, card := range cards {
		views = append(views, CardView{Card: card, Href: mediaHref(card.ID), Sub: defaultSub(card)})
	}
	return views
}

// remaining 把剩余时间写成"剩 18 分钟"。
func remaining(position, duration float64) string {
	if duration <= 0 {
		return ""
	}
	left := int((duration - position) / 60)
	if left <= 0 {
		return "即将看完"
	}
	if left >= 60 {
		return fmt.Sprintf("剩 %d 小时 %d 分", left/60, left%60)
	}
	return fmt.Sprintf("剩 %d 分钟", left)
}

func wideViews(recent []playback.Recent) []WideView {
	views := make([]WideView, 0, len(recent))
	for _, item := range recent {
		sub := remaining(item.Position, item.Duration)
		if item.Card.Type != media.TypeMovie {
			sub = strings.TrimSuffix(fmt.Sprintf("第 %d 集 · %s", item.Episode, sub), " · ")
		}
		pct := 0
		if item.Duration > 0 {
			pct = int(min(100, item.Position/item.Duration*100))
		}
		views = append(views, WideView{Card: item.Card, Href: playHref(item.Card.ID, item.Season, item.Episode), Sub: sub, Pct: pct, At: item.At.UnixMilli()})
	}
	return views
}

// episodeStatus 是"更新至第 8 集 / 全 24 集"这样的状态文字。
func episodeStatus(m media.Media, aired int) string {
	switch {
	case m.Type == media.TypeMovie:
		if m.DurationMinutes > 0 {
			return fmt.Sprintf("%d 分钟", m.DurationMinutes)
		}
		return ""
	case m.IsShort() && m.SeriesStatus == "ongoing" && m.EpisodeCount > 0:
		// 短剧没有分集的播出日期，集数就是已经上线的集数。
		return fmt.Sprintf("更新至第 %d 集", m.EpisodeCount)
	case m.SeriesStatus == "ongoing" && aired > 0 && (m.EpisodeCount == 0 || aired < m.EpisodeCount):
		return fmt.Sprintf("更新至第 %d 集", aired)
	case m.EpisodeCount > 0:
		return fmt.Sprintf("全 %d 集", m.EpisodeCount)
	}
	return ""
}

// viewerID 返回当前访客的用户 ID，没登录时返回 0。
func viewerID(c *gin.Context) int64 {
	if user := auth.Current(c); user != nil {
		return user.ID
	}
	return 0
}

// visitorKey 返回限流用的访客标识：登录用户用 ID，没登录的用 IP。
func visitorKey(c *gin.Context) string {
	if id := viewerID(c); id != 0 {
		return "u" + strconv.FormatInt(id, 10)
	}
	return "ip" + c.ClientIP()
}

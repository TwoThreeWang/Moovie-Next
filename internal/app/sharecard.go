package app

import (
	"strconv"
	"strings"

	"moovie/internal/collections"
	"moovie/internal/media"
	"moovie/internal/report"
	"moovie/internal/social"
	"moovie/internal/ui"
)

// ShareCard 是分享图片的内容。图片由浏览器用 canvas 画（web/static/js/core/sharecard.js），
// 服务器只提供文字和图片地址：省掉中文字体和图片处理，也不占服务器资源。
type ShareCard struct {
	Kind    string      `json:"kind"` // media、report、list、comment
	Kicker  string      `json:"kicker,omitempty"`
	Title   string      `json:"title"`
	Sub     string      `json:"sub,omitempty"`
	Badge   string      `json:"badge,omitempty"`
	Text    string      `json:"text,omitempty"`
	Author  string      `json:"author,omitempty"`
	Poster  string      `json:"poster,omitempty"`
	Posters []string    `json:"posters,omitempty"`
	Stats   [][2]string `json:"stats,omitempty"`
	URL     string      `json:"url"`
	Name    string      `json:"name"` // 保存时的文件名
	Site    string      `json:"site"`
}

func (a *App) mediaCard(m media.Media) ShareCard {
	kicker := m.TypeLabel()
	if m.Year > 0 {
		kicker += " · " + strconv.Itoa(m.Year)
	}
	card := ShareCard{Kind: "media", Kicker: kicker, Title: m.Title, Sub: strings.Join(firstN(m.Genres, 3), " / "),
		Text: clip(m.Summary, 90), Poster: ui.Image(m.Poster, "l"), URL: a.absolute(mediaHref(m.ID)),
		Name: "moovie-" + strconv.FormatInt(m.ID, 10), Site: a.Config.SiteName}
	switch {
	case m.RatingDouban > 0:
		card.Badge = "豆瓣 " + strconv.FormatFloat(m.RatingDouban, 'f', 1, 64)
	case m.RatingTMDB > 0:
		card.Badge = "TMDB " + strconv.FormatFloat(m.RatingTMDB, 'f', 1, 64)
	}
	return card
}

// reportCard 是月度报告的卡片。主页没公开时，二维码指向首页（别人打不开报告）。
func (a *App) reportCard(profile social.Profile, monthly report.Report, path string) ShareCard {
	target := a.absolute(path)
	if !profile.IsPublic {
		target = a.absolute("/")
	}
	posters := make([]string, 0, 5)
	for _, poster := range firstN(monthly.Posters, 5) {
		posters = append(posters, ui.Image(poster, "m"))
	}
	return ShareCard{Kind: "report", Kicker: profile.Username + " 的" + monthly.Label() + "观影报告", Title: monthly.PersonaTitle,
		Text: monthly.PersonaLine, Posters: posters, URL: target, Name: "moovie-report-" + monthly.YearMonth, Site: a.Config.SiteName,
		Stats: [][2]string{
			{strconv.Itoa(monthly.WatchedCount), "部看过"}, {strconv.Itoa(monthly.Hours()), "小时"},
			{strconv.Itoa(monthly.ActiveDays), "天有观影"}, {strconv.Itoa(monthly.LongestStreak), "天最长连续"},
		}}
}

func (a *App) listCard(collection collections.Collection, cards []CardView) ShareCard {
	posters := make([]string, 0, 10)
	for _, card := range cards {
		if len(posters) == 10 {
			break
		}
		if image := ui.Image(card.Card.Poster, "m"); image != "" {
			posters = append(posters, image)
		}
	}
	return ShareCard{Kind: "list", Kicker: "片单", Title: collection.Title, Text: clip(collection.Description, 80),
		Sub: "共 " + strconv.Itoa(collection.Count) + " 部", Posters: posters, URL: a.absolute("/list/" + collection.Slug),
		Name: "moovie-list-" + collection.Slug, Site: a.Config.SiteName}
}

func firstN[T any](items []T, n int) []T {
	if len(items) > n {
		return items[:n]
	}
	return items
}

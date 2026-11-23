package media_test

import (
	"testing"

	"moovie/internal/media"
)

// 集号编码要和 web/static/js/lib/issue.js 一致。
func TestEpisodeLabel(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		episode int
		want    string
	}{
		{media.TypeTV, 3, "第 3 集"},
		{media.TypeAnime, 120, "第 120 集"},
		{media.TypeShow, 302, "第 3 期 上"},
		{media.TypeShow, 304, "第 3 期 下"},
		{media.TypeShow, 211, "第 2 期 加更"},
		{media.TypeShow, 121, "第 1 期 加更 2"},
		{media.TypeShow, 244, "第 2 期 日记 下"},
		{media.TypeShow, 501, "第 5 期"},
		{media.TypeShow, 500, "第 5 期"},
		{media.TypeShow, 2, "先导片 上"},
		{media.TypeShow, 1, "先导片"},
	} {
		if got := media.EpisodeLabel(tc.kind, tc.episode); got != tc.want {
			t.Errorf("EpisodeLabel(%s, %d) = %q，期望 %q", tc.kind, tc.episode, got, tc.want)
		}
	}
	if !media.DecodeIssue(501).Main() || media.DecodeIssue(502).Main() || media.DecodeIssue(511).Main() {
		t.Error("只有正片整期算 Main")
	}
	if media.IssueEpisode(7) != 700 || media.EpisodeUnit(media.TypeShow) != "期" || media.EpisodeUnit(media.TypeTV) != "集" {
		t.Error("IssueEpisode / EpisodeUnit 不对")
	}
	if label := (media.Airing{Card: media.Card{Type: media.TypeShow}, First: 7, Last: 7}).Label(); label != "更新第 7 期" {
		t.Errorf("综艺的更新说明 = %q", label)
	}
}

package providers

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseSubjectPage(t *testing.T) {
	page, err := os.ReadFile("testdata/subject_movie.html")
	if err != nil {
		t.Fatal(err)
	}
	if pageBlocked(page) {
		t.Fatal("正常的详情网页不该被当成验证页")
	}
	movie, err := parseSubjectPage("1292052", page)
	if err != nil {
		t.Fatal(err)
	}
	if movie.Kind != "movie" || movie.Title != "肖申克的救赎" || movie.OriginalTitle != "The Shawshank Redemption" || movie.Year != 1994 ||
		movie.Rating != 9.7 || movie.DurationMinutes != 142 || movie.EpisodesCount != 0 {
		t.Errorf("电影的基本资料不对: %+v", movie)
	}
	if strings.Join(movie.Genres, ",") != "剧情,犯罪" || strings.Join(movie.Countries, ",") != "美国" || strings.Join(movie.AKA, ",") != "月黑高飞(港),刺激1995(台)" ||
		strings.Join(movie.Directors, ",") != "弗兰克·德拉邦特" || strings.Join(movie.Actors, ",") != "蒂姆·罗宾斯,摩根·弗里曼" {
		t.Errorf("类型、地区、又名、导演、演员不对: %+v", movie)
	}
	if movie.Poster != "https://img3.doubanio.com/view/photo/m_ratio_poster/public/p480747492.webp" {
		t.Errorf("海报应该换成接口那一档: %s", movie.Poster)
	}
	if !strings.HasSuffix(movie.Intro, "多年后，他终于重获自由。") || strings.Count(movie.Intro, "\n") != 1 || strings.Contains(movie.Intro, "……") {
		t.Errorf("简介应该取完整的那份，段落之间一个换行: %q", movie.Intro)
	}

	page, err = os.ReadFile("testdata/subject_tv.html")
	if err != nil {
		t.Fatal(err)
	}
	show, err := parseSubjectPage("35588177", page)
	if err != nil {
		t.Fatal(err)
	}
	if show.Kind != "tv" || show.Title != "漫长的季节" || show.OriginalTitle != "" || show.EpisodesCount != 12 || show.DurationMinutes != 45 ||
		show.Year != 2023 || show.Intro != "故事发生在一座东北小城。" {
		t.Errorf("剧集的资料不对: %+v", show)
	}

	if _, err := parseSubjectPage("1", []byte(`<html><title>页面不存在 (豆瓣)</title></html>`)); err == nil {
		t.Error("解析不出片名时应该报错")
	}
	for _, blocked := range []string{`<html><title>豆瓣</title></html>`, `<title>禁止访问</title>`, `<html><body>no title</body></html>`} {
		if !pageBlocked([]byte(blocked)) {
			t.Errorf("应该认出验证页: %s", blocked)
		}
	}
}

// 抖动只往上加：两次请求之间不短于当前间隔，多出来的在一半以内，偶尔歇一下。
func TestPacerJitter(t *testing.T) {
	p := NewPacer(100 * time.Millisecond)
	seen := map[time.Duration]bool{}
	rests := 0
	for range 4000 {
		gap := p.gap()
		if gap < 100*time.Millisecond {
			t.Fatalf("间隔不能短于基准: %s", gap)
		}
		if gap > 150*time.Millisecond {
			rests++
			if gap < 600*time.Millisecond || gap > 1650*time.Millisecond {
				t.Fatalf("歇的时间应该是基准的 5~15 倍: %s", gap)
			}
		}
		seen[gap] = true
	}
	if len(seen) < 1000 {
		t.Errorf("间隔应该是随机的，4000 次只出现了 %d 种", len(seen))
	}
	if rests < 40 || rests > 200 {
		t.Errorf("平均每 40 次歇一次，4000 次歇了 %d 次", rests)
	}
	// 被限流放慢以后，歇的时间仍按基准间隔算。
	p.interval = time.Minute
	for range 4000 {
		if gap := p.gap(); gap > 90*time.Second+1500*time.Millisecond {
			t.Fatalf("放慢后歇的时间不该跟着翻倍: %s", gap)
		}
	}
}

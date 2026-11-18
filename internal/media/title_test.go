package media

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"雾港 第二季":                      "雾港第二季",
		"雾港：第二季":                      "雾港第二季",
		"  The Shawshank Redemption ": "theshawshankredemption",
		"ＡＢＣ１２３":                      "abc123", // 全角转半角
		"!!!":                         "",
	}
	for input, want := range cases {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestSplitSeason(t *testing.T) {
	cases := []struct {
		title, base string
		season      int
	}{
		{"雾港", "雾港", 1},
		{"雾港 第二季", "雾港", 2},
		{"雾港第十二季", "雾港", 12},
		{"雾港 第2季", "雾港", 2},
		{"Dark Season 3", "Dark", 3},
		{"三体 第一部", "三体", 1},
		{"第二季", "第二季", 1}, // 去掉季数后什么都不剩时保持原样
		{"风骚律师 S06", "风骚律师", 6},
	}
	for _, c := range cases {
		base, season := SplitSeason(c.title)
		if base != c.base || season != c.season {
			t.Errorf("SplitSeason(%q) = (%q, %d)，期望 (%q, %d)", c.title, base, season, c.base, c.season)
		}
	}
	if SeriesKey("雾港 第二季") != SeriesKey("雾港") {
		t.Error("同一系列的不同季应该有相同的 SeriesKey")
	}
}

func TestChineseNumber(t *testing.T) {
	cases := map[string]int{"一": 1, "十": 10, "十二": 12, "二十": 20, "二十一": 21, "两": 2, "7": 7, "一百零一": 101, "甲": 0}
	for input, want := range cases {
		if got := chineseNumber(input); got != want {
			t.Errorf("chineseNumber(%q) = %d，期望 %d", input, got, want)
		}
	}
}

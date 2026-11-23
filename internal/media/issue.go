package media

import (
	"fmt"
	"strings"
)

// 综艺按"期"选集，集号编码和 web/static/js/lib/issue.js 一致：
//
//	集号 = 期号 × 100 + 版本 × 10 + 段落，先导片是第 0 期。
//	版本：0 正片，1 加更，2 第二个加更，3 沉浸版，4 日记，5 福利篇，6 会员版。
//	段落：1 整期，2 上，3 中，4 下。末两位是 00 表示"这一期，哪段都行"，播放页再换成实际的段落。
//
// 例如 302 是第 3 期上，211 是第 2 期加更。剧集、动漫的集号就是第几集。

// Issue 是综艺集号拆开后的样子。
type Issue struct {
	Number  int // 期号，0 是先导片
	Variant int
	Half    int
}

const issueWhole = 1

var (
	issueVariants = map[int]string{1: "加更", 2: "加更 2", 3: "沉浸版", 4: "日记", 5: "福利篇", 6: "会员版"}
	issueHalves   = map[int]string{2: "上", 3: "中", 4: "下"}
)

// DecodeIssue 拆开综艺集号。
func DecodeIssue(episode int) Issue {
	return Issue{Number: episode / 100, Variant: episode % 100 / 10, Half: episode % 10}
}

// IssueEpisode 是"第 n 期，哪段都行"的集号。
func IssueEpisode(number int) int { return number * 100 }

// Main 表示这是正片整期（不是上下、加更）。
func (i Issue) Main() bool { return i.Variant == 0 && i.Half == issueWhole }

// String 是"第 3 期 上""第 2 期 加更""先导片"这样的文字。
func (i Issue) String() string {
	head := fmt.Sprintf("第 %d 期", i.Number)
	if i.Number == 0 {
		head = "先导片"
	}
	words := []string{head}
	if name := issueVariants[i.Variant]; name != "" {
		words = append(words, name)
	}
	if name := issueHalves[i.Half]; name != "" {
		words = append(words, name)
	}
	return strings.Join(words, " ")
}

// EpisodeLabel 是某一集给人看的名字：综艺是"第 3 期 上"，其他是"第 3 集"。
func EpisodeLabel(mediaType string, episode int) string {
	if mediaType == TypeShow {
		return DecodeIssue(episode).String()
	}
	return fmt.Sprintf("第 %d 集", episode)
}

// EpisodeUnit 是集数的单位：综艺是"期"，其他是"集"。
func EpisodeUnit(mediaType string) string {
	if mediaType == TypeShow {
		return "期"
	}
	return "集"
}

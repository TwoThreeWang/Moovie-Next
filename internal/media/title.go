package media

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Normalize 把片名规范化成只含字母和数字的小写形式，用于比较和搜索。
// "雾港 第二季"、"雾港第二季"、"雾港：第二季"规范化后相同。
func Normalize(value string) string {
	value = strings.ToLower(norm.NFKC.String(strings.TrimSpace(value)))
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

var seasonPatterns = []*regexp.Regexp{
	regexp.MustCompile(`第\s*([0-9一二三四五六七八九十百零两]+)\s*[季部]`),
	regexp.MustCompile(`(?i)\bseason\s*([0-9]+)\b`),
	regexp.MustCompile(`(?i)\bs([0-9]{1,2})\b`),
}

// SplitSeason 从片名里拆出季数。没有季数标记时返回原片名和 1。
// 例如 "雾港 第二季" → ("雾港", 2)，"Dark Season 3" → ("Dark", 3)。
func SplitSeason(title string) (base string, season int) {
	for _, pattern := range seasonPatterns {
		location := pattern.FindStringSubmatchIndex(title)
		if location == nil {
			continue
		}
		number := chineseNumber(title[location[2]:location[3]])
		if number < 1 || number > 99 {
			continue
		}
		base = strings.TrimSpace(title[:location[0]] + " " + title[location[1]:])
		base = strings.TrimRight(base, " :：-—·")
		if base == "" {
			return title, 1
		}
		return base, number
	}
	return strings.TrimSpace(title), 1
}

// SeriesKey 返回同一系列各季共用的键：去掉季数标记后的规范化片名。
func SeriesKey(title string) string {
	base, _ := SplitSeason(title)
	return Normalize(base)
}

var chineseDigits = map[rune]int{'零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}

// chineseNumber 把 "12"、"二"、"十二"、"二十一" 这样的数字转成整数，无法识别时返回 0。
func chineseNumber(text string) int {
	if number, err := strconv.Atoi(text); err == nil {
		return number
	}
	total, current := 0, 0
	for _, r := range text {
		switch {
		case r == '百':
			if current == 0 {
				current = 1
			}
			total += current * 100
			current = 0
		case r == '十':
			if current == 0 {
				current = 1
			}
			total += current * 10
			current = 0
		default:
			digit, ok := chineseDigits[r]
			if !ok {
				return 0
			}
			current = digit
		}
	}
	return total + current
}

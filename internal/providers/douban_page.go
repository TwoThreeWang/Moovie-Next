package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// SubjectPage 打开条目的详情网页（movie.douban.com/subject/<id>/）读资料，是详情接口（Subject）的兜底。
// 豆瓣分接口限流，详情接口被限流、或者对个别条目返回 403 时，网页常常还能打开。
//
// 网页比接口少几样东西：没有 is_show，演员只有页面上列出的那些；海报换成和接口一样大小的那一档。
// 网页的结构随时可能变，解析不出片名就报错，不会写进半截资料。
func (d *Douban) SubjectPage(ctx context.Context, id string) (*DoubanSubject, error) {
	if id == "" || strings.Trim(id, "0123456789") != "" {
		return nil, fmt.Errorf("豆瓣 ID 不合法: %q", id)
	}
	var page []byte
	endpoint := d.WebBase + "/subject/" + url.PathEscape(id) + "/"
	if err := d.get(ctx, doubanCall{endpoint: endpoint, referer: "https://movie.douban.com/", desktop: true, page: true}, &page); err != nil {
		return nil, err
	}
	return parseSubjectPage(id, page)
}

var pageTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// pageBlocked 认出豆瓣拦人时给的网页：标题只有"豆瓣"两个字，或者写着禁止访问、验证、异常请求。
// 正常的详情网页标题是"片名 (豆瓣)"。
func pageBlocked(page []byte) bool {
	match := pageTitle.FindSubmatch(page)
	if match == nil {
		return true
	}
	title := strings.TrimSpace(html.UnescapeString(string(match[1])))
	return title == "豆瓣" || strings.Contains(title, "禁止访问") || strings.Contains(title, "验证") || strings.Contains(title, "异常请求")
}

var (
	ldType    = regexp.MustCompile(`"@type"\s*:\s*"([A-Za-z]+)"`)
	infoField = regexp.MustCompile(`^\s*([^:：]+?)\s*[:：]\s*(.*?)\s*$`)
)

// parseSubjectPage 从详情网页里取出资料。
func parseSubjectPage(id string, page []byte) (*DoubanSubject, error) {
	root, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, fmt.Errorf("解析豆瓣网页: %w", err)
	}
	var (
		title, reviewed, year, poster, rating, summary, fullSummary, runtime, info, ld string
		genres, directors, actors                                                      []string
	)
	var walk func(node *html.Node, inMainPic bool)
	walk = func(node *html.Node, inMainPic bool) {
		if node.Type == html.ElementNode {
			property, rel, class := attr(node, "property"), attr(node, "rel"), " "+attr(node, "class")+" "
			switch {
			case node.Data == "title" && title == "":
				title = text(node)
			case node.Data == "div" && attr(node, "id") == "mainpic":
				inMainPic = true
			case node.Data == "div" && attr(node, "id") == "info":
				info = text(node)
			case node.Data == "img" && inMainPic && poster == "":
				poster = attr(node, "src")
			case node.Data == "script" && attr(node, "type") == "application/ld+json":
				ld = text(node)
			case property == "v:itemreviewed":
				reviewed = text(node)
			case property == "v:average":
				rating = text(node)
			case property == "v:summary":
				summary = text(node)
			case property == "v:genre":
				genres = append(genres, text(node))
			case property == "v:runtime":
				runtime = attr(node, "content")
			case rel == "v:directedBy":
				directors = append(directors, text(node))
			case rel == "v:starring":
				actors = append(actors, text(node))
			case node.Data == "span" && strings.Contains(class, " year "):
				year = text(node)
			case node.Data == "span" && strings.Contains(class, " all ") && strings.Contains(class, " hidden "):
				fullSummary = text(node) // 简介很长时，页面先显示一段，完整的藏在这里
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, inMainPic)
		}
	}
	walk(root, false)

	title = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(title), "(豆瓣)"))
	if title == "" || reviewed == "" {
		return nil, errors.New("豆瓣网页里解析不出片名，页面结构可能变了")
	}
	subject := &DoubanSubject{ID: id, Kind: "movie", Title: title, Genres: genres, Directors: directors, Actors: actors}
	if original := strings.TrimSpace(strings.TrimPrefix(reviewed, title)); original != reviewed {
		subject.OriginalTitle = original
	}
	subject.Year, _ = strconv.Atoi(leadingNumber.FindString(year))
	subject.Rating, _ = strconv.ParseFloat(strings.TrimSpace(rating), 64)
	// 网页上是小图（s_ratio_poster），换成接口给的那一档（m_ratio_poster），免得刷新以后海报变糊。
	subject.Poster = strings.Replace(poster, "/s_ratio_poster/", "/m_ratio_poster/", 1)
	if fullSummary != "" {
		summary = fullSummary
	}
	subject.Intro = paragraphs(summary)
	subject.DurationMinutes, _ = strconv.Atoi(runtime)
	if match := ldType.FindStringSubmatch(ld); match != nil && match[1] == "TVSeries" {
		subject.Kind = "tv"
	}
	for _, line := range strings.Split(info, "\n") {
		match := infoField.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		value := match[2]
		switch match[1] {
		case "制片国家/地区":
			subject.Countries = splitSlash(value)
		case "又名":
			subject.AKA = splitSlash(value)
		case "集数":
			subject.Kind = "tv"
			subject.EpisodesCount, _ = strconv.Atoi(leadingNumber.FindString(value))
		case "单集片长", "片长":
			if subject.DurationMinutes == 0 {
				subject.DurationMinutes, _ = strconv.Atoi(leadingNumber.FindString(value))
			}
		}
	}
	return subject, nil
}

func attr(node *html.Node, name string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			return attribute.Val
		}
	}
	return ""
}

// text 取节点里的文字，<br> 换成换行。
func text(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		switch {
		case current.Type == html.TextNode:
			builder.WriteString(current.Data)
		case current.Type == html.ElementNode && current.Data == "br":
			builder.WriteByte('\n')
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.TrimSpace(builder.String())
}

// paragraphs 去掉每行首尾的空白和空行，段落之间用一个换行隔开，和接口给的简介一样。
func paragraphs(value string) string {
	lines := []string{}
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func splitSlash(value string) []string {
	values := []string{}
	for _, part := range strings.Split(value, "/") {
		if part = strings.TrimSpace(part); part != "" {
			values = append(values, part)
		}
	}
	return values
}

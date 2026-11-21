package app

import (
	"strconv"
	"strings"
	"time"

	"moovie/internal/collections"
)

func (a *App) collectionIndexSEO(order string, page int, list []collections.Collection) SEO {
	const name = "片单广场"
	seo := a.pageSEO(collectionIndexURL(order, page), "在 "+a.Config.SiteName+"发现影迷整理的电影与剧集主题片单：按题材、导演、心情挑片，看每部作品的推荐语，收藏感兴趣的片单。")
	seo.Title = name + " - 电影与剧集主题片单"
	seo.Keywords = []string{"片单", "电影片单", "剧集片单", "片单推荐", "影单", "主题片单"}
	if order == "hot" {
		seo.Title = "近 7 天热门片单"
		seo.NoIndex = true
	}
	if page > 1 {
		label := "第 " + strconv.Itoa(page) + " 页"
		seo.Title += " · " + label
		seo.Description = name + label + "。 " + seo.Description
	}
	if seo.NoIndex {
		return seo
	}
	links := make([][2]string, 0, len(list))
	for _, item := range list {
		links = append(links, [2]string{item.Title, a.absolute("/list/" + item.Slug)})
	}
	crumbs := [][2]string{{"首页", a.absolute("/")}, {name, a.absolute("/list")}}
	if page > 1 {
		crumbs = append(crumbs, [2]string{"第 " + strconv.Itoa(page) + " 页", seo.Canonical})
	}
	seo.JSONLD = collectionSchema(seo, seo.Title, links, crumbs, nil)
	return seo
}

func (a *App) collectionDetailSEO(collection collections.Collection, cards []CardView) SEO {
	// 描述：作者写的简介；没写或太短（不到 30 字）时补上作者、作品数和前几部作品。
	summary := "《" + collection.Title + "》由 " + collection.AuthorName + " 整理，收录 " + strconv.Itoa(len(cards)) + " 部影视作品"
	titles := make([]string, 0, 3)
	for _, card := range firstN(cards, 3) {
		titles = append(titles, "《"+card.Card.Title+"》")
	}
	if len(titles) > 0 {
		summary += "，包括" + strings.Join(titles, "、")
	}
	description := clip(collection.Description, descriptionLimit)
	if count := len([]rune(description)); count == 0 {
		description = summary + "。"
	} else if count < 30 {
		description = strings.TrimRight(description, "。.！!") + "。" + summary + "。"
	}
	description = clip(description, descriptionLimit)
	seo := a.pageSEO("/list/"+collection.Slug, description)
	seo.Title = collection.Title
	if !strings.Contains(seo.Title, "片单") {
		seo.Title += " - 片单"
	}
	seo.Keywords = []string{collection.Title, collection.Title + " 片单"}
	for _, card := range firstN(cards, 4) {
		seo.Keywords = append(seo.Keywords, card.Card.Title)
	}
	seo.Keywords = append(seo.Keywords, "片单推荐")
	seo.NoIndex = collection.Status != collections.Published || len(cards) == 0
	if len(collection.Covers) > 0 {
		cover := collection.Covers[0]
		seo.Image = a.absolute(a.images.URL(cover.URL, "display", cover.Title, "poster"))
		seo.ImageAlt = "《" + cover.Title + "》海报"
	}
	if seo.NoIndex {
		return seo
	}
	links := make([][2]string, 0, len(cards))
	for _, card := range cards {
		links = append(links, [2]string{card.Card.Title, a.absolute(card.Href)})
	}
	author := map[string]any{"@type": "Person", "name": collection.AuthorName,
		"url": a.absolute("/user/" + strconv.FormatInt(collection.OwnerID, 10))}
	crumbs := [][2]string{{"首页", a.absolute("/")}, {"片单广场", a.absolute("/list")}, {collection.Title, seo.Canonical}}
	page := map[string]any{"author": author, "dateModified": collection.UpdatedAt.UTC().Format(time.RFC3339)}
	if collection.PublishedAt != nil {
		page["datePublished"] = collection.PublishedAt.UTC().Format(time.RFC3339)
	}
	seo.JSONLD = collectionSchema(seo, collection.Title, links, crumbs, page)
	return seo
}

func collectionSchema(seo SEO, name string, links, crumbs [][2]string, page map[string]any) map[string]any {
	if page == nil {
		page = make(map[string]any)
	}
	page["@type"], page["@id"], page["url"] = "CollectionPage", seo.Canonical+"#page", seo.Canonical
	page["name"], page["description"], page["inLanguage"] = name, seo.Description, "zh-CN"
	if seo.Image != "" {
		page["image"] = seo.Image
	}
	graph := []any{page, breadcrumbs(crumbs)}
	if len(links) > 0 {
		listID := seo.Canonical + "#items"
		page["mainEntity"] = map[string]string{"@id": listID}
		entries := make([]map[string]any, 0, len(links))
		for index, link := range links {
			entries = append(entries, map[string]any{"@type": "ListItem", "position": index + 1, "name": link[0], "url": link[1]})
		}
		graph = append(graph, map[string]any{"@type": "ItemList", "@id": listID, "name": name,
			"itemListElement": entries, "numberOfItems": len(entries)})
	}
	return map[string]any{"@context": "https://schema.org", "@graph": graph}
}

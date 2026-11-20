package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
	"moovie/internal/collections"
	"moovie/internal/httpx"
	"moovie/internal/media"
)

// ---------- 片单 ----------

func (a *App) listIndex(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	const pageSize = 24
	order := c.Query("sort")
	if order != "hot" {
		order = "new"
	}
	page, offset, valid := httpx.ParsePage(c.Query("page"), pageSize)
	if !valid {
		a.errorPage(c, http.StatusNotFound, "这页片单不存在", "可以返回片单列表继续浏览。")
		return
	}
	list, err := a.Collections.Browse(c.Request.Context(), order, pageSize+1, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	if page > 1 && len(list) == 0 {
		a.errorPage(c, http.StatusNotFound, "这页片单不存在", "可以返回片单列表继续浏览。")
		return
	}
	seo := a.collectionIndexSEO(order, page, firstN(list, pageSize))
	data := gin.H{"Title": "片单广场", "Nav": "lists", "Collections": firstN(list, pageSize), "Order": order, "SEO": seo}
	setPager(data, page, len(list) > pageSize, func(page int) string { return collectionIndexURL(order, page) })
	a.render.Page(c, http.StatusOK, "lists.html", data)
}

func collectionIndexURL(order string, page int) string {
	query := url.Values{}
	if order == "hot" {
		query.Set("sort", order)
	}
	return pagedURL("/list", query, page)
}

type collectionEntry struct {
	Card CardView
	Note string
}

func listEntry(card media.Card, note string) collectionEntry {
	return collectionEntry{Card: CardView{Card: card, Href: mediaHref(card.ID), Sub: defaultSub(card)}, Note: note}
}

func (a *App) listDetail(c *gin.Context) {
	ctx := c.Request.Context()
	c.Header("Cache-Control", "private, no-store")
	collection, items, err := a.Collections.VisibleDetail(ctx, c.Param("slug"))
	if err != nil {
		a.collectionError(c, err)
		return
	}
	cards, listEntries := collectionEntries(items)
	path := "/list/" + collection.Slug
	seo := a.collectionDetailSEO(collection, cards)
	data := gin.H{
		"Title": collection.Title, "Nav": "lists", "Collection": collection, "Entries": listEntries,
		"Card": a.listCard(collection, cards), "SEO": seo,
	}
	if collection.Status != collections.Published {
		c.Header("Referrer-Policy", "no-referrer")
		data["Shared"] = true
		a.render.Page(c, http.StatusOK, "list.html", data)
		return
	}
	engagement, err := a.Collections.Engagement(ctx, collection.ID, viewerID(c))
	if err != nil {
		a.fail(c, err)
		return
	}
	related, err := a.Collections.RelatedTo(ctx, collection.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	data["Related"], data["Engagement"] = related, engagement
	data["Public"] = true
	data["LoginURL"] = "/auth/login?redirect=" + url.QueryEscape(path)
	data["CountView"] = viewerID(c) != collection.OwnerID
	data["Scripts"] = []string{"pages/collection-detail.js"}
	a.render.Page(c, http.StatusOK, "list.html", data)
}

func collectionEntries(items []collections.Item) ([]CardView, []collectionEntry) {
	cards := make([]CardView, 0, len(items))
	listEntries := make([]collectionEntry, 0, len(items))
	for _, item := range items {
		entry := listEntry(item.Card, item.Note)
		cards = append(cards, entry.Card)
		listEntries = append(listEntries, entry)
	}
	return cards, listEntries
}

func (a *App) collectionView(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	now := time.Now().UTC()
	viewer := viewerID(c)
	key := "guest:" + httpx.CSRFToken(c)
	if viewer != 0 {
		key = "user:" + strconv.FormatInt(viewer, 10)
	}
	mac := hmac.New(sha256.New, []byte(a.Config.AppSecret))
	mac.Write([]byte("collection-view:" + now.Format(time.DateOnly) + ":" + key))
	if err := a.Collections.RecordView(c.Request.Context(), c.Param("slug"), viewer, now, mac.Sum(nil)[:16]); err != nil {
		a.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (a *App) collectionFavorite(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	active := c.PostForm("intent") == "save"
	if !active && c.PostForm("intent") != "remove" {
		httpx.Error(c, http.StatusBadRequest, "收藏操作无效。")
		return
	}
	ctx := c.Request.Context()
	if err := a.Collections.SetFavorite(ctx, id, viewerID(c), active); err != nil {
		a.collectionError(c, err)
		return
	}
	if c.PostForm("from") != "favorites" {
		collection, err := a.Collections.ByID(ctx, id)
		if err != nil && !errors.Is(err, collections.ErrNotFound) {
			a.fail(c, err)
			return
		}
		if collection.Status == collections.Published {
			c.Redirect(http.StatusSeeOther, "/list/"+collection.Slug)
			return
		}
	}
	c.Redirect(http.StatusSeeOther, "/me/collections/favorites")
}

func (a *App) myCollections(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	const pageSize = 24
	page, offset := httpx.Page(c, pageSize)
	list, err := a.Collections.Owned(c.Request.Context(), viewerID(c), pageSize+1, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	data := gin.H{"Title": "我的片单", "Nav": "me", "SEO": SEO{NoIndex: true}, "Collections": firstN(list, pageSize), "Notice": c.Query("notice")}
	setPager(data, page, len(list) > pageSize, pageLink("/me/collections"))
	a.render.Page(c, http.StatusOK, "my_collections.html", data)
}

func (a *App) favoriteCollections(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	const pageSize = 24
	page, offset := httpx.Page(c, pageSize)
	list, err := a.Collections.Favorites(c.Request.Context(), viewerID(c), pageSize+1, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	data := gin.H{"Title": "收藏的片单", "Nav": "me", "SEO": SEO{NoIndex: true}, "Favorites": firstN(list, pageSize), "ShowFavorites": true}
	setPager(data, page, len(list) > pageSize, pageLink("/me/collections/favorites"))
	a.render.Page(c, http.StatusOK, "my_collections.html", data)
}

func (a *App) collectionVisibility(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	ctx, userID := c.Request.Context(), viewerID(c)
	intent := c.PostForm("intent")
	var base time.Time
	if intent != "delete" {
		var err error
		base, err = time.Parse(time.RFC3339Nano, c.PostForm("base"))
		if err != nil {
			httpx.Error(c, http.StatusBadRequest, "请重新打开片单后再操作。")
			return
		}
	}
	var err error
	var notice string
	switch intent {
	case "share":
		err = a.Collections.SetLinkVisibility(ctx, id, userID, base, true)
		notice = "链接访问已开启"
	case "unshare":
		err = a.Collections.SetLinkVisibility(ctx, id, userID, base, false)
		notice = "链接访问已关闭"
	case "discard-revision":
		err = a.Collections.DiscardRevision(ctx, id, userID, base)
		notice = "已放弃修改，公开页保持原样"
	case "withdraw":
		err = a.Collections.Withdraw(ctx, id, userID, base, false)
		notice = "已撤回，片单现为私密"
	case "withdraw-link":
		err = a.Collections.Withdraw(ctx, id, userID, base, true)
		notice = "已撤下广场，原链接仍可访问"
	case "delete":
		err = a.Collections.DeleteOwned(ctx, id, userID)
		notice = "片单已删除"
	default:
		httpx.Error(c, http.StatusBadRequest, "片单操作无效。")
		return
	}
	if err != nil {
		a.collectionError(c, err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/me/collections?notice="+url.QueryEscape(notice))
}

func (a *App) adminCollections(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	status := c.Query("status")
	if status != collections.Published && status != collections.Returned {
		status = collections.Pending
	}
	page, offset := httpx.Page(c, adminPageSize)
	list, err := a.Collections.ModerationQueue(c.Request.Context(), status, adminPageSize+1, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	data := gin.H{"Status": status, "Collections": firstN(list, adminPageSize)}
	setPager(data, page, len(list) > adminPageSize, pageLink("/admin/collections?status="+status))
	a.adminPage(c, "admin_collections.html", "片单管理", "collections", data)
}

func (a *App) adminCollectionReview(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	collection, items, err := a.Collections.ModerationDetail(c.Request.Context(), id)
	if err != nil {
		a.collectionError(c, err)
		return
	}
	_, entries := collectionEntries(items)
	a.adminPage(c, "admin_collection_review.html", "审核片单", "collections", gin.H{
		"Collection": collection, "Entries": entries, "Base": collection.UpdatedAt.Format(time.RFC3339Nano),
	})
}

func (a *App) adminCollectionDecision(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	base, err := time.Parse(time.RFC3339Nano, c.PostForm("base"))
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, "请重新打开审核页后再操作。")
		return
	}
	err = a.Collections.Review(c.Request.Context(), id, auth.Current(c).ID, base, c.PostForm("intent"), c.PostForm("reason"))
	if err != nil {
		a.collectionError(c, err)
		return
	}
	c.Redirect(http.StatusSeeOther, "/admin/collections?notice="+url.QueryEscape("审核结果已保存"))
}

func (a *App) collectionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, collections.ErrNotFound):
		httpx.Error(c, http.StatusNotFound, "这个片单暂时无法访问。")
	case errors.Is(err, collections.ErrConflict):
		httpx.Error(c, http.StatusConflict, "片单状态或内容已变化，请重新打开片单后再操作。")
	case errors.Is(err, collections.ErrInvalid):
		httpx.Error(c, http.StatusBadRequest, "请检查片单内容或填写 300 字以内的审核原因。")
	case errors.Is(err, collections.ErrMissingItem):
		httpx.Error(c, http.StatusConflict, "请确认所选作品仍在作品库，公开片单至少需要一部可见作品。")
	default:
		a.fail(c, err)
	}
}

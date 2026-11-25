package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
	"moovie/internal/collections"
	"moovie/internal/httpx"
	"moovie/internal/media"
)

type editorEntry struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Meta   string `json:"meta"`
	Poster string `json:"poster,omitempty"`
	Note   string `json:"note,omitempty"`
	Hidden bool   `json:"hidden,omitempty"`
}

func (a *App) editorEntry(card media.Card) editorEntry {
	meta := card.TypeLabel()
	if card.Year > 0 {
		meta = strconv.Itoa(card.Year) + " · " + meta
	}
	return editorEntry{
		ID: strconv.FormatInt(card.ID, 10), Title: card.Title, Meta: meta,
		Poster: a.images.URL(card.Poster, "card", card.Title, "poster"),
	}
}

func (a *App) collectionSearch(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	q := strings.TrimSpace(c.Query("q"))
	if q == "" || utf8.RuneCountInString(q) > 120 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入 120 字以内的片名。"})
		return
	}
	found, err := a.Media.Search(c.Request.Context(), q, 16)
	if err != nil {
		a.fail(c, err)
		return
	}
	entries := make([]editorEntry, 0, len(found))
	for _, card := range found {
		entries = append(entries, a.editorEntry(card))
	}
	c.JSON(http.StatusOK, entries)
}

func (a *App) collectionEdit(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	data := gin.H{"Title": "新建片单", "Nav": "me", "Collection": collections.Collection{}, "Entries": []editorEntry{},
		"Scripts": []string{"pages/collection-editor.js"}, "SEO": SEO{NoIndex: true}, "Notice": c.Query("notice")}
	if c.Param("id") != "" {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		collection, items, err := a.Collections.OwnerDetail(c.Request.Context(), id, auth.Current(c).ID)
		if err != nil {
			a.collectionError(c, err)
			return
		}
		entries := make([]editorEntry, 0, len(items))
		for _, item := range items {
			entry := a.editorEntry(item.Card)
			entry.Note, entry.Hidden = item.Note, item.Hidden
			entries = append(entries, entry)
		}
		data["Title"], data["Collection"], data["Entries"] = "编辑片单", collection, entries
		data["Base"] = collection.UpdatedAt.Format(time.RFC3339Nano)
	}
	a.render.Page(c, http.StatusOK, "collection_editor.html", data)
}

// 只接受作品 ID 和备注，片名、海报等展示数据从作品库取。
func (a *App) collectionDraft(c *gin.Context) (collections.Draft, bool) {
	draft := collections.Draft{
		OwnerID: auth.Current(c).ID, Title: strings.TrimSpace(c.PostForm("title")),
		Description: strings.TrimSpace(c.PostForm("description")), Submit: c.PostForm("intent") == "submit",
		Notes: make(map[int64]string),
	}
	var err error
	draft.ID, err = strconv.ParseInt(c.PostForm("id"), 10, 64)
	if err != nil || draft.ID < 0 {
		httpx.Error(c, http.StatusBadRequest, "片单编号无效，请重新打开编辑器。")
		return draft, false
	}
	if draft.Title == "" || utf8.RuneCountInString(draft.Title) > 80 || utf8.RuneCountInString(draft.Description) > 1000 {
		httpx.Error(c, http.StatusBadRequest, "请填写 80 字以内的标题，简介最多 1000 字。")
		return draft, false
	}
	var entries []editorEntry
	raw := c.PostForm("entries")
	if len(raw) > 400000 || json.Unmarshal([]byte(raw), &entries) != nil || len(entries) == 0 || len(entries) > 200 {
		httpx.Error(c, http.StatusBadRequest, "请在片单中加入 1 至 200 部作品。")
		return draft, false
	}
	for _, entry := range entries {
		id, err := strconv.ParseInt(entry.ID, 10, 64)
		_, duplicate := draft.Notes[id]
		if err != nil || id <= 0 || duplicate || utf8.RuneCountInString(entry.Note) > 300 {
			httpx.Error(c, http.StatusBadRequest, "请检查作品是否重复或无效，每条推荐语最多 300 字。")
			return draft, false
		}
		draft.MediaIDs = append(draft.MediaIDs, id)
		draft.Notes[id] = strings.TrimSpace(entry.Note)
	}
	if draft.ID > 0 {
		current, err := a.Collections.ByID(c.Request.Context(), draft.ID)
		if errors.Is(err, collections.ErrNotFound) || (err == nil && current.OwnerID != draft.OwnerID) {
			httpx.Error(c, http.StatusNotFound, "这个片单不存在。")
			return draft, false
		}
		if err != nil {
			a.fail(c, err)
			return draft, false
		}
		draft.Slug = current.Slug
		base, err := time.Parse(time.RFC3339Nano, c.PostForm("base"))
		if err != nil || !base.Equal(current.UpdatedAt) {
			httpx.Error(c, http.StatusConflict, "片单状态或内容已变化。请保留本机草稿，再重新打开片单核对。")
			return draft, false
		}
		draft.Base = base
	}
	return draft, true
}

func (a *App) collectionSave(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	draft, ok := a.collectionDraft(c)
	if !ok {
		return
	}
	if draft.ID == 0 {
		var suffix [16]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			a.fail(c, err)
			return
		}
		draft.Slug = "list-" + hex.EncodeToString(suffix[:])
	}
	id, err := a.Collections.Save(c.Request.Context(), draft)
	if err != nil {
		a.collectionError(c, err)
		return
	}
	next := "/me/collections/" + strconv.FormatInt(id, 10) + "?notice=" + url.QueryEscape("已保存片单")
	if draft.Submit {
		next = "/me/collections?notice=" + url.QueryEscape("已投稿，等待管理员审核")
	}
	if httpx.WantsJSON(c) {
		c.JSON(http.StatusOK, gin.H{"redirect": next})
		return
	}
	c.Redirect(http.StatusSeeOther, next)
}

func (a *App) collectionPreview(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	draft, ok := a.collectionDraft(c)
	if !ok {
		return
	}
	cards, err := a.Media.Cards(c.Request.Context(), draft.MediaIDs)
	if err != nil {
		a.fail(c, err)
		return
	}
	entries := make([]collectionEntry, 0, len(cards))
	for _, card := range cards {
		entries = append(entries, listEntry(card, draft.Notes[card.ID]))
	}
	data := gin.H{"Entries": entries, "Collection": collections.Collection{Title: draft.Title, Description: draft.Description, Count: len(entries)}}
	// 编辑器用 fetch 取片段，显示在页内对话框里。不再把表单 POST 到新标签页：
	// 安卓上已安装的网页应用、各家浏览器的 WebView 打开新窗口时只带地址，POST 的内容会丢，变成一次 GET。
	if httpx.IsHTMX(c) {
		a.render.Partial(c, "collection_preview", data)
		return
	}
	data["Title"], data["Nav"], data["PreviewLabel"], data["SEO"] = draft.Title+" · 预览", "me", "编辑预览 · 仅自己可见", SEO{NoIndex: true}
	a.render.Page(c, http.StatusOK, "list.html", data)
}

// 直接打开预览地址（旧版本在安卓新标签页里打开预览、刷新预览页）时没有草稿内容。
// 单独接住，免得落到 /me/collections/:id，把 preview 当成片单编号报「参数不正确」。
func (a *App) collectionPreviewGet(c *gin.Context) {
	httpx.Error(c, http.StatusBadRequest, "预览内容只在片单编辑器里生成。请回到编辑器，再点「预览」。")
}

package app

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/ads"
	"moovie/internal/auth"
	"moovie/internal/httpx"
	"moovie/internal/ingest"
	"moovie/internal/library"
	"moovie/internal/media"
	"moovie/internal/playback"
)

// EpisodeView 是详情页和播放页上一集的展示数据。
// Number 是显示的第几集（综艺是第几期），Episode 是链接里的集号（综艺的编码见 media.DecodeIssue）。
type EpisodeView struct {
	Number    int
	Episode   int
	Title     string
	AirDate   *time.Time
	Runtime   int
	Href      string
	Pct       int
	Completed bool
	Current   bool
	Upcoming  bool
}

// ClientMedia 是嵌在页面里给浏览器查源用的作品信息。只有片名这类信息。
type ClientMedia struct {
	ID            int64    `json:"id"`
	Title         string   `json:"title"`
	OriginalTitle string   `json:"original_title"`
	Aliases       []string `json:"aliases"`
	Year          int      `json:"year"`
	Type          string   `json:"type"`
	Season        int      `json:"season"`
	EpisodeCount  int      `json:"episode_count"`
	DoubanID      string   `json:"douban_id"`
	Poster        string   `json:"poster"`
}

// loadMedia 读取路径里的作品。不存在或已隐藏时渲染 404 并返回 false。
func (a *App) loadMedia(c *gin.Context) (media.Media, bool) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return media.Media{}, false
	}
	m, err := a.Media.Get(c.Request.Context(), id)
	if errors.Is(err, media.ErrNotFound) || (err == nil && m.Hidden && !auth.Current(c).IsAdmin()) {
		if httpx.WantsJSON(c) || httpx.IsHTMX(c) {
			httpx.Error(c, http.StatusNotFound, "作品不存在")
		} else {
			a.render.NotFound(c)
		}
		return m, false
	}
	if err != nil {
		a.fail(c, err)
		return m, false
	}
	return m, true
}

// episodes 生成分集列表：有分集资料时用资料，没有时按总集数生成"第 N 集"。
// 综艺按期列出，每一期链到"这一期，哪段都行"；源里有哪几段（上下、加更）要等浏览器查完源，由页面脚本按期重排。
func (a *App) episodes(m media.Media, units []media.Unit, positions []playback.Position, currentEpisode int) (list []EpisodeView, aired int) {
	if m.Type == media.TypeMovie {
		return nil, 0
	}
	show := m.Type == media.TypeShow
	progress := map[int]playback.Position{}
	for _, p := range positions {
		if p.Season == m.Season {
			progress[p.Episode] = p
		}
	}
	today := media.Today()
	add := func(view EpisodeView) {
		view.Episode = view.Number
		view.Current = view.Number == currentEpisode
		if show {
			view.Episode = media.IssueEpisode(view.Number)
			view.Current = media.DecodeIssue(currentEpisode).Number == view.Number
		}
		view.Href = playHref(m.ID, m.Season, view.Episode)
		if p, ok := progress[view.Episode]; ok {
			view.Completed = p.Completed
			if p.Duration > 0 {
				view.Pct = int(min(100, p.Position/p.Duration*100))
			}
		}
		if view.AirDate != nil && view.AirDate.After(today) {
			view.Upcoming = true
		} else {
			aired = max(aired, view.Number)
		}
		list = append(list, view)
	}
	seen := 0
	for _, unit := range units {
		if unit.Season != m.Season {
			continue
		}
		seen++
		add(EpisodeView{Number: unit.Episode, Title: unit.Title, AirDate: unit.AirDate, Runtime: unit.RuntimeMinutes})
	}
	if seen == 0 {
		for number := 1; number <= min(m.EpisodeCount, 500); number++ {
			add(EpisodeView{Number: number})
		}
	}
	return list, aired
}

// completedEpisodes 是这一季看完了的集号。综艺的选集由页面脚本按源重排，用它标出看过的。
func completedEpisodes(m media.Media, positions []playback.Position) []int {
	seen := []int{}
	for _, p := range positions {
		if p.Season == m.Season && p.Completed {
			seen = append(seen, p.Episode)
		}
	}
	return seen
}

// firstEpisode 是从头播放时的集号：第 1 集；综艺是"第 1 期，哪段都行"。
func firstEpisode(m media.Media) int {
	if m.Type == media.TypeShow {
		return media.IssueEpisode(1)
	}
	return 1
}

func (a *App) clientMedia(c *gin.Context, m media.Media) (ClientMedia, error) {
	aliases, err := a.Media.Aliases(c.Request.Context(), m.ID, 6)
	return ClientMedia{
		ID: m.ID, Title: m.Title, OriginalTitle: m.OriginalTitle, Aliases: aliases, Year: m.Year, Type: m.Type,
		Season: m.Season, EpisodeCount: m.EpisodeCount, DoubanID: m.DoubanID, Poster: m.Poster,
	}, err
}

func (a *App) detailPage(c *gin.Context) {
	m, ok := a.loadMedia(c)
	if !ok {
		return
	}
	ctx, viewer := c.Request.Context(), viewerID(c)
	// 标记和播放进度只有登录用户才有；没登录的访客（包括爬虫）不用查这两次。
	var (
		entry     library.Entry
		hasEntry  bool
		positions []playback.Position
		err       error
	)
	if viewer != 0 {
		if entry, hasEntry, err = a.Library.Get(ctx, viewer, m.ID); err != nil {
			a.fail(c, err)
			return
		}
		if positions, err = a.Positions.ForMedia(ctx, viewer, m.ID); err != nil {
			a.fail(c, err)
			return
		}
	}
	// 有人打开了一部还没补全资料的作品：让后台优先去抓，页面上显示"资料获取中"。
	if m.MetadataSyncedAt == nil {
		if err := a.Ingest.EnqueueRefresh(ctx, m.ID, ingest.PriorityViewing); err != nil {
			c.Error(err)
		}
	}
	units, err := a.Media.Units(ctx, m.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	series, err := a.Media.Series(ctx, m)
	if err != nil {
		a.fail(c, err)
		return
	}
	client, err := a.clientMedia(c, m)
	if err != nil {
		a.fail(c, err)
		return
	}

	// 播放按钮：有进度就接着看，没有就从头开始。
	playLabel, playSub, playEpisode := "播放", "", firstEpisode(m)
	if len(positions) > 0 {
		latest := positions[0]
		playLabel, playEpisode = "继续播放", latest.Episode
		playSub = remaining(latest.Position, latest.Duration)
		if m.IsSeries() {
			playSub = strings.TrimSuffix(media.EpisodeLabel(m.Type, latest.Episode)+" · "+playSub, " · ")
		}
	} else if m.IsSeries() {
		playSub = "第 1 " + media.EpisodeUnit(m.Type)
	}
	episodes, aired := a.episodes(m, units, positions, playEpisode)
	var nextAir *EpisodeView
	for index := range episodes {
		if episodes[index].Upcoming {
			nextAir = &episodes[index]
			break
		}
	}
	data := gin.H{
		"Title": m.Title, "Nav": "home", "CSS": []string{"detail"},
		"M": m, "Client": client, "Entry": entry, "HasEntry": hasEntry,
		"Episodes": episodes, "Status": episodeStatus(m, aired), "NextAir": nextAir, "Unit": media.EpisodeUnit(m.Type), "Series": cardViews(series),
		"IsShow": m.Type == media.TypeShow, "PlayEpisode": playEpisode, "Seen": completedEpisodes(m, positions),
		"PlayLabel": playLabel, "PlaySub": playSub, "PlayHref": playHref(m.ID, m.Season, playEpisode),
		"IMDb": m.IMDbID, "SEO": a.mediaSEO(m), "Card": a.mediaCard(m), "Fetching": m.MetadataSyncedAt == nil,
		"Login": "/auth/login?redirect=" + url.QueryEscape(mediaHref(m.ID)),
	}
	// 收录这部作品的片单：所有人都直接写在页面里（一次走索引的小查询）。读不出来不影响页面，只是不显示这一行。
	if lists, err := a.Collections.ContainingMedia(ctx, m.ID, 4); err != nil {
		a.Log.Warn("读取收录作品的片单失败", "media", m.ID, "error", err)
	} else {
		data["Lists"] = lists
	}
	if viewer != 0 {
		data["Scripts"] = []string{"pages/detail.js"}
	} else {
		// 没登录的访客里有搜索引擎的爬虫，很多（比如百度）不执行脚本：
		// 短评和相似推荐直接写在页面里，而不是滚动到附近才加载。这样爬虫能读到内容，也能顺着链接发现其他作品。
		comments, err := a.Social.MediaComments(ctx, 0, m.ID, 10, 0)
		if err != nil {
			a.fail(c, err)
			return
		}
		similar, err := a.Media.Similar(ctx, m, 16)
		if err != nil {
			a.fail(c, err)
			return
		}
		more := ""
		if len(comments) == 10 {
			more = "/api/htmx/m/" + strconv.FormatInt(m.ID, 10) + "/comments?page=2"
		}
		data["Inline"], data["Comments"], data["CommentsMore"], data["Similar"] = true, comments, more, cardViews(similar)
	}
	data["SideAds"] = a.Ads.Current(c.Request.Context(), ads.Sidebar)
	a.render.Page(c, http.StatusOK, "detail.html", data)
}

func (a *App) playPage(c *gin.Context) {
	m, ok := a.loadMedia(c)
	if !ok {
		return
	}
	ctx, user := c.Request.Context(), auth.Current(c)
	positions, err := a.Positions.ForMedia(ctx, user.ID, m.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	units, err := a.Media.Units(ctx, m.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	client, err := a.clientMedia(c, m)
	if err != nil {
		a.fail(c, err)
		return
	}
	episode := max(1, httpx.QueryInt(c, "e", firstEpisode(m)))
	if c.Query("e") == "" && len(positions) > 0 {
		episode = positions[0].Episode
	}
	episodes, aired := a.episodes(m, units, positions, episode)
	entry, hasEntry, err := a.Library.Get(ctx, user.ID, m.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.render.Page(c, http.StatusOK, "play.html", gin.H{
		"Title": m.Title + " - 播放", "Nav": "home", "CSS": []string{"player"}, "BodyClass": "player-page",
		"Scripts": []string{"pages/play.js"},
		"M":       m, "Client": client, "Episode": episode, "Episodes": episodes, "Status": episodeStatus(m, aired),
		"Positions": positions, "Entry": entry, "HasEntry": hasEntry, "SideAds": a.Ads.Current(c.Request.Context(), ads.Sidebar),
	})
}

// playLocal 是没有元数据的作品的播放页。片名、源、进度都在浏览器本地（加密后同步），
// 服务器只输出一个空壳页面。
func (a *App) playLocal(c *gin.Context) {
	a.render.Page(c, http.StatusOK, "play.html", gin.H{
		"Title": "播放", "Nav": "home", "CSS": []string{"player"}, "BodyClass": "player-page",
		"Scripts": []string{"pages/play.js"}, "Local": true, "SideAds": a.Ads.Current(c.Request.Context(), ads.Sidebar),
	})
}

// resolveAPI 把浏览器发来的片名信息对应到作品。请求里只有片名、年份、类型和豆瓣 ID。
// 只做匹配，不收录：搜索页"我的源"的结果里有大量不相干的片子，收录要等用户真的播放了再做（见 discoverAPI）。
func (a *App) resolveAPI(c *gin.Context) {
	user := auth.Current(c)
	if !a.resolve.Allow(strconv.FormatInt(user.ID, 10)) {
		httpx.Error(c, http.StatusTooManyRequests, "操作太频繁")
		return
	}
	var body struct {
		Items []media.Candidate `json:"items"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Items) == 0 || len(body.Items) > 40 {
		httpx.Error(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	results, err := a.Media.Resolve(c.Request.Context(), body.Items)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

// discoverAPI 收录用户正在播放的一部作品：从搜索页"我的源"点进本地播放、作品库里还没有、播放源给了豆瓣 ID 的。
// 先在作品库里占一行，后台去豆瓣确认并抓资料；已经有了就什么都不做。之后搜索和观看记录就能匹配上它。
func (a *App) discoverAPI(c *gin.Context) {
	var body struct {
		DoubanID string `json:"douban_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || !media.ValidDoubanID(body.DoubanID) {
		httpx.Error(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	if !a.discover.Allow("r" + strconv.FormatInt(auth.Current(c).ID, 10)) {
		httpx.Error(c, http.StatusTooManyRequests, "操作太频繁")
		return
	}
	if err := a.Ingest.DiscoverDoubanID(c.Request.Context(), body.DoubanID); err != nil {
		a.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ---------- 短评、相似推荐 ----------

func (a *App) mediaComments(c *gin.Context) {
	m, ok := a.loadMedia(c)
	if !ok {
		return
	}
	page, offset := httpx.Page(c, 10)
	comments, err := a.Social.MediaComments(c.Request.Context(), viewerID(c), m.ID, 10, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	more := ""
	if len(comments) == 10 {
		more = "/api/htmx/m/" + strconv.FormatInt(m.ID, 10) + "/comments?page=" + strconv.Itoa(page+1)
	}
	a.render.Partial(c, "media_comments", gin.H{"Comments": comments, "More": more, "First": page == 1, "MediaID": m.ID})
}

func (a *App) mediaSimilar(c *gin.Context) {
	m, ok := a.loadMedia(c)
	if !ok {
		return
	}
	cards, err := a.Media.Similar(c.Request.Context(), m, 16)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.row(c, RowView{Title: "相似推荐", Cards: cardViews(cards), More: mediaHref(m.ID) + "/similar"})
}

// ---------- 想看 / 看过 ----------

func (a *App) libraryActions(c *gin.Context, mediaID int64) {
	entry, hasEntry, err := a.Library.Get(c.Request.Context(), auth.Current(c).ID, mediaID)
	if err != nil {
		a.fail(c, err)
		return
	}
	if err := a.Recommend.Invalidate(c.Request.Context(), auth.Current(c).ID); err != nil {
		c.Error(err)
	}
	if !httpx.IsHTMX(c) {
		status := ""
		if hasEntry {
			status = entry.Status
		}
		c.JSON(http.StatusOK, gin.H{"status": status})
		return
	}
	c.Header("HX-Trigger", "close-modal")
	a.render.Partial(c, "library_actions", gin.H{"MediaID": mediaID, "Entry": entry, "HasEntry": hasEntry})
}

// toggleWish 加入或移出想看。已经标成看过的不受影响。
func (a *App) toggleWish(c *gin.Context) {
	m, ok := a.loadMedia(c)
	if !ok {
		return
	}
	ctx, user := c.Request.Context(), auth.Current(c)
	entry, hasEntry, err := a.Library.Get(ctx, user.ID, m.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	switch {
	case !hasEntry:
		if _, err = a.Library.Save(ctx, user.ID, m.ID, library.StatusWish, 0, ""); err == nil {
			a.queueOutbox(c, m, library.StatusWish, 0)
		}
	case entry.Status == library.StatusWish:
		err = a.Library.Remove(ctx, user.ID, m.ID)
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	a.libraryActions(c, m.ID)
}

func (a *App) markForm(c *gin.Context) {
	m, ok := a.loadMedia(c)
	if !ok {
		return
	}
	entry, _, err := a.Library.Get(c.Request.Context(), auth.Current(c).ID, m.ID)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.render.Partial(c, "mark_form", gin.H{"M": m, "Entry": entry})
}

func (a *App) markWatched(c *gin.Context) {
	m, ok := a.loadMedia(c)
	if !ok {
		return
	}
	rating, _ := strconv.Atoi(c.PostForm("rating"))
	user, comment := auth.Current(c), c.PostForm("comment")
	// 被禁言时可以照常标记和评分，但不能新写或修改短评
	if user.Muted() {
		if entry, found, _ := a.Library.Get(c.Request.Context(), user.ID, m.ID); strings.TrimSpace(comment) != "" && (!found || entry.Comment != comment) {
			httpx.Error(c, http.StatusForbidden, "你"+user.RestrictionText()+"，暂时不能写短评")
			return
		}
	}
	_, err := a.Library.Save(c.Request.Context(), user.ID, m.ID, library.StatusWatched, rating, comment)
	if errors.Is(err, library.ErrInvalid) {
		httpx.Error(c, http.StatusBadRequest, "评分或短评不合法")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	a.queueOutbox(c, m, library.StatusWatched, rating)
	a.libraryActions(c, m.ID)
}

func (a *App) removeMark(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	if err := a.Library.Remove(c.Request.Context(), auth.Current(c).ID, id); err != nil {
		a.fail(c, err)
		return
	}
	a.libraryActions(c, id)
}

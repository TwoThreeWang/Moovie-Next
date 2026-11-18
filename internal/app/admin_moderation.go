package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"moovie/internal/adskip"
	"moovie/internal/auth"
	"moovie/internal/httpx"
	"moovie/internal/social"
)

// 后台：内容审核（短评、回复、弹幕）、禁言 / 封禁、广告指纹裁定、运营趋势。

// ---------- 内容审核 ----------

type contentItem struct {
	ID          int64
	UserID      int64
	Username    string
	Restriction string
	MediaID     int64
	MediaTitle  string
	Text        string
	Where       string // 弹幕：第几集、第几秒
	At          time.Time
}

var contentKinds = []struct{ Key, Label string }{{"comments", "短评"}, {"replies", "回复"}, {"danmaku", "弹幕"}}

func (a *App) contentList(ctx context.Context, kind, q string, limit, offset int) ([]contentItem, int, error) {
	var query string
	switch kind {
	case "replies":
		query = `SELECT r.id, u.id, u.username, u.restriction, m.id, m.title, r.content, '', r.created_at, count(*) OVER ()
FROM comment_replies r JOIN users u ON u.id = r.user_id JOIN user_movies um ON um.id = r.user_movie_id JOIN media m ON m.id = um.media_id
WHERE ($1 = '' OR r.content ILIKE '%' || $1 || '%' OR u.username ILIKE '%' || $1 || '%') ORDER BY r.created_at DESC LIMIT $2 OFFSET $3`
	case "danmaku":
		query = `SELECT d.id, u.id, u.username, u.restriction, COALESCE(m.id, 0), COALESCE(m.title, split_part(d.title_key, '|', 1)), d.body,
'第 ' || d.episode || ' 集 · ' || floor(d.time_seconds / 60)::int || ':' || lpad((floor(d.time_seconds)::int % 60)::text, 2, '0'), d.created_at, count(*) OVER ()
FROM danmakus d JOIN users u ON u.id = d.user_id LEFT JOIN media m ON m.id = d.media_id
WHERE NOT d.deleted AND ($1 = '' OR d.body ILIKE '%' || $1 || '%' OR u.username ILIKE '%' || $1 || '%') ORDER BY d.created_at DESC LIMIT $2 OFFSET $3`
	default:
		query = `SELECT um.id, u.id, u.username, u.restriction, m.id, m.title, um.comment, '', um.updated_at, count(*) OVER ()
FROM user_movies um JOIN users u ON u.id = um.user_id JOIN media m ON m.id = um.media_id
WHERE um.comment <> '' AND ($1 = '' OR um.comment ILIKE '%' || $1 || '%' OR u.username ILIKE '%' || $1 || '%') ORDER BY um.updated_at DESC LIMIT $2 OFFSET $3`
	}
	rows, err := a.Pool.Query(ctx, query, strings.TrimSpace(q), limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items, total := []contentItem{}, 0
	for rows.Next() {
		var item contentItem
		if err := rows.Scan(&item.ID, &item.UserID, &item.Username, &item.Restriction, &item.MediaID, &item.MediaTitle, &item.Text, &item.Where, &item.At, &total); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func contentKind(c *gin.Context) string {
	kind := c.DefaultQuery("type", c.PostForm("type"))
	if kind != "replies" && kind != "danmaku" {
		kind = "comments"
	}
	return kind
}

func (a *App) adminContent(c *gin.Context) {
	kind := contentKind(c)
	_, offset := httpx.Page(c, adminPageSize)
	items, total, err := a.contentList(c.Request.Context(), kind, c.Query("q"), adminPageSize, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.adminPage(c, "admin_content.html", "内容审核", "content", gin.H{"Items": items, "Kind": kind, "Kinds": contentKinds,
		"Query": c.Query("q"), "Pager": pager(c, total)})
}

// adminContentDelete 批量删除。短评和回复会通知作者（附原因）；弹幕直接删。
func (a *App) adminContentDelete(c *gin.Context) {
	ctx, admin, kind := c.Request.Context(), auth.Current(c), contentKind(c)
	reason := strings.TrimSpace(c.PostForm("reason"))
	ids := []int64{}
	for _, raw := range c.PostFormArray("ids") {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		adminBack(c, "/admin/content?type="+kind, "没有选中任何内容")
		return
	}
	deleted := 0
	for _, id := range ids {
		var err error
		switch kind {
		case "replies":
			_, err = a.Social.DeleteReply(ctx, admin.ID, true, id, reason)
		case "danmaku":
			_, err = a.Pool.Exec(ctx, `UPDATE danmakus SET deleted = true WHERE id = $1`, id)
		default:
			err = a.Social.RemoveComment(ctx, admin.ID, id, reason)
		}
		if err == nil {
			deleted++
		}
	}
	adminBack(c, "/admin/content?type="+kind, "已删除 "+strconv.Itoa(deleted)+" 条")
}

// ---------- 禁言 / 封禁 ----------

func (a *App) adminUserRestrict(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	ctx, admin := c.Request.Context(), auth.Current(c)
	target, err := a.Users.ByID(ctx, id)
	if err != nil {
		httpx.Error(c, http.StatusNotFound, "用户不存在")
		return
	}
	if target.ID == admin.ID || target.IsAdmin() {
		adminBack(c, "/admin/users", "不能限制管理员（先取消他的管理员身份）")
		return
	}
	kind, reason := c.PostForm("kind"), strings.TrimSpace(c.PostForm("reason"))
	var until *time.Time
	if days, _ := strconv.Atoi(c.PostForm("days")); days > 0 && kind != "" {
		at := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		until = &at
	}
	if err := a.Users.SetRestriction(ctx, id, kind, until, reason); err != nil {
		adminBack(c, "/admin/users", err.Error())
		return
	}
	if kind == "mute" {
		target.Restriction, target.RestrictedUntil, target.RestrictionReason = kind, until, reason
		_ = a.Social.Notify(ctx, social.Notice{UserID: id, ActorID: admin.ID, Kind: "muted", Body: target.RestrictionText(), Reason: reason})
	}
	notice := map[string]string{"": "已解除限制", "mute": "已禁言", "ban": "已封禁，他在所有设备上的登录都已失效"}[kind]
	adminBack(c, "/admin/users", target.Username+" "+notice)
}

// ---------- 广告指纹 ----------
// 只读：管理员看不到视频内容，没有依据判断是不是广告，所以不提供人工裁定；
// 判断交给看到画面的观众投票（询问"这一段是广告吗"、自动跳过后的"这段不是广告"）。
// 这里用来看整体情况，发现异常（比如短时间内大量投票）。

func (a *App) adminFingerprints(c *gin.Context) {
	_, offset := httpx.Page(c, adminPageSize)
	status := c.Query("status")
	list, total, err := a.AdSkip.Admin(c.Request.Context(), status, adminPageSize, offset)
	if err != nil {
		a.fail(c, err)
		return
	}
	a.adminPage(c, "admin_fingerprints.html", "广告指纹", "fingerprints", gin.H{"List": list, "Status": status,
		"Pager": pager(c, total), "MinConfirm": adskip.MinConfirmCount, "MinReject": adskip.MinRejectCount})
}

// ---------- 运营趋势 ----------

type trendSeries struct {
	Label  string
	Total  int
	Today  int
	Bars   []int // 每天的数值占最大值的百分比
	Values []int
	Days   []string
}

// trends30 统计最近 30 天每天的数字。
func (a *App) trends30(ctx context.Context) ([]trendSeries, error) {
	metrics := []struct{ label, query string }{
		{"新用户", `SELECT count(*) FROM users WHERE created_at >= d AND created_at < d + 1`},
		{"观影人数", `SELECT count(*) FROM watch_days WHERE day = d`},
		{"新标记", `SELECT count(*) FROM user_movies WHERE created_at >= d AND created_at < d + 1`},
		{"回复", `SELECT count(*) FROM comment_replies WHERE created_at >= d AND created_at < d + 1`},
		{"弹幕", `SELECT count(*) FROM danmakus WHERE created_at >= d AND created_at < d + 1`},
		{"搜索", `SELECT COALESCE(sum(count), 0)::int FROM search_counts WHERE day = d`},
		{"发信", `SELECT count(*) FROM mail_log WHERE created_at >= d AND created_at < d + 1`},
	}
	series := make([]trendSeries, 0, len(metrics))
	for _, metric := range metrics {
		rows, err := a.Pool.Query(ctx, `SELECT to_char(d, 'MM-DD'), (`+metric.query+`)
FROM generate_series(current_date - 29, current_date, interval '1 day') AS g(day), LATERAL (SELECT g.day::date AS d) x ORDER BY g.day`)
		if err != nil {
			return nil, err
		}
		s := trendSeries{Label: metric.label}
		for rows.Next() {
			var day string
			var value int
			if err := rows.Scan(&day, &value); err != nil {
				rows.Close()
				return nil, err
			}
			s.Days, s.Values = append(s.Days, day), append(s.Values, value)
			s.Total += value
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		top := 0
		for _, value := range s.Values {
			top = max(top, value)
		}
		for _, value := range s.Values {
			percent := 0
			if top > 0 {
				percent = max(value*100/top, 2)
			}
			s.Bars = append(s.Bars, percent)
		}
		if len(s.Values) > 0 {
			s.Today = s.Values[len(s.Values)-1]
		}
		series = append(series, s)
	}
	return series, nil
}

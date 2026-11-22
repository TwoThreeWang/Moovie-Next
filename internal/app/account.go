package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"moovie/internal/auth"
	"moovie/internal/mail"
)

// 用户自己注销账号、导出数据（《个人信息保护法》第 45、47 条：查阅复制、删除）。

var errLastAdmin = errors.New("你是唯一的管理员，先在后台把别人设为管理员再注销")

// deleteAccount 删除账号和它的所有数据（外键级联）。
// 旧站的编辑片单交接给另一位管理员，个人片单随账号删除。唯一的管理员不能删。
func (a *App) deleteAccount(ctx context.Context, userID int64) error {
	return pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		var role string
		if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&role); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return auth.ErrNotFound
			}
			return err
		}
		var heir int64
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE role = 'admin' AND id <> $1 ORDER BY id LIMIT 1`, userID).Scan(&heir)
		switch {
		case errors.Is(err, pgx.ErrNoRows) && role == "admin":
			return errLastAdmin
		case err == nil:
			if _, err := tx.Exec(ctx, `UPDATE collections SET owner_user_id = $2, link_visible = false, updated_at = clock_timestamp()
WHERE owner_user_id = $1 AND legacy_editorial AND status = 'published'`, userID, heir); err != nil {
				return err
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		return err
	})
}

func (a *App) deleteOwnAccount(c *gin.Context) {
	ctx, user := c.Request.Context(), auth.Current(c)
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(c.PostForm("password"))) != nil {
		settingsDone(c, "danger", "password_wrong")
		return
	}
	if c.PostForm("confirm") != "注销" {
		settingsDone(c, "danger", "confirm_missing")
		return
	}
	err := a.deleteAccount(ctx, user.ID)
	if errors.Is(err, errLastAdmin) {
		settingsDone(c, "danger", "last_admin")
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	a.Sessions.SignOut(c)
	// 告诉本人一声：万一不是本人操作（比如密码泄露），也能知道。发不出去不影响结果。
	_ = a.Mail.Send(ctx, mail.Message{To: user.Email, Kind: "deleted", Security: true, Subject: a.Config.SiteName + "：账号已注销",
		Title: "账号已注销",
		Text: "你在" + a.Config.SiteName + "的账号「" + user.Username + "」已经注销，账号下的所有数据（想看、看过、评分、短评、回复、进度、加密历史、弹幕、通知）都已删除，无法恢复。\n\n" +
			"欢迎以后再来。"})
	a.render.Page(c, http.StatusOK, "message.html", gin.H{"Title": "账号已注销", "Heading": "账号已注销",
		"Message": "账号和所有数据都已删除。这台设备上保存的播放源和密钥也已清除。", "Bare": true,
		"ClearUser": user.ID, "Scripts": []string{"pages/goodbye.js"}})
}

// ---------- 导出 ----------

type exportEntry struct {
	Title    string    `json:"title"`
	Year     int       `json:"year,omitempty"`
	Type     string    `json:"type"`
	DoubanID string    `json:"douban_id,omitempty"`
	IMDbID   string    `json:"imdb_id,omitempty"`
	Status   string    `json:"status"`
	Rating   int       `json:"rating,omitempty"`
	Comment  string    `json:"comment,omitempty"`
	Marked   time.Time `json:"marked_at"`
	Updated  time.Time `json:"updated_at"`
	URL      string    `json:"url"`
}

type exportProgress struct {
	Title     string    `json:"title"`
	Season    int       `json:"season"`
	Episode   int       `json:"episode"`
	Position  float64   `json:"position_seconds"`
	Duration  float64   `json:"duration_seconds"`
	Completed bool      `json:"completed"`
	At        time.Time `json:"watched_at"`
}

type exportText struct {
	Title   string    `json:"title,omitempty"`
	Content string    `json:"content"`
	At      time.Time `json:"created_at"`
}

type exportFeedback struct {
	Kind    string    `json:"kind"`
	Content string    `json:"content"`
	Status  string    `json:"status"`
	Reply   string    `json:"reply,omitempty"`
	At      time.Time `json:"created_at"`
}

type exportData struct {
	ExportedAt time.Time `json:"exported_at"`
	Site       string    `json:"site"`
	Account    struct {
		Email     string    `json:"email"`
		Username  string    `json:"username"`
		Public    bool      `json:"public"`
		DoubanID  string    `json:"douban_user_id,omitempty"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"account"`
	Library             []exportEntry    `json:"library"`
	Progress            []exportProgress `json:"progress"`
	Replies             []exportText     `json:"replies"`
	Danmaku             []exportText     `json:"danmaku"`
	Following           []string         `json:"following"`
	Feedback            []exportFeedback `json:"feedback"`
	Collections         json.RawMessage  `json:"collections"`
	CollectionFavorites json.RawMessage  `json:"collection_favorites"`
	Note                string           `json:"private_history_note"`
}

// collectExport 读出一个账号在服务器上的所有数据。
func (a *App) collectExport(ctx context.Context, user *auth.User) (*exportData, error) {
	data := &exportData{ExportedAt: time.Now(), Site: a.Config.SiteURL,
		Library: []exportEntry{}, Progress: []exportProgress{}, Replies: []exportText{}, Danmaku: []exportText{},
		Following: []string{}, Feedback: []exportFeedback{},
		Note: "没有元数据的作品的观看历史在服务器上是加密保存的，服务器解不开。在网页上点导出时，浏览器会把它解密后附在 private_history 里。"}
	data.Account.Email, data.Account.Username, data.Account.Public = user.Email, user.Username, user.IsPublic
	data.Account.DoubanID, data.Account.CreatedAt = user.DoubanUserID, user.CreatedAt

	rows, err := a.Pool.Query(ctx, `SELECT m.id, m.title, m.year, m.media_type,
m.douban_id, m.imdb_id,
um.status, um.rating, um.comment, um.created_at, um.updated_at
FROM user_movies um JOIN media m ON m.id = um.media_id WHERE um.user_id = $1 ORDER BY um.updated_at DESC`, user.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e exportEntry
		var id int64
		if err := rows.Scan(&id, &e.Title, &e.Year, &e.Type, &e.DoubanID, &e.IMDbID, &e.Status, &e.Rating, &e.Comment, &e.Marked, &e.Updated); err != nil {
			rows.Close()
			return nil, err
		}
		e.URL = a.absolute(mediaHref(id))
		data.Library = append(data.Library, e)
	}
	rows.Close()

	rows, err = a.Pool.Query(ctx, `SELECT m.title, p.season, p.episode, p.position_seconds, p.duration_seconds, p.completed, p.activity_at
FROM playback_positions p JOIN media m ON m.id = p.media_id WHERE p.user_id = $1 ORDER BY p.activity_at DESC`, user.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p exportProgress
		if err := rows.Scan(&p.Title, &p.Season, &p.Episode, &p.Position, &p.Duration, &p.Completed, &p.At); err != nil {
			rows.Close()
			return nil, err
		}
		data.Progress = append(data.Progress, p)
	}
	rows.Close()

	texts := func(query string) ([]exportText, error) {
		rows, err := a.Pool.Query(ctx, query, user.ID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list := []exportText{}
		for rows.Next() {
			var t exportText
			if err := rows.Scan(&t.Title, &t.Content, &t.At); err != nil {
				return nil, err
			}
			list = append(list, t)
		}
		return list, rows.Err()
	}
	if data.Replies, err = texts(`SELECT m.title, r.content, r.created_at FROM comment_replies r
JOIN user_movies um ON um.id = r.user_movie_id JOIN media m ON m.id = um.media_id WHERE r.user_id = $1 ORDER BY r.created_at DESC`); err != nil {
		return nil, err
	}
	// 没有元数据的作品，弹幕按"片名|季|集"归档，这里取片名部分
	if data.Danmaku, err = texts(`SELECT COALESCE(m.title, split_part(d.title_key, '|', 1)), d.body, d.created_at FROM danmakus d
LEFT JOIN media m ON m.id = d.media_id WHERE d.user_id = $1 AND NOT d.deleted ORDER BY d.created_at DESC`); err != nil {
		return nil, err
	}

	rows, err = a.Pool.Query(ctx, `SELECT u.username FROM user_follows f JOIN users u ON u.id = f.followee_id WHERE f.follower_id = $1 ORDER BY u.username`, user.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		data.Following = append(data.Following, name)
	}
	rows.Close()

	feedbacks, err := a.Feedback.List(ctx, user.ID, "", 1000, 0)
	if err != nil {
		return nil, err
	}
	for _, f := range feedbacks {
		data.Feedback = append(data.Feedback, exportFeedback{Kind: f.Kind, Content: f.Content, Status: f.Status, Reply: f.Reply, At: f.CreatedAt})
	}
	if err := a.Pool.QueryRow(ctx, `SELECT
    COALESCE((SELECT jsonb_agg(to_jsonb(owned) ORDER BY owned.updated_at DESC, owned.id DESC) FROM (
        SELECT c.id, c.slug, c.title, c.description, c.status, c.link_visible, c.published_at, c.updated_at, c.revision AS pending_revision,
            COALESCE((SELECT jsonb_agg(jsonb_build_object('media_id', i.media_id, 'title', m.title, 'note', i.note) ORDER BY i.position)
                FROM collection_items i JOIN media m ON m.id = i.media_id WHERE i.collection_id = c.id), '[]') AS entries
        FROM collections c WHERE c.owner_user_id = $1
    ) owned), '[]'),
    COALESCE((SELECT jsonb_agg(jsonb_build_object('collection_id', f.collection_id, 'saved_at', f.created_at,
        'slug', CASE WHEN c.status = 'published' THEN c.slug END) ORDER BY f.created_at DESC, f.collection_id DESC)
        FROM collection_favorites f JOIN collections c ON c.id = f.collection_id WHERE f.user_id = $1 AND f.active), '[]')`,
		user.ID).Scan(&data.Collections, &data.CollectionFavorites); err != nil {
		return nil, err
	}
	return data, nil
}

func (a *App) exportJSON(c *gin.Context) {
	data, err := a.collectExport(c.Request.Context(), auth.Current(c))
	if err != nil {
		a.fail(c, err)
		return
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		a.fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="moovie-export-`+time.Now().Format("20060102")+`.json"`)
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json; charset=utf-8", encoded)
}

// exportCSV 只导出想看、看过（最常用的部分，可以用 Excel 打开）。
func (a *App) exportCSV(c *gin.Context) {
	data, err := a.collectExport(c.Request.Context(), auth.Current(c))
	if err != nil {
		a.fail(c, err)
		return
	}
	var buffer bytes.Buffer
	buffer.WriteString("\uFEFF") // 让 Excel 按 UTF-8 打开
	writer := csv.NewWriter(&buffer)
	writer.Write([]string{"片名", "年份", "类型", "豆瓣ID", "IMDb ID", "状态", "评分", "短评", "标记时间", "链接"})
	status := map[string]string{"wish": "想看", "watched": "看过"}
	for _, e := range data.Library {
		year, rating := "", ""
		if e.Year > 0 {
			year = strconv.Itoa(e.Year)
		}
		if e.Rating > 0 {
			rating = strconv.Itoa(e.Rating)
		}
		writer.Write([]string{csvSafe(e.Title), year, e.Type, e.DoubanID, e.IMDbID, status[e.Status], rating, csvSafe(e.Comment), e.Updated.Format("2006-01-02 15:04"), e.URL})
	}
	writer.Flush()
	c.Header("Content-Disposition", `attachment; filename="moovie-library-`+time.Now().Format("20060102")+`.csv"`)
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/csv; charset=utf-8", buffer.Bytes())
}

// csvSafe 防止表格软件把 = + - @ 开头的内容当公式执行（短评是用户写的）。
func csvSafe(value string) string {
	if value != "" && (value[0] == '=' || value[0] == '+' || value[0] == '-' || value[0] == '@' || value[0] == '\t' || value[0] == '\r') {
		return "'" + value
	}
	return value
}

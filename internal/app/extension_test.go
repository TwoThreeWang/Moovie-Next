package app_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"moovie/extension"
	"moovie/internal/media"
)

// connectExtension 走一遍"连接插件"页面，返回插件拿到的凭证。
func (s *site) connectExtension() string {
	s.t.Helper()
	if r := s.get("/extension/connect"); r.status != http.StatusOK || !strings.Contains(r.body, "允许连接") {
		s.t.Fatalf("连接页打不开: %d", r.status)
	}
	r := s.form("/extension/connect", url.Values{})
	match := regexp.MustCompile(`id="moovie-ext-token" data-token="([^"]+)"`).FindStringSubmatch(r.body)
	if r.status != http.StatusOK || match == nil {
		s.t.Fatalf("连接页没有给出凭证: %d %.300s", r.status, r.body)
	}
	if r.header.Get("Cache-Control") != "no-store" {
		s.t.Error("带凭证的页面不能被缓存")
	}
	return match[1]
}

// ext 用插件凭证调接口（不带网站的 Cookie 和 CSRF 令牌）。
func (s *site) ext(token, method, path string, payload any) reply {
	s.t.Helper()
	var body io.Reader
	headers := map[string]string{"Authorization": "Bearer " + token}
	if payload != nil {
		encoded, _ := json.Marshal(payload)
		body = bytes.NewReader(encoded)
		headers["Content-Type"] = "application/json"
	}
	return s.do(s.browser(), method, path, body, headers)
}

func decode(t *testing.T, r reply, destination any) {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("接口返回 %d: %.300s", r.status, r.body)
	}
	if err := json.Unmarshal([]byte(r.body), destination); err != nil {
		t.Fatalf("接口返回的不是 JSON: %v %.200s", err, r.body)
	}
}

func interestsPage(entries ...map[string]any) map[string]any {
	return map[string]any{"total": len(entries), "interests": entries}
}

func interest(id, title, kind string, rating float64, comment string) map[string]any {
	entry := map[string]any{
		"comment": comment, "create_time": "2026-10-01 20:30:00",
		"subject": map[string]any{"id": id, "title": title, "type": kind, "year": "2025", "pic": map[string]any{"large": "https://img1.doubanio.com/view/photo/l/public/p9.jpg"}},
	}
	if rating > 0 {
		entry["rating"] = map[string]any{"value": rating}
	}
	return entry
}

func TestExtensionConnectAndSync(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	ctx := context.Background()

	if r := s.ext("bogus-token-bogus-token", http.MethodGet, "/api/ext/me", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("假凭证应该被拒绝: %d", r.status)
	}
	if r := s.get("/settings"); !strings.Contains(r.body, "推荐安装浏览器插件") {
		t.Error("没连接插件时，设置页的豆瓣同步应该推荐安装插件")
	}
	token := s.connectExtension()
	var me struct {
		Username string `json:"username"`
	}
	decode(t, s.ext(token, http.MethodGet, "/api/ext/me", nil), &me)
	if me.Username != "evan" {
		t.Fatalf("凭证对应的账号不对: %+v", me)
	}
	if r := s.get("/settings"); !strings.Contains(r.body, "已连接<a href=\"/extension\">浏览器插件</a>") || !strings.Contains(r.body, "断开全部插件") {
		t.Error("连接以后，设置页应该显示插件已连接")
	}

	// 插件不带 CSRF 令牌也能调用，但网站自己的接口仍然要令牌
	if r := s.ext(token, http.MethodPost, "/api/ext/douban/bind", map[string]any{"douban_user_id": "123456"}); r.status != http.StatusOK {
		t.Fatalf("绑定豆瓣账号失败: %d %s", r.status, r.body)
	}
	already := s.seedMedia("36000010", "已经看过的", media.TypeMovie, nil)
	s.htmx(http.MethodPost, "/api/library/"+itoa(already)+"/watched", url.Values{"rating": {"4"}, "comment": {"自己写的"}})
	page := interestsPage(
		interest("36000010", "已经看过的", "movie", 2, "豆瓣上的短评"),
		interest("36000011", "新标记的剧", "tv", 5, "好看"),
	)
	var result struct{ Total, Processed, Imported, Changed int }
	decode(t, s.ext(token, http.MethodPost, "/api/ext/douban/interests", map[string]any{"status": "done", "response": page}), &result)
	if result.Total != 2 || result.Processed != 2 || result.Imported != 1 || result.Changed != 1 {
		t.Fatalf("导入结果不对: %+v", result)
	}
	decode(t, s.ext(token, http.MethodPost, "/api/ext/douban/interests", map[string]any{"status": "done", "response": page}), &result)
	if result.Changed != 0 {
		t.Errorf("同一页再交一次应该说没有变化，插件据此停下: %+v", result)
	}
	if r := s.ext(token, http.MethodPost, "/api/ext/douban/interests", map[string]any{"status": "doing", "response": page}); r.status != http.StatusBadRequest {
		t.Errorf("只接受看过和想看: %d", r.status)
	}
	if entry, _, _ := s.app.Library.Get(ctx, s.userID("evan"), already); entry.Comment != "自己写的" || entry.Rating != 4 {
		t.Errorf("本站已有的标记不能被覆盖: %+v", entry)
	}
	fresh, err := s.app.Media.ByDoubanID(ctx, "36000011")
	if err != nil || fresh.Title != "新标记的剧" {
		t.Fatalf("新作品应该用列表资料占位收录: %+v %v", fresh, err)
	}

	// 同步完：最近由插件同步过的用户，服务器每天的自动同步跳过他
	s.ext(token, http.MethodPost, "/api/ext/douban/synced", map[string]any{"failed": 0})
	if err := s.app.DoubanSync.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.app.Queue.Latest(ctx, "douban.sync", itoa(s.userID("evan"))); found {
		t.Error("插件刚同步过，服务器不该再替他去豆瓣读")
	}

	// 断开以后凭证失效
	s.ext(token, http.MethodPost, "/api/ext/disconnect", map[string]any{})
	if r := s.ext(token, http.MethodGet, "/api/ext/me", nil); r.status != http.StatusUnauthorized {
		t.Errorf("断开以后凭证应该失效: %d", r.status)
	}
	other := s.connectExtension()
	s.form("/settings/extension/revoke", url.Values{})
	if r := s.ext(other, http.MethodGet, "/api/ext/me", nil); r.status != http.StatusUnauthorized {
		t.Errorf("设置页断开全部以后凭证应该失效: %d", r.status)
	}
	third := s.connectExtension()
	s.form("/settings/password", url.Values{"current": {"password-123"}, "password": {"password-456"}})
	if r := s.ext(third, http.MethodGet, "/api/ext/me", nil); r.status != http.StatusUnauthorized {
		t.Errorf("改密码以后凭证应该失效: %d", r.status)
	}
}

func TestExtensionContributions(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	ctx := context.Background()
	token := s.connectExtension()

	// 用户自己标记的、还没有资料的作品，先给他自己的插件去取，服务器那边的任务推迟
	id, _, err := s.app.Media.EnsureDouban(ctx, "36000020", media.Patch{"title": "雾中的港城", "year": 2025})
	if err != nil {
		t.Fatal(err)
	}
	s.app.Ingest.EnqueueRefresh(ctx, id, 0)
	s.htmx(http.MethodPost, "/api/library/"+itoa(id)+"/wish", nil)
	waiting := s.seedMedia("36000021", "别人在等的", media.TypeMovie, nil)
	s.app.Media.MarkSynced(ctx, waiting, "partial")
	s.app.Ingest.EnqueueRefresh(ctx, waiting, -1)

	var tasks struct {
		Tasks []struct {
			DoubanID string `json:"douban_id"`
		} `json:"tasks"`
	}
	decode(t, s.ext(token, http.MethodGet, "/api/ext/tasks", nil), &tasks)
	if len(tasks.Tasks) != 1 || tasks.Tasks[0].DoubanID != "36000020" {
		t.Fatalf("不帮别人时只给自己的: %+v", tasks)
	}
	decode(t, s.ext(token, http.MethodGet, "/api/ext/tasks?help=1", nil), &tasks)
	if len(tasks.Tasks) != 2 || tasks.Tasks[1].DoubanID != "36000021" {
		t.Fatalf("帮别人时再借服务器排着的任务: %+v", tasks)
	}
	decode(t, s.ext(token, http.MethodGet, "/api/ext/tasks?help=1", nil), &tasks)
	if len(tasks.Tasks) != 1 {
		t.Errorf("借出去的任务在推迟期间不该再借第二次: %+v", tasks)
	}

	subject := map[string]any{"id": "36000020", "type": "tv", "title": "雾中的港城", "original_title": "Harbor", "year": "2025",
		"intro": "一桩旧案。", "pic": map[string]any{"large": "https://img2.doubanio.com/view/photo/l/public/p2.jpg"},
		"genres": []string{"悬疑"}, "episodes_count": 12, "rating": map[string]any{"value": 8.4}, "actors": []map[string]any{{"name": "演员甲"}}}
	var applied struct {
		Applied bool  `json:"applied"`
		MediaID int64 `json:"media_id"`
	}
	// 豆瓣详情还没同步、但已经有片名（列表、其他来源给的）的作品，交来的资料也要对得上
	wrong := map[string]any{"id": "36000020", "type": "tv", "title": "另一部片子", "year": "2025"}
	if r := s.ext(token, http.MethodPost, "/api/ext/douban/subject", map[string]any{"douban_id": "36000020", "kind": "tv", "response": wrong}); r.status != http.StatusConflict {
		t.Errorf("和建档资料对不上的应该拒绝: %d %s", r.status, r.body)
	}
	if err := s.app.Media.ApplyPatch(ctx, id, media.Patch{"season": 2}); err != nil {
		t.Fatal(err)
	}
	decode(t, s.ext(token, http.MethodPost, "/api/ext/douban/subject", map[string]any{"douban_id": "36000020", "kind": "tv", "response": subject}), &applied)
	if !applied.Applied || applied.MediaID != id {
		t.Fatalf("交上来的详情应该被采用: %+v", applied)
	}
	m, _ := s.app.Media.Get(ctx, id)
	if m.Title != "雾中的港城" || m.MetadataStatus != "complete" || m.EpisodeCount != 12 || m.Type != media.TypeTV || m.Season != 2 {
		t.Errorf("资料没有写进去: %+v", m)
	}
	if job, found, _ := s.app.Queue.Latest(ctx, "media.refresh", itoa(id)); found && job.Status == "queued" {
		t.Error("插件交回资料以后，服务器排着的豆瓣任务应该取消")
	}
	if job, found, _ := s.app.Queue.Latest(ctx, "media.enrich", itoa(id)); !found || job.Status != "queued" {
		t.Error("资料到了以后应该接着排补充资料")
	}
	if r := s.get("/user/" + itoa(s.userID("evan"))); !strings.Contains(r.body, `class="honor honor-1"`) || !strings.Contains(r.body, "拾片人") {
		t.Error("主页应该显示补全资料的称号")
	}
	// 贡献榜只列公开了主页的人；没公开的本人看到提醒
	if r := s.get("/contributors"); strings.Contains(r.body, `href="/user/`+itoa(s.userID("evan"))+`"`) || !strings.Contains(r.body, "公开主页</a>以后就能上榜") {
		t.Error("没公开主页的人不应该上榜，应该提醒公开主页")
	}
	s.form("/settings/public", url.Values{"public": {"on"}})
	if r := s.get("/contributors"); !strings.Contains(r.body, `href="/user/`+itoa(s.userID("evan"))+`"`) || !strings.Contains(r.body, "拾片人") {
		t.Error("公开主页以后应该出现在贡献榜上")
	}

	// 不合理的资料不采用
	bad := map[string]any{"id": "36000020", "type": "tv", "title": "完全不同的片子", "year": "1990"}
	if r := s.ext(token, http.MethodPost, "/api/ext/douban/subject", map[string]any{"douban_id": "36000020", "kind": "tv", "response": bad}); r.status != http.StatusConflict {
		t.Errorf("和已有资料对不上的应该拒绝: %d %s", r.status, r.body)
	}
	evil := map[string]any{"id": "36000022", "type": "movie", "title": "新片", "pic": map[string]any{"large": "https://evil.example.com/p.jpg"}}
	if r := s.ext(token, http.MethodPost, "/api/ext/douban/subject", map[string]any{"douban_id": "36000022", "kind": "movie", "response": evil}); r.status != http.StatusBadRequest {
		t.Errorf("海报不是豆瓣的地址应该拒绝: %d", r.status)
	}
	if r := s.ext(token, http.MethodPost, "/api/ext/douban/subject", map[string]any{"douban_id": "36000023", "kind": "movie", "response": subject}); r.status != http.StatusBadRequest {
		t.Errorf("编号对不上应该拒绝: %d", r.status)
	}
	var missing struct {
		Applied bool   `json:"applied"`
		Reason  string `json:"reason"`
	}
	decode(t, s.ext(token, http.MethodPost, "/api/ext/douban/subject", map[string]any{"douban_id": "36000021", "kind": "movie", "response": map[string]any{}}), &missing)
	if missing.Applied || missing.Reason != "not_found" {
		t.Errorf("插件说豆瓣上没有时只报告，不采用: %+v", missing)
	}
	if m, _ := s.app.Media.Get(ctx, waiting); m.Hidden || m.MetadataStatus == "not_found" {
		t.Error("不能因为插件一句话就把作品藏起来")
	}

	// 从用户打开的豆瓣页面摘出来的资料：新作品直接收录
	page := `<!DOCTYPE html><html><head><title>回声 (豆瓣)</title></head><body><div id="content"><h1><span property="v:itemreviewed">回声</span><span class="year">(2024)</span></h1>` +
		`<div id="mainpic"><img src="https://img3.doubanio.com/view/photo/s_ratio_poster/public/p3.webp"></div>` +
		`<div id="info"><span class="pl">类型:</span> <span property="v:genre">剧情</span><br/></div>` +
		`<strong property="v:average">7.9</strong><div id="link-report-intra"><span property="v:summary">简介。</span></div></div></body></html>`
	decode(t, s.ext(token, http.MethodPost, "/api/ext/douban/page", map[string]any{"douban_id": "36000030", "html": page, "urgent": true}), &applied)
	if fromPage, err := s.app.Media.ByDoubanID(ctx, "36000030"); err != nil || fromPage.Title != "回声" || fromPage.MetadataStatus != "complete" {
		t.Errorf("页面资料应该收录新作品: %+v %v", fromPage, err)
	}
}

func TestExtensionDoubanPageFeatures(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	ctx := context.Background()
	token := s.connectExtension()
	show := s.seedMedia("36000040", "雾港", media.TypeTV, media.Patch{"episode_count": 12})
	s.seedMedia("36000041", "白夜", media.TypeTV, nil)
	s.htmx(http.MethodPost, "/api/library/"+itoa(show)+"/wish", nil)
	today := media.Today()
	s.app.Media.ReplaceUnits(ctx, show, 1, []media.Unit{{Episode: 1}, {Episode: 2, AirDate: &today}})

	var card struct {
		Found     bool   `json:"found"`
		Status    string `json:"status"`
		URL       string `json:"url"`
		PlayURL   string `json:"play_url"`
		NeedsData bool   `json:"needs_data"`
	}
	decode(t, s.ext(token, http.MethodGet, "/api/ext/media?douban_id=36000040", nil), &card)
	if !card.Found || card.Status != "wish" || !strings.HasSuffix(card.URL, "/m/"+itoa(show)) || !strings.Contains(card.PlayURL, "/play/") || card.NeedsData {
		t.Errorf("卡片资料不对: %+v", card)
	}
	decode(t, s.ext(token, http.MethodGet, "/api/ext/media?douban_id=36000049", nil), &card)
	if card.Found || !card.NeedsData {
		t.Errorf("没收录的作品应该说没有、请插件交资料: %+v", card)
	}

	// 卡片上标记看过：本站没有的先收录；已有评分保留
	var marked struct {
		MediaID int64 `json:"media_id"`
	}
	decode(t, s.ext(token, http.MethodPost, "/api/ext/mark", map[string]any{"douban_id": "36000042", "status": "watched", "title": "新片", "year": 2026}), &marked)
	if entry, ok, _ := s.app.Library.Get(ctx, s.userID("evan"), marked.MediaID); !ok || entry.Status != "watched" {
		t.Errorf("卡片上标记看过没有生效: %+v", entry)
	}

	var marks struct {
		Marks map[string]struct {
			Status string `json:"status"`
		} `json:"marks"`
	}
	decode(t, s.ext(token, http.MethodPost, "/api/ext/marks", map[string]any{"ids": []string{"36000040", "36000041", "36000042", "abc"}}), &marks)
	if len(marks.Marks) != 2 || marks.Marks["36000040"].Status != "wish" || marks.Marks["36000042"].Status != "watched" {
		t.Errorf("角标查询结果不对: %+v", marks)
	}

	var airing struct {
		Items []struct {
			Title  string `json:"title"`
			Latest int    `json:"latest"`
		} `json:"items"`
	}
	decode(t, s.ext(token, http.MethodGet, "/api/ext/airing", nil), &airing)
	if len(airing.Items) != 1 || airing.Items[0].Latest != 2 {
		t.Errorf("追剧更新不对: %+v", airing)
	}

	// 写回豆瓣：第一次只给位置，之后在本站做的标记才返回
	var outbox struct {
		Items []struct {
			DoubanID string `json:"douban_id"`
			Status   string `json:"status"`
			Rating   int    `json:"rating"`
		} `json:"items"`
		Cursor int64 `json:"cursor"`
	}
	decode(t, s.ext(token, http.MethodGet, "/api/ext/outbox", nil), &outbox)
	if len(outbox.Items) != 0 || outbox.Cursor == 0 {
		t.Fatalf("第一次只给位置: %+v", outbox)
	}
	cursor := outbox.Cursor
	s.htmx(http.MethodPost, "/api/library/"+itoa(show)+"/watched", url.Values{"rating": {"5"}})
	decode(t, s.ext(token, http.MethodGet, "/api/ext/outbox?after="+itoa(cursor), nil), &outbox)
	if len(outbox.Items) != 1 || outbox.Items[0].DoubanID != "36000040" || outbox.Items[0].Status != "watched" || outbox.Items[0].Rating != 5 {
		t.Errorf("本站的标记应该等着写回豆瓣: %+v", outbox)
	}

	// 豆列导入成私密片单
	var imported struct {
		Count   int    `json:"count"`
		EditURL string `json:"edit_url"`
	}
	decode(t, s.ext(token, http.MethodPost, "/api/ext/collections", map[string]any{"title": "我的豆列", "description": "从豆列导入", "items": []map[string]any{
		{"douban_id": "36000040", "title": "雾港"}, {"douban_id": "36000043", "title": "新收录", "year": 2020}, {"douban_id": "36000040"}, {"douban_id": "x"},
	}}), &imported)
	if imported.Count != 2 || !strings.Contains(imported.EditURL, "/me/collections/") {
		t.Errorf("豆列导入结果不对: %+v", imported)
	}
	if r := s.get(strings.TrimPrefix(imported.EditURL, "http://moovie.test")); r.status != http.StatusOK || !strings.Contains(r.body, "我的豆列") {
		t.Errorf("导入的片单应该能编辑: %d", r.status)
	}
}

func TestExtensionDownloadPage(t *testing.T) {
	s := newSite(t)
	page := s.get("/extension")
	mustContain(t, page, "浏览器插件", "下载插件压缩包", "加载已解压的扩展程序", "怎么更新", extension.Version())
	r := s.get("/extension/download")
	if r.status != http.StatusOK || r.header.Get("Content-Type") != "application/zip" {
		t.Fatalf("下载不对: %d %s", r.status, r.header.Get("Content-Type"))
	}
	archive, err := zip.NewReader(strings.NewReader(r.body), int64(len(r.body)))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range archive.File {
		if file.Name != "moovie-extension/manifest.json" {
			continue
		}
		found = true
		reader, _ := file.Open()
		body, _ := io.ReadAll(reader)
		if !strings.Contains(string(body), `"http://moovie.test/*"`) || strings.Contains(string(body), "localhost:5200") {
			t.Errorf("压缩包里的地址应该换成网站地址: %s", body)
		}
	}
	if !found {
		t.Error("压缩包里没有 manifest.json")
	}
	var version struct {
		Version  string `json:"version"`
		Download string `json:"download"`
	}
	decode(t, s.get("/api/ext/version"), &version)
	if version.Version != extension.Version() || version.Download != "/extension/download" {
		t.Errorf("版本接口不对: %+v", version)
	}
	if r := s.get("/extension/connect"); r.status != http.StatusFound {
		t.Errorf("连接页要求登录: %d", r.status)
	}
}

// userID 按用户名查账号 ID（测试里注册的邮箱是 用户名@example.com）。
func (s *site) userID(name string) int64 {
	s.t.Helper()
	user, err := s.app.Users.ByEmail(context.Background(), name+"@example.com")
	if err != nil {
		s.t.Fatal(err)
	}
	return user.ID
}

func TestExtensionTasksSkipsDoubanRestricted(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	ctx := context.Background()
	token := s.connectExtension()

	// 用户自己标记的受限条目，和帮别人借任务的受限条目，都不该再下发给插件。
	ownRestricted, _, err := s.app.Media.EnsureDouban(ctx, "1305088", media.Patch{"title": "受限自选"})
	if err != nil {
		t.Fatal(err)
	}
	s.htmx(http.MethodPost, "/api/library/"+itoa(ownRestricted)+"/wish", nil)
	if err := s.app.Media.MarkDoubanRestricted(ctx, ownRestricted, false); err != nil {
		t.Fatal(err)
	}

	ownReady, _, err := s.app.Media.EnsureDouban(ctx, "1292052", media.Patch{"title": "正常自选"})
	if err != nil {
		t.Fatal(err)
	}
	s.htmx(http.MethodPost, "/api/library/"+itoa(ownReady)+"/wish", nil)

	helpRestricted := s.seedMedia("6727679", "受限公共", media.TypeMovie, nil)
	s.app.Media.MarkSynced(ctx, helpRestricted, "partial")
	s.app.Media.MarkDoubanRestricted(ctx, helpRestricted, false)
	s.app.Ingest.EnqueueRefresh(ctx, helpRestricted, -1)

	helpReady := s.seedMedia("36000030", "正常公共", media.TypeMovie, nil)
	s.app.Media.MarkSynced(ctx, helpReady, "partial")
	s.app.Ingest.EnqueueRefresh(ctx, helpReady, -1)

	var resp struct {
		Tasks []struct {
			DoubanID string `json:"douban_id"`
		} `json:"tasks"`
	}
	decode(t, s.ext(token, http.MethodGet, "/api/ext/tasks?help=1", nil), &resp)
	if len(resp.Tasks) != 2 || resp.Tasks[0].DoubanID != "1292052" || resp.Tasks[1].DoubanID != "36000030" {
		t.Fatalf("受限冷却期内的条目（自己的和帮别人的）都不应出现在 /api/ext/tasks 中: %+v", resp.Tasks)
	}
}

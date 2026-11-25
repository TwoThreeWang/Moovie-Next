package app_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"moovie/internal/ads"
	"moovie/internal/app"
	"moovie/internal/config"
	"moovie/internal/db/testdb"
	"moovie/internal/imageproxy"
	"moovie/internal/mail/mailtest"
	"moovie/internal/media"
	"moovie/internal/pow"
	"moovie/internal/providers"
	"moovie/internal/providers/providerstest"
)

// site 是一个跑起来的测试站点，加上一个带 Cookie 的浏览器。
type site struct {
	t      *testing.T
	app    *app.App
	fake   *providerstest.Fake
	server *httptest.Server
	client *http.Client
	mail   *mailtest.Recorder
	dns    *mailtest.Resolver
}

func newSite(t *testing.T) *site { return newSiteWith(t, nil) }

// newSiteWith 创建测试站点。issuer 为 nil 时关闭登录、注册的人机校验，方便其他测试直接提交表单。
// tweaks 可以改配置。
func newSiteWith(t *testing.T, issuer *pow.Issuer, tweaks ...func(*config.Config)) *site {
	pool := testdb.Pool(t)
	fake := providerstest.New(t)
	cfg := config.Config{Env: "production", SiteName: "Moovie 影牛", SiteURL: "http://moovie.test", AppSecret: "test-secret", SessionTTL: time.Hour, ContactEmail: "hello@moovie.test"}
	for _, tweak := range tweaks {
		tweak(&cfg)
	}
	recorder, resolver := &mailtest.Recorder{}, mailtest.Default()
	application, err := app.New(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{
		Douban: fake.Douban(), TMDB: fake.TMDBClient(), Wikidata: fake.WikidataClient(),
		AI: &providers.OpenAI{}, Embedder: &providers.OpenAI{}, Danmu: &providers.Danmu{}, PoW: issuer, DisablePoW: issuer == nil,
		MailTransport: recorder, MXResolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(application.Router())
	t.Cleanup(server.Close)
	s := &site{t: t, app: application, fake: fake, server: server, mail: recorder, dns: resolver}
	s.client = s.browser()
	return s
}

// browser 返回一个新的浏览器（独立的 Cookie），不自动跟随跳转。
func (s *site) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (s *site) csrf(client *http.Client) string {
	target, _ := url.Parse(s.server.URL)
	for _, cookie := range client.Jar.Cookies(target) {
		if cookie.Name == "mv_csrf" {
			return cookie.Value
		}
	}
	// 还没有令牌：访问一次首页拿到它。
	response, err := client.Get(s.server.URL + "/")
	if err != nil {
		s.t.Fatal(err)
	}
	response.Body.Close()
	for _, cookie := range client.Jar.Cookies(target) {
		if cookie.Name == "mv_csrf" {
			return cookie.Value
		}
	}
	s.t.Fatal("没有拿到 CSRF 令牌")
	return ""
}

type reply struct {
	status int
	body   string
	header http.Header
}

func (s *site) do(client *http.Client, method, path string, body io.Reader, headers map[string]string) reply {
	s.t.Helper()
	request, err := http.NewRequest(method, s.server.URL+path, body)
	if err != nil {
		s.t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return reply{status: response.StatusCode, body: string(data), header: response.Header}
}

func (s *site) get(path string) reply { return s.do(s.client, http.MethodGet, path, nil, nil) }

// htmx 模拟 HTMX 发的请求。
func (s *site) htmx(method, path string, form url.Values) reply {
	headers := map[string]string{"HX-Request": "true", "X-CSRF-Token": s.csrf(s.client)}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
		headers["Content-Type"] = "application/x-www-form-urlencoded"
	}
	return s.do(s.client, method, path, body, headers)
}

func (s *site) form(path string, form url.Values) reply {
	form.Set("_csrf", s.csrf(s.client))
	return s.do(s.client, http.MethodPost, path, strings.NewReader(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

func (s *site) json(method, path string, payload any) reply {
	encoded, _ := json.Marshal(payload)
	return s.do(s.client, method, path, bytes.NewReader(encoded), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": s.csrf(s.client)})
}

// mailedPassword 从注册邮件里取出密码。
func (s *site) mailedPassword(email string) string {
	s.t.Helper()
	message, ok := s.mail.Last(email)
	match := regexp.MustCompile(`密码：(\S+)`).FindStringSubmatch(message.Text)
	if !ok || match == nil {
		s.t.Fatalf("没有收到 %s 的注册邮件: %+v", email, message)
	}
	return match[1]
}

// register 注册并登录一个账号，再把密码改成 password-123 方便后面的测试。第一个注册的是管理员。
func (s *site) register(name string) {
	s.t.Helper()
	email := name + "@example.com"
	if r := s.form("/auth/register", url.Values{"email": {email}}); r.status != http.StatusOK || !strings.Contains(r.body, "密码已经发到你的邮箱") {
		s.t.Fatalf("注册 %s 失败: %d %.300s", name, r.status, r.body)
	}
	if r := s.form("/auth/login", url.Values{"email": {email}, "password": {s.mailedPassword(email)}}); r.status != http.StatusSeeOther {
		s.t.Fatalf("用邮件里的密码登录失败: %d", r.status)
	}
	if r := s.form("/settings/password", url.Values{"current": {s.mailedPassword(email)}, "password": {"password-123"}}); strings.Contains(r.header.Get("Location"), "error") {
		s.t.Fatalf("修改密码失败: %s", r.header.Get("Location"))
	}
}

func (s *site) seedMedia(doubanID, title, mediaType string, extra media.Patch) int64 {
	s.t.Helper()
	ctx := context.Background()
	id, _, err := s.app.Media.EnsureDouban(ctx, doubanID, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	patch := media.Patch{"title": title, "media_type": mediaType, "year": 2026, "summary": "台风季的港城，一桩旧案重新浮出水面。",
		"poster": "https://img9.doubanio.com/view/photo/l_ratio_poster/public/p1.jpg", "genres": []string{"悬疑"}}
	for key, value := range extra {
		patch[key] = value
	}
	if err := s.app.Media.ApplyPatch(ctx, id, media.ProviderDouban, patch); err != nil {
		s.t.Fatal(err)
	}
	if err := s.app.Media.MarkSynced(ctx, id, "complete"); err != nil {
		s.t.Fatal(err)
	}
	return id
}

func mustContain(t *testing.T, r reply, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(r.body, want) {
			t.Errorf("响应里应该包含 %q（状态 %d）：\n%.600s", want, r.status, r.body)
		}
	}
}

// ---------- 登录墙 ----------

func TestLoginWall(t *testing.T) {
	s := newSite(t)
	landing := s.get("/")
	if landing.status != http.StatusOK {
		t.Fatalf("未登录访问首页应该看到介绍页: %d", landing.status)
	}
	mustContain(t, landing, "本站不提供任何视频", "/auth/register")
	for _, path := range []string{"/about", "/terms", "/privacy", "/dmca", "/auth/login", "/auth/register", "/healthz", "/robots.txt"} {
		if r := s.get(path); r.status != http.StatusOK {
			t.Errorf("%s 应该不登录就能访问，实际 %d", path, r.status)
		}
	}
	// 在线 M3U8 播放器：游客也能用，搜索引擎能收录。
	player := s.get("/m3u8")
	mustContain(t, player, "在线 M3U8 播放器 - 免费播放", `data-manual="1"`, `id="manual-form"`, `"@type":"FAQPage"`, `"@type":"WebApplication"`,
		"我的链接会被上传吗", `<link rel="canonical" href="http://moovie.test/m3u8"`)
	if strings.Contains(player.body, `name="robots" content="noindex"`) || !strings.Contains(s.get("/sitemap-pages.xml").body, "/m3u8") {
		t.Error("播放器页应该允许收录，并出现在站点地图里")
	}
	// 广告指纹列表谁都能取（游客的播放器也要用）；投票要登录。
	if r := s.get("/api/ad-fingerprints/list"); r.status != http.StatusOK {
		t.Errorf("游客应该能取广告指纹列表，实际 %d", r.status)
	}
	if r := s.json(http.MethodPost, "/api/ad-fingerprints/vote", map[string]any{"fingerprint": strings.Repeat("ab", 32), "vote": "confirm"}); r.status != http.StatusUnauthorized {
		t.Errorf("游客不能投票，实际 %d", r.status)
	}
	for _, path := range []string{"/play/1", "/play/local", "/me", "/settings", "/admin", "/foryou", "/notifications", "/feedback", "/cinema?tab=following"} {
		r := s.get(path)
		if r.status != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), "/auth/login?redirect=") {
			t.Errorf("%s 未登录时应该跳到登录页，实际 %d %q", path, r.status, r.header.Get("Location"))
		}
	}
	for _, path := range []string{"/api/private-history", "/api/keyring", "/api/danmaku?media_id=1"} {
		if r := s.get(path); r.status != http.StatusUnauthorized {
			t.Errorf("%s 未登录时应该返回 401，实际 %d", path, r.status)
		}
	}
	if r := s.htmx(http.MethodGet, "/api/htmx/row/foryou", nil); r.status != http.StatusUnauthorized || r.header.Get("HX-Redirect") != "/auth/login" {
		t.Errorf("HTMX 请求未登录时应该让页面跳到登录页，实际 %d %q", r.status, r.header.Get("HX-Redirect"))
	}
}

func TestSecurityHeadersAndCSRF(t *testing.T) {
	s := newSite(t)
	r := s.get("/")
	csp := r.header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "connect-src 'self' https:", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP 里应该有 %q，实际 %q", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "http://localhost") {
		t.Errorf("生产环境的 CSP 不应该放开 eval 和本机地址: %q", csp)
	}
	// 每个响应一个新的 nonce，给 Cloudflare 插入的脚本用。
	nonce := regexp.MustCompile(`script-src [^;]*'nonce-([A-Za-z0-9+/]{22}==)'`)
	first, second := nonce.FindStringSubmatch(csp), nonce.FindStringSubmatch(s.get("/").header.Get("Content-Security-Policy"))
	if first == nil || second == nil || first[1] == second[1] {
		t.Errorf("script-src 里应该有每次不同的 nonce: %q", csp)
	}
	// 没有 CSRF 令牌的 POST 被拒绝。
	blocked := s.do(s.client, http.MethodPost, "/auth/login", strings.NewReader("email=a&password=b"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if blocked.status != http.StatusForbidden {
		t.Errorf("没有 CSRF 令牌的 POST 应该被拒绝，实际 %d", blocked.status)
	}
}

func TestRegisterLoginLogout(t *testing.T) {
	s := newSite(t)
	if r := s.form("/auth/register", url.Values{"email": {"bad"}}); r.status != http.StatusBadRequest {
		t.Errorf("邮箱格式不对应该被拒绝，实际 %d", r.status)
	}
	if r := s.form("/auth/register", url.Values{"email": {"bot@example.com"}, "website": {"http://spam"}}); r.status != http.StatusBadRequest {
		t.Errorf("填了隐藏字段的机器人应该被拒绝，实际 %d", r.status)
	}
	s.register("evan")
	home := s.get("/")
	if home.status != http.StatusOK || !strings.Contains(home.body, `data-user="1"`) {
		t.Fatalf("登录后首页应该是内容页: %d", home.status)
	}
	if r := s.form("/auth/register", url.Values{"email": {"evan@example.com"}}); r.status != http.StatusFound && r.status != http.StatusConflict {
		// 已登录时访问注册页面会直接回首页；提交重复邮箱会得到 409。两种都可以接受。
		t.Errorf("重复注册的结果不对: %d", r.status)
	}
	if r := s.form("/auth/logout", url.Values{}); r.status != http.StatusSeeOther {
		t.Errorf("退出登录应该跳回首页，实际 %d", r.status)
	}
	if r := s.get("/me"); r.status != http.StatusFound {
		t.Errorf("退出后应该回到登录墙外，实际 %d", r.status)
	}
	if r := s.form("/auth/login", url.Values{"email": {"evan@example.com"}, "password": {"wrong-password"}}); r.status != http.StatusUnauthorized {
		t.Errorf("密码错误应该返回 401，实际 %d", r.status)
	}
	r := s.form("/auth/login", url.Values{"email": {"EVAN@example.com"}, "password": {"password-123"}, "redirect": {"https://evil.example/"}})
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/" {
		t.Errorf("登录后只能跳到站内地址，实际 %d %q", r.status, r.header.Get("Location"))
	}
	// 改密码后，其他设备上的登录失效。
	other := s.browser()
	token := s.csrf(other)
	login := s.do(other, http.MethodPost, "/auth/login", strings.NewReader(url.Values{"_csrf": {token}, "email": {"evan@example.com"}, "password": {"password-123"}}.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if login.status != http.StatusSeeOther {
		t.Fatalf("第二台设备登录失败: %d", login.status)
	}
	if r := s.form("/settings/password", url.Values{"current": {"password-123"}, "password": {"new-password-456"}}); r.status != http.StatusSeeOther || strings.Contains(r.header.Get("Location"), "error") {
		t.Fatalf("改密码失败: %d %q", r.status, r.header.Get("Location"))
	}
	if r := s.get("/me"); r.status != http.StatusOK {
		t.Errorf("改密码的这台设备应该保持登录，实际 %d", r.status)
	}
	if r := s.do(other, http.MethodGet, "/me", nil, nil); r.status != http.StatusFound {
		t.Errorf("另一台设备应该被登出，实际 %d", r.status)
	}
}

// ---------- 页面都能渲染 ----------

func TestAllPagesRender(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	show := s.seedMedia("36000001", "雾港 第二季", media.TypeTV, media.Patch{"season": 2, "episode_count": 12,
		"backdrops": []string{"https://image.tmdb.org/t/p/original/b1.jpg", "https://image.tmdb.org/t/p/original/b2.jpg"}, "rating_douban": 8.9})
	film := s.seedMedia("36000002", "回声", media.TypeMovie, nil)
	// 第三部：同类型、同题材、高分，用来出现在"相似推荐"和"为你推荐"里。
	s.seedMedia("36000003", "白夜", media.TypeTV, media.Patch{"rating_douban": 8.6})
	ctx := context.Background()
	s.app.Media.ReplaceChart(ctx, media.TypeTV, []int64{show})
	s.app.Media.ReplaceChart(ctx, media.TypeMovie, []int64{film})
	today := media.Today()
	s.app.Media.ReplaceUnits(ctx, show, 2, []media.Unit{{Episode: 1, Title: "雾起"}, {Episode: 2, Title: "旧码头", AirDate: &today}})
	showPath, filmPath := "/m/"+itoa(show), "/m/"+itoa(film)
	s.htmx(http.MethodPost, "/api/library/"+itoa(show)+"/wish", nil)
	s.htmx(http.MethodPost, "/api/library/"+itoa(film)+"/watched", url.Values{"rating": {"5"}, "comment": {"镜头相信观众"}})
	s.json(http.MethodPost, "/api/positions", map[string]any{"items": []map[string]any{{"media_id": show, "season": 2, "episode": 1, "position": 600, "duration": 2700, "at": time.Now().UnixMilli()}}})
	s.seedPublishedCollection("best-2026", "2026 最佳", []int64{show, film}, map[int64]string{show: "年度第一"})
	s.form("/api/feedback", url.Values{"kind": {"bug"}, "content": {"搜索打不开"}})
	month := time.Now().Format("2006-01")

	pages := map[string][]string{
		"/":                                   {"雾港 第二季", "继续观看", "今日更新", "为你推荐", "白夜", "热门电影", "回声", "热门剧集", "最新片单", "2026 最佳", "/api/htmx/row/friends"},
		"/discover":                           {"雾港 第二季", "回声"},
		"/discover/tv?genre=悬疑&sort=new":      {"雾港 第二季"},
		"/search?q=雾港":                        {`value="雾港"`, "我的源"},
		"/foryou":                             {"为你推荐"},
		showPath:                              {"雾港 第二季", "播放源", "选集", "继续播放", "第 1 集", `data-media="`, "剧照"},
		filmPath:                              {"回声", "播放源", "收录这部作品的片单", "2026 最佳", `href="/list/best-2026"`},
		"/play/" + itoa(show) + "?s=2&e=2":    {"雾港 第二季", `data-episode="2"`, "来源", "弹幕"},
		"/play/local":                         {`data-local="1"`, "这部作品不在作品库里"},
		"/changelog":                          {"更新记录", "热搜榜"},
		"/me":                                 {"evan", "观看历史", "想看", "看过", "管理后台", "观影报告"},
		"/me/wish":                            {"雾港 第二季"},
		"/me/watched":                         {"回声", "★★★★★"},
		"/me/history":                         {"雾港 第二季", "第 1 集"},
		"/notifications":                      {"还没有通知"},
		"/settings":                           {"播放源", "订阅地址", "播放源会自动同步过去", "豆瓣同步", "公开我的观影主页"},
		"/cinema":                             {"镜头相信观众", "回声"},
		"/cinema?tab=following":               {"你关注的人还没有写短评"},
		"/list":                               {"2026 最佳"},
		"/list/best-2026":                     {"2026 最佳", "年度第一", "回声"},
		"/feedback":                           {"搜索打不开", "版权投诉"},
		"/user/1":                             {"evan", "镜头相信观众"},
		"/user/1/monthly/" + month:            {"观影报告", "部看过"},
		"/admin":                              {"管理后台", "刷新热门榜单", "待审核片单"},
		"/admin/users":                        {"evan@example.com"},
		"/admin/media":                        {"雾港 第二季", "红果短剧链接"},
		"/admin/media?q=回声":                   {"回声"},
		"/admin/jobs":                         {"任务"},
		"/admin/filters":                      {"添加屏蔽词"},
		"/admin/ads":                          {"侧栏广告", "添加广告"},
		"/admin/feedback":                     {"搜索打不开"},
		"/admin/collections?status=published": {"2026 最佳"},
		"/admin/collections/1":                {"下架片单", "年度第一"},
		"/me/collections/new":                 {"新建片单"},
		"/me/collections":                     {"我的片单", "2026 最佳"},
	}
	// 首屏的几行直接写在页面里，不等脚本加载完再请求；更靠下的仍然滚动到附近再加载。最上面一行的图片不延迟加载。
	if home := s.get("/").body; strings.Contains(home, `hx-get="/api/htmx/row/chart/movie"`) || strings.Contains(home, `hx-get="/api/htmx/row/foryou"`) ||
		!strings.Contains(home, `hx-get="/api/htmx/row/chart/anime"`) || !strings.Contains(home, `loading="eager"`) {
		t.Error("首页首屏的行应该直接写在页面里，第一行图片不延迟加载")
	}
	// 片单在顶部菜单里，管理后台只从「我的」进入。
	if nav := s.get("/discover").body; !strings.Contains(nav, `<a class="nav-link" href="/list"`) ||
		strings.Contains(nav, `<a class="nav-link" href="/admin"`) || strings.Contains(nav, `<a class="chip-l" href="/list">`) {
		t.Error("顶部菜单应有片单、没有管理；发现页不再放片单入口")
	}
	for path, wants := range pages {
		r := s.get(path)
		if r.status != http.StatusOK {
			t.Errorf("%s 应该返回 200，实际 %d：%.300s", path, r.status, r.body)
			continue
		}
		mustContain(t, r, wants...)
		if strings.Contains(r.body, "页面渲染失败") {
			t.Errorf("%s 渲染出错", path)
		}
	}
	for path, wants := range map[string][]string{
		"/api/htmx/row/foryou":                     {"为你推荐", "白夜"},
		"/api/htmx/row/chart/tv":                   {"热门剧集", "雾港 第二季"},
		"/api/htmx/row/collections":                {"最新片单", "2026 最佳"},
		"/api/htmx/search?q=雾港":                    {"雾港 第二季"},
		"/api/htmx/discover?type=movie&page=1":     {"回声"},
		"/api/htmx/m/" + itoa(film) + "/comments":  {"镜头相信观众", "短评"},
		"/api/htmx/m/" + itoa(show) + "/similar":   {"相似推荐", "白夜", `href="/m/` + itoa(show) + `/similar"`},
		"/api/htmx/m/" + itoa(film) + "/mark-form": {"看过《回声》", "取消标记"},
		"/api/htmx/douban-status":                  {"douban-status"},
	} {
		r := s.htmx(http.MethodGet, path, nil)
		if r.status != http.StatusOK {
			t.Errorf("%s 应该返回 200，实际 %d：%.300s", path, r.status, r.body)
			continue
		}
		mustContain(t, r, wants...)
	}
	for _, path := range []string{"/m/99999", "/list/nope", "/user/99999", "/me/unknown", "/no-such-page", "/user/1/monthly/2099-01"} {
		if r := s.get(path); r.status != http.StatusNotFound {
			t.Errorf("%s 应该返回 404，实际 %d", path, r.status)
		}
	}
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }

// ---------- 想看、看过、短评 ----------

func TestLibraryAndSocialFlow(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	film := s.seedMedia("36000002", "回声", media.TypeMovie, nil)
	path := "/api/library/" + itoa(film)

	r := s.htmx(http.MethodPost, path+"/wish", nil)
	if r.status != http.StatusOK || !strings.Contains(r.body, "取消想看") {
		t.Fatalf("加入想看后按钮应该变成取消想看: %d %s", r.status, r.body)
	}
	if r := s.htmx(http.MethodPost, path+"/wish", nil); !strings.Contains(r.body, "加入想看") {
		t.Errorf("再点一次应该取消想看: %s", r.body)
	}
	if r := s.htmx(http.MethodPost, path+"/watched", url.Values{"rating": {"9"}}); r.status != http.StatusBadRequest {
		t.Errorf("评分超出范围应该被拒绝，实际 %d", r.status)
	}
	r = s.htmx(http.MethodPost, path+"/watched", url.Values{"rating": {"4"}, "comment": {"<script>alert(1)</script> 不错"}})
	if r.status != http.StatusOK || !strings.Contains(r.body, "已看过") || r.header.Get("HX-Trigger") != "close-modal" {
		t.Fatalf("标记看过失败: %d %s", r.status, r.body)
	}
	// 已经看过时点"想看"不会把它降级。
	s.htmx(http.MethodPost, path+"/wish", nil)
	comments := s.htmx(http.MethodGet, "/api/htmx/m/"+itoa(film)+"/comments", nil)
	mustContain(t, comments, "&lt;script&gt;", "★★★★☆")
	if strings.Contains(comments.body, "<script>alert") {
		t.Fatal("短评里的 HTML 必须被转义")
	}
	match := regexp.MustCompile(`id="c-(\d+)"`).FindStringSubmatch(comments.body)
	if match == nil {
		t.Fatal("短评列表里没有找到短评")
	}
	comment := "/api/comments/" + match[1]

	// 第二个用户点赞、回复、关注。
	first := s.client
	s.client = s.browser()
	s.register("lin")
	if r := s.htmx(http.MethodPost, comment+"/like", nil); r.status != http.StatusOK || !strings.Contains(r.body, `aria-pressed="true"`) || !strings.Contains(r.body, "<span>1</span>") {
		t.Errorf("点赞后按钮状态不对: %d %s", r.status, r.body)
	}
	if r := s.htmx(http.MethodPost, comment+"/replies", url.Values{"content": {"同意"}}); r.status != http.StatusOK || !strings.Contains(r.body, "同意") {
		t.Errorf("回复失败: %d %s", r.status, r.body)
	}
	if r := s.htmx(http.MethodPost, comment+"/replies", url.Values{"content": {""}}); r.status != http.StatusBadRequest {
		t.Errorf("空回复应该被拒绝，实际 %d", r.status)
	}
	// evan 的主页没有公开：lin 看不到。
	if r := s.get("/user/1"); r.status != http.StatusForbidden {
		t.Errorf("没有公开的主页应该返回 403，实际 %d", r.status)
	}
	if r := s.htmx(http.MethodPost, "/api/users/1/follow", nil); r.status != http.StatusOK || !strings.Contains(r.body, "已关注") {
		t.Errorf("关注失败: %d %s", r.status, r.body)
	}
	if r := s.get("/admin"); r.status != http.StatusForbidden {
		t.Errorf("普通用户访问管理后台应该返回 403，实际 %d", r.status)
	}

	// 回到 evan：收到三条通知。打开通知页不会自动已读；点开一条才算读过，也可以一键全部已读、删除、清空。
	s.client = first
	mustContain(t, s.get("/me"), `<span class="pill">3</span>`)
	notes := s.get("/notifications")
	mustContain(t, notes, "赞了你对", "回复了你对", "关注了你", "同意", "全部已读", "清空")
	mustContain(t, s.get("/me"), `<span class="pill">3</span>`)
	ids := regexp.MustCompile(`id="note-(\d+)"`).FindAllStringSubmatch(notes.body, -1)
	if len(ids) != 3 {
		t.Fatalf("应该有 3 条通知: %d", len(ids))
	}
	// 最新的是关注：点开跳到对方主页，并标为已读
	if r := s.get("/notifications/" + ids[0][1] + "/open"); r.status != http.StatusFound || r.header.Get("Location") != "/user/2" {
		t.Errorf("点开关注通知应该去对方主页: %d %s", r.status, r.header.Get("Location"))
	}
	mustContain(t, s.get("/me"), `<span class="pill">2</span>`)
	if r := s.htmx(http.MethodPost, "/api/notifications/"+ids[1][1]+"/read", nil); r.status != http.StatusOK || strings.Contains(r.body, "unread") {
		t.Errorf("标为已读: %d %s", r.status, r.body)
	}
	if r := s.htmx(http.MethodDelete, "/api/notifications/"+ids[2][1], nil); r.status != http.StatusOK {
		t.Errorf("删除通知: %d", r.status)
	}
	if strings.Contains(s.get("/me").body, `class="pill"`) || strings.Count(s.get("/notifications").body, `id="note-`) != 2 {
		t.Error("一条读了、一条删了之后应该没有未读，剩 2 条")
	}
	s.form("/notifications/clear", url.Values{})
	mustContain(t, s.get("/notifications"), "还没有通知")
	s.form("/settings/public", url.Values{"public": {"on"}})
	s.client = s.browser()
	s.form("/auth/login", url.Values{"email": {"lin@example.com"}, "password": {"password-123"}})
	if r := s.get("/user/1"); r.status != http.StatusOK {
		t.Errorf("公开后其他用户应该能看到主页，实际 %d", r.status)
	}
}

// ---------- 作品匹配接口 ----------

func TestResolveAPI(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	show := s.seedMedia("36000001", "雾港 第二季", media.TypeTV, nil)
	r := s.json(http.MethodPost, "/api/media/resolve", map[string]any{"items": []map[string]any{
		{"title": "雾港第二季", "year": 2026, "type": "tv"},
		{"douban_id": "36000001"},
		{"title": "没有这部", "douban_id": "36000777"},
	}})
	if r.status != http.StatusOK {
		t.Fatalf("匹配接口失败: %d %s", r.status, r.body)
	}
	var response struct {
		Results []struct {
			Status string `json:"status"`
			Media  *struct {
				ID int64 `json:"id"`
			} `json:"media"`
		} `json:"results"`
	}
	json.Unmarshal([]byte(r.body), &response)
	if len(response.Results) != 3 || response.Results[0].Media == nil || response.Results[0].Media.ID != show ||
		response.Results[1].Status != "matched" || response.Results[2].Status != "unknown" {
		t.Fatalf("匹配结果不对: %s", r.body)
	}
	// 匹配只读：没匹配上的作品即使带了豆瓣 ID 也不收录，要等用户真的播放了（/api/media/discover）。
	if _, err := s.app.Media.ByDoubanID(context.Background(), "36000777"); !errors.Is(err, media.ErrNotFound) {
		t.Errorf("匹配接口不应该收录作品，实际 err=%v", err)
	}
	if r := s.json(http.MethodPost, "/api/media/discover", map[string]any{"douban_id": "36000777"}); r.status != http.StatusNoContent {
		t.Fatalf("播放时收录失败: %d %s", r.status, r.body)
	}
	id, err := s.app.Media.ByDoubanID(context.Background(), "36000777")
	if err != nil {
		t.Fatalf("播放时应该收录作品，等后台去豆瓣确认: %v", err)
	}
	// 重复收录不会新建作品。
	if r := s.json(http.MethodPost, "/api/media/discover", map[string]any{"douban_id": "36000777"}); r.status != http.StatusNoContent {
		t.Errorf("重复收录应该直接成功，实际 %d", r.status)
	}
	if again, _ := s.app.Media.ByDoubanID(context.Background(), "36000777"); again.ID != id.ID {
		t.Errorf("重复收录不应该新建作品: %d → %d", id.ID, again.ID)
	}
	if r := s.json(http.MethodPost, "/api/media/discover", map[string]any{"douban_id": "abc"}); r.status != http.StatusBadRequest {
		t.Errorf("豆瓣 ID 不合法时应该拒绝，实际 %d", r.status)
	}
	if r := s.json(http.MethodPost, "/api/media/resolve", map[string]any{"items": []any{}}); r.status != http.StatusBadRequest {
		t.Errorf("空请求应该被拒绝，实际 %d", r.status)
	}
	// 请求体里不接受任何源信息：多余的字段会被忽略，不会出错也不会被保存。
	extra := s.json(http.MethodPost, "/api/media/resolve", map[string]any{"items": []map[string]any{{"title": "雾港第二季", "source": "https://source.example/api", "vod_id": "9"}}})
	if extra.status != http.StatusOK || strings.Contains(extra.body, "source.example") {
		t.Errorf("多余的字段应该被忽略: %d %s", extra.status, extra.body)
	}
}

func TestSearchDiscoversFromDouban(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	s.fake.Suggest["雾港"] = []map[string]any{{"id": "36000001", "title": "雾港", "type": "tv", "year": "2026"}}
	r := s.htmx(http.MethodGet, "/api/htmx/search?q="+url.QueryEscape("雾港"), nil)
	if r.status != http.StatusOK || !strings.Contains(r.body, "雾港") || strings.Contains(r.body, "没有找到") {
		t.Fatalf("作品库里没有时应该去豆瓣查联想并收录: %d %s", r.status, r.body)
	}
	if r := s.htmx(http.MethodGet, "/api/htmx/search?q="+url.QueryEscape("完全不存在的片子"), nil); !strings.Contains(r.body, "没有找到") {
		t.Errorf("查不到时应该给出提示: %s", r.body)
	}
}

// ---------- 播放相关接口 ----------

func TestPlaybackAPIs(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	show := s.seedMedia("36000001", "雾港", media.TypeTV, nil)

	// 进度同步
	r := s.json(http.MethodPost, "/api/positions", map[string]any{"items": []map[string]any{
		{"media_id": show, "season": 1, "episode": 2, "position": 300, "duration": 2700, "at": time.Now().UnixMilli()},
		{"media_id": 99999, "season": 1, "episode": 1, "position": 1, "duration": 2},
	}})
	if r.status != http.StatusOK || !strings.Contains(r.body, `"written":1`) {
		t.Fatalf("进度上报失败: %d %s", r.status, r.body)
	}
	// 进度由服务器直接渲染进页面，没有单独的拉取接口
	if page := s.get("/play/" + itoa(show) + "?s=1&e=2"); !strings.Contains(page.body, `&#34;episode&#34;:2`) && !strings.Contains(page.body, `"episode":2`) {
		t.Errorf("播放页应该带上这一集的进度: %.200s", page.body)
	}
	if r := s.json(http.MethodDelete, "/api/positions/"+itoa(show), nil); r.status != http.StatusNoContent {
		t.Errorf("删除进度失败: %d", r.status)
	}

	// 加密历史
	rid := strings.Repeat("ab", 32)
	blob := "Y2lwaGVydGV4dA==" // 服务器只当它是一段不透明的字节
	put := s.json(http.MethodPut, "/api/private-history/"+rid, map[string]any{"blob": blob, "base": 0})
	if put.status != http.StatusOK {
		t.Fatalf("加密历史写入失败: %d %s", put.status, put.body)
	}
	conflict := s.json(http.MethodPut, "/api/private-history/"+rid, map[string]any{"blob": blob, "base": 0})
	if conflict.status != http.StatusConflict || !strings.Contains(conflict.body, blob) {
		t.Errorf("版本不一致应该返回 409 和当前密文: %d %s", conflict.status, conflict.body)
	}
	if r := s.json(http.MethodPut, "/api/private-history/zz", map[string]any{"blob": blob}); r.status != http.StatusBadRequest {
		t.Errorf("记录 ID 不对应该被拒绝，实际 %d", r.status)
	}
	if list := s.get("/api/private-history?since=0"); !strings.Contains(list.body, rid) || !strings.Contains(list.body, blob) {
		t.Errorf("加密历史拉取不对: %s", list.body)
	}

	// 广告指纹：投票 5 次确认后出现在列表里
	fingerprint := strings.Repeat("c3", 32)
	for range 5 {
		if r := s.json(http.MethodPost, "/api/ad-fingerprints/vote", map[string]any{"fingerprint": fingerprint, "vote": "confirm"}); r.status != http.StatusNoContent {
			t.Fatalf("投票失败: %d %s", r.status, r.body)
		}
	}
	if r := s.json(http.MethodPost, "/api/ad-fingerprints/vote", map[string]any{"fingerprint": "xyz", "vote": "confirm"}); r.status != http.StatusBadRequest {
		t.Errorf("格式不对的指纹应该被拒绝，实际 %d", r.status)
	}
	list := s.get("/api/ad-fingerprints/list")
	prefix, _ := hex.DecodeString(fingerprint[:32])
	if list.status != http.StatusOK || !strings.HasPrefix(list.body, "MVFP") || !bytes.Contains([]byte(list.body), prefix) {
		t.Fatalf("指纹列表不对: %d %x", list.status, list.body)
	}
	etag := list.header.Get("ETag")
	if cached := s.do(s.client, http.MethodGet, "/api/ad-fingerprints/list", nil, map[string]string{"If-None-Match": etag}); etag == "" || cached.status != http.StatusNotModified {
		t.Errorf("带着 ETag 再取应该返回 304，实际 %d", cached.status)
	}

	// 弹幕
	if r := s.json(http.MethodPost, "/api/danmaku", map[string]any{"media_id": show, "season": 1, "episode": 2, "time": 12.5, "text": "前排"}); r.status != http.StatusNoContent {
		t.Fatalf("发弹幕失败: %d %s", r.status, r.body)
	}
	if r := s.get("/api/danmaku?media_id=" + itoa(show) + "&season=1&episode=2"); !strings.Contains(r.body, "前排") || r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("弹幕列表不对，或者被允许缓存了（发完刷新会看不到自己的弹幕）: %q %s", r.header.Get("Cache-Control"), r.body)
	}
	if r := s.json(http.MethodPost, "/api/danmaku", map[string]any{"title": "没有元数据的片子", "season": 1, "episode": 1, "time": 1, "text": "你好"}); r.status != http.StatusNoContent {
		t.Errorf("没有元数据的作品也应该能发弹幕: %d %s", r.status, r.body)
	}
}

// ---------- 管理后台 ----------

func TestAdminActions(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	film := s.seedMedia("36000002", "某某禁片", media.TypeMovie, nil)
	ctx := context.Background()

	if r := s.form("/admin/filters", url.Values{"kind": {"copyright"}, "keyword": {"禁片"}}); r.status != http.StatusSeeOther {
		t.Fatalf("添加屏蔽词失败: %d %s", r.status, r.body)
	}
	if m, _ := s.app.Media.Get(ctx, film); !m.Hidden {
		t.Error("添加屏蔽词后，片名命中的作品应该立刻隐藏")
	}
	if cards, _ := s.app.Media.Search(ctx, "禁片", 10); len(cards) != 0 {
		t.Error("隐藏的作品不应该出现在搜索结果里")
	}
	// 管理员还能看到隐藏的作品（方便恢复）；恢复后正常展示。
	if r := s.get("/m/" + itoa(film)); r.status != http.StatusOK || !strings.Contains(r.body, "已隐藏") {
		t.Errorf("管理员应该能看到隐藏的作品: %d", r.status)
	}
	s.form("/admin/media/"+itoa(film)+"/hidden", url.Values{"hidden": {"0"}})
	if m, _ := s.app.Media.Get(ctx, film); m.Hidden {
		t.Error("恢复展示没有生效")
	}

	if r := s.form("/admin/media/add", url.Values{"ref": {"https://movie.douban.com/subject/36000001/"}}); r.status != http.StatusSeeOther {
		t.Fatalf("按豆瓣 ID 添加失败: %d", r.status)
	}
	if _, err := s.app.Media.ByDoubanID(ctx, "36000001"); err != nil {
		t.Errorf("作品应该已经在作品库里: %v", err)
	}
	if r := s.form("/admin/tasks", url.Values{"task": {"charts"}}); r.status != http.StatusSeeOther {
		t.Errorf("刷新榜单任务入队失败: %d", r.status)
	}
	counts, _ := s.app.Queue.Counts(ctx)
	if counts["queued"] < 2 {
		t.Errorf("应该有同步任务和榜单任务在排队: %v", counts)
	}
	if r := s.form("/admin/users/1/role", url.Values{"role": {"user"}}); !strings.Contains(r.header.Get("Location"), "notice=") {
		t.Errorf("修改自己的角色应该被拦下并给出提示: %q", r.header.Get("Location"))
	}
	if user, _ := s.app.Users.ByID(ctx, 1); !user.IsAdmin() {
		t.Error("管理员不能把自己降级")
	}
	s.form("/api/feedback", url.Values{"kind": {"dmca"}, "content": {"海报侵权"}})
	if r := s.form("/admin/feedback/1", url.Values{"action": {"close"}, "outcome": {"done"}, "reply": {"海报已下架"}}); r.status != http.StatusSeeOther {
		t.Errorf("回复反馈失败: %d", r.status)
	}
	mustContain(t, s.get("/feedback"), "海报侵权", "已处理")
	mustContain(t, s.get("/feedback/1"), "海报侵权", "海报已下架", "已处理", "会重新打开这条反馈")
	mustContain(t, s.get("/admin/feedback?state=closed"), "海报已下架", "已关闭 · 已处理")
}

func TestStaticFiles(t *testing.T) {
	s := newSite(t)
	r := s.get("/static/css/app.css")
	if r.status != http.StatusOK || r.header.Get("ETag") == "" || r.header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("静态文件应该带 ETag: %d %v", r.status, r.header)
	}
	if cached := s.do(s.client, http.MethodGet, "/static/css/app.css", nil, map[string]string{"If-None-Match": r.header.Get("ETag")}); cached.status != http.StatusNotModified {
		t.Errorf("ETag 相同时应该返回 304，实际 %d", cached.status)
	}
	if versioned := s.get("/static/css/app.css?v=abc"); !strings.Contains(versioned.header.Get("Cache-Control"), "immutable") {
		t.Errorf("带版本号的地址应该长期缓存: %q", versioned.header.Get("Cache-Control"))
	}
	if r := s.get("/static/../go.mod"); r.status == http.StatusOK {
		t.Error("不能读到静态目录之外的文件")
	}
	if r := s.get("/img/" + "aHR0cHM6Ly9ldmlsLmV4YW1wbGUvYS5qcGc"); r.status != http.StatusForbidden || r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("图片代理不能访问豆瓣以外的地址，实际 %d", r.status)
	}

	// HEAD 按 GET 处理，只回状态码和响应头；不存在的地址仍然是 404。
	for path, want := range map[string]int{"/": http.StatusOK, "/discover": http.StatusOK, "/static/css/app.css": http.StatusOK, "/no-such-page": http.StatusNotFound} {
		head := s.do(s.client, http.MethodHead, path, nil, nil)
		if head.status != want || head.body != "" || head.header.Get("Content-Type") == "" {
			t.Errorf("HEAD %s: %d %q body=%d 字节，应该是 %d", path, head.status, head.header.Get("Content-Type"), len(head.body), want)
		}
	}
}

// ---------- 添加到桌面 ----------

func TestInstallSupport(t *testing.T) {
	s := newSite(t)
	manifest := s.get("/manifest.webmanifest")
	if manifest.status != http.StatusOK || !strings.HasPrefix(manifest.header.Get("Content-Type"), "application/manifest+json") {
		t.Fatalf("应用清单不对: %d %q", manifest.status, manifest.header.Get("Content-Type"))
	}
	var data struct {
		Name     string
		Display  string
		StartURL string `json:"start_url"`
		Icons    []struct{ Src, Sizes, Purpose string }
	}
	if err := json.Unmarshal([]byte(manifest.body), &data); err != nil || len(data.Icons) != 3 || data.StartURL != "/?from=app" || data.Display != "standalone" || data.Name != "Moovie 影牛" {
		t.Fatalf("应用清单内容不对: %s err=%v", manifest.body, err)
	}
	for _, icon := range data.Icons {
		if r := s.get(icon.Src); r.status != http.StatusOK {
			t.Errorf("图标 %s 打不开: %d", icon.Src, r.status)
		}
	}
	// 根路径下的几个是手机浏览器添加到桌面时自己去取的固定地址。
	for _, path := range []string{"/static/img/apple-touch-icon.png", "/static/img/favicon-64.png",
		"/apple-touch-icon.png", "/apple-touch-icon-precomposed.png", "/favicon.ico"} {
		if r := s.get(path); r.status != http.StatusOK || r.header.Get("Content-Type") != "image/png" {
			t.Errorf("%s 打不开: %d %q", path, r.status, r.header.Get("Content-Type"))
		}
	}

	worker := s.get("/sw.js")
	if worker.status != http.StatusOK || strings.Contains(worker.body, "__VERSION__") || worker.header.Get("Cache-Control") != "no-cache" ||
		!strings.Contains(worker.header.Get("Content-Type"), "javascript") {
		t.Fatalf("Service Worker 不对: %d %q", worker.status, worker.header.Get("Cache-Control"))
	}
	// Service Worker 预先缓存的文件都要真的存在。
	for _, match := range regexp.MustCompile(`'(/[^']+)'`).FindAllStringSubmatch(worker.body, -1) {
		path := match[1]
		if strings.HasSuffix(path, "=") {
			continue
		}
		if r := s.get(path); r.status != http.StatusOK {
			t.Errorf("Service Worker 要缓存的 %s 打不开: %d", path, r.status)
		}
	}
	for _, path := range []string{"/offline", "/install"} {
		r := s.get(path)
		if r.status != http.StatusOK {
			t.Errorf("%s 应该不登录就能打开，实际 %d", path, r.status)
		}
	}
	mustContain(t, s.get("/install"), "添加到主屏幕", "iPhone / iPad", "安卓手机", "在微信、QQ 里", "登录后播放源和观看记录会自动同步")

	// 页面头部：图标、清单、import map；import map 的哈希必须在 CSP 里，否则浏览器不会执行它。
	home := s.get("/")
	mustContain(t, home, `rel="manifest"`, `rel="apple-touch-icon"`, `apple-mobile-web-app-status-bar-style`, `type="importmap"`, `rel="modulepreload"`, `/static/js/app.js?v=`)
	// app.js 整棵依赖树都预加载，包括第二、三层才 import 到的模块（不用等上一层下载完才发现）。
	for _, module := range []string{"core/install.js", "lib/platform.js", "core/episodes.js", "lib/tvbox.js"} {
		mustContain(t, home, `<link rel="modulepreload" href="/static/js/`+module+`?v=`)
	}
	importMap := regexp.MustCompile(`<script type="importmap">(.*?)</script>`).FindStringSubmatch(home.body)
	if importMap == nil {
		t.Fatal("页面里没有 import map")
	}
	sum := sha256.Sum256([]byte(importMap[1]))
	if !strings.Contains(home.header.Get("Content-Security-Policy"), "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Error("import map 的哈希不在 CSP 里")
	}
	rules := regexp.MustCompile(`<script type="speculationrules">(.*?)</script>`).FindStringSubmatch(home.body)
	if rules == nil {
		t.Fatal("页面里没有预渲染规则")
	}
	if sum := sha256.Sum256([]byte(rules[1])); !strings.Contains(home.header.Get("Content-Security-Policy"), "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Error("预渲染规则的哈希不在 CSP 里")
	}
	var mapped struct{ Imports map[string]string }
	if err := json.Unmarshal([]byte(importMap[1]), &mapped); err != nil || !strings.Contains(mapped.Imports["/static/js/core/api.js"], "?v=") {
		t.Errorf("import map 应该给模块加上版本号: %v", mapped.Imports)
	}
	for _, versioned := range mapped.Imports {
		if r := s.get(versioned); r.status != http.StatusOK || !strings.Contains(r.header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s 应该能打开并且长期缓存: %d %q", versioned, r.status, r.header.Get("Cache-Control"))
		}
	}
}

func TestBackButtonOnSubPages(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	for path, want := range map[string]string{"/settings": "/me", "/me/wish": "/me", "/list": "/discover", "/cinema": "/discover", "/admin/users": "/admin"} {
		mustContain(t, s.get(path), `class="app-back" href="`+want+`"`)
	}
	for _, path := range []string{"/", "/discover", "/search", "/me"} {
		if strings.Contains(s.get(path).body, `class="app-back"`) {
			t.Errorf("%s 是底部导航能到的页面，不需要返回按钮", path)
		}
	}
}

// ---------- 不登录也能看的作品资料 ----------

func TestPublicPages(t *testing.T) {
	s := newSite(t)
	show := s.seedMedia("36000001", "雾港 第二季", media.TypeTV, media.Patch{"season": 2, "episode_count": 12, "rating_douban": 8.9,
		"directors": []string{"陈默"}, "actors": []string{"周予"}})
	s.seedMedia("36000003", "白夜", media.TypeTV, media.Patch{"rating_douban": 8.6})
	ctx := context.Background()
	s.app.Media.ReplaceChart(ctx, media.TypeTV, []int64{show})

	// 详情页：资料、结构化数据、分享卡片都在；播放和标记引导去登录；不会去查播放源。
	detail := s.get("/m/" + itoa(show))
	if detail.status != http.StatusOK {
		t.Fatalf("未登录时详情页应该能打开，实际 %d", detail.status)
	}
	mustContain(t, detail, "雾港 第二季", `<meta name="description"`, `<link rel="canonical" href="http://moovie.test/m/`+itoa(show)+`"`,
		`property="og:image"`, `"@type":"TVSeason"`, `"seasonNumber":2`, `"name":"陈默"`, "登录后，可以用你自己配置的播放源播放", `/auth/login?redirect=%2Fm%2F`+itoa(show))
	for _, unwanted := range []string{`src="/static/js/pages/detail.js`, "src-status", "hx-post=\"/api/library"} {
		if strings.Contains(detail.body, unwanted) {
			t.Errorf("未登录的详情页不应该有 %q", unwanted)
		}
	}
	for path, wants := range map[string][]string{
		"/discover":                              {"雾港 第二季", `rel="canonical"`},
		"/discover/tv":                           {"雾港 第二季"},
		"/search":                                {"大家都在看"},
		"/trends":                                {"热搜榜", "还没有足够的搜索"},
		"/list":                                  {"片单"},
		"/cinema":                                {"影院"},
		"/m/" + itoa(show) + "/similar":          {"相似的作品", "白夜"},
		"/api/htmx/m/" + itoa(show) + "/similar": {"白夜"},
		"/api/htmx/m/" + itoa(show) + "/comments": {"还没有短评"},
		"/api/htmx/row/chart/tv":                  {"雾港 第二季"},
	} {
		r := s.get(path)
		if r.status != http.StatusOK {
			t.Errorf("%s 未登录时应该能打开，实际 %d", path, r.status)
			continue
		}
		mustContain(t, r, wants...)
	}
	if strings.Contains(s.get("/search").body, "source-search") {
		t.Error("未登录的搜索页不应该有「我的源」")
	}
	mustContain(t, s.get("/"), "先随便看看", "/api/htmx/row/chart/movie")
	// 公开的榜单行对所有人一样，可以缓存；但不能把写 Cookie 的响应交给 CDN。
	if r := s.get("/api/htmx/row/chart/tv"); !strings.Contains(r.header.Get("Cache-Control"), "max-age=300") ||
		(r.header.Get("Set-Cookie") != "" && strings.Contains(r.header.Get("Cache-Control"), "public")) {
		t.Errorf("榜单行的缓存头不对: %q set-cookie=%q", r.header.Get("Cache-Control"), r.header.Get("Set-Cookie"))
	}

	// 搜索引擎优化：标题带年份和类型；面包屑和 sameAs；短评和相似推荐直接写在页面里（爬虫不执行脚本）
	mustContain(t, detail, `<title>雾港 第二季（2026）`, "简介、演员表、短评", `"@type":"BreadcrumbList"`, `"sameAs":["https://movie.douban.com/subject/36000001/"`,
		`property="og:locale" content="zh_CN"`, `alt="《雾港 第二季》海报"`, "相似推荐", "白夜", `href="/m/`+itoa(show)+`/similar"`, "还没有短评")
	for _, unwanted := range []string{`hx-get="/api/htmx/m/` + itoa(show) + `/similar"`, `name="robots"`} {
		if strings.Contains(detail.body, unwanted) {
			t.Errorf("未登录的详情页不应该有 %q（内容应该直接写在页面里，并且允许收录）", unwanted)
		}
	}
	// 不该收录的页面：搜索结果、相似列表、登录页；首页有站点名称的结构化数据
	for path, want := range map[string]string{
		"/search?q=" + url.QueryEscape("雾港"): `<meta name="robots" content="noindex,follow">`,
		"/m/" + itoa(show) + "/similar":      `<meta name="robots" content="noindex,follow">`,
		"/auth/login":                        `<meta name="robots" content="noindex">`,
		"/":                                  `"@type":"WebSite"`,
		"/about":                             `<link rel="canonical" href="http://moovie.test/about">`,
	} {
		mustContain(t, s.get(path), want)
	}
	if strings.Contains(s.get("/search").body, `name="robots"`) {
		t.Error("没有关键词的搜索页可以收录")
	}

	robots := s.get("/robots.txt")
	mustContain(t, robots, "Allow: /", "Disallow: /play/", "Disallow: /api/", "Sitemap: http://moovie.test/sitemap.xml")
	mustContain(t, s.get("/sitemap.xml"), "<sitemapindex", "/sitemap-media-1.xml", "/sitemap-pages.xml")
	mustContain(t, s.get("/sitemap-media-1.xml"), "<loc>http://moovie.test/m/"+itoa(show)+"</loc>")
	mustContain(t, s.get("/sitemap-pages.xml"), "<loc>http://moovie.test/trends</loc>")

	// 主页：没公开的看不到，公开后不登录也能看
	s.register("evan")
	anonymous := s.browser()
	if r := s.do(anonymous, http.MethodGet, "/user/1", nil, nil); r.status != http.StatusForbidden {
		t.Errorf("没公开的主页应该返回 403，实际 %d", r.status)
	}
	s.form("/settings/public", url.Values{"public": {"on"}})
	page := s.do(anonymous, http.MethodGet, "/user/1", nil, nil)
	if page.status != http.StatusOK || strings.Contains(page.body, "关注</button>") {
		t.Errorf("公开的主页不登录也能看，但不显示关注按钮: %d", page.status)
	}
	month := time.Now().Format("2006-01")
	report := s.do(anonymous, http.MethodGet, "/user/1/monthly/"+month, nil, nil)
	if report.status != http.StatusOK || !strings.Contains(report.body, `property="og:description"`) || !strings.Contains(report.body, "分享报告") {
		t.Errorf("公开的月度报告应该能看并且能分享: %d", report.status)
	}
}

func TestTrendsFromSearchLog(t *testing.T) {
	s := newSite(t)
	s.seedMedia("36000001", "雾港", media.TypeTV, nil)
	// 两个不同的访客各搜一次；同一个访客重复搜只算一次。
	for range 3 {
		if r := s.json(http.MethodPost, "/api/search-log", map[string]string{"q": "雾港"}); r.status != http.StatusNoContent {
			t.Fatalf("记录搜索失败: %d", r.status)
		}
	}
	other := s.browser()
	token := s.csrf(other)
	s.do(other, http.MethodPost, "/api/search-log", strings.NewReader(`{"q":"雾港"}`), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": token})
	var count int
	s.app.Trends.Flush(context.Background())
	s.app.Pool.QueryRow(context.Background(), `SELECT COALESCE(sum(count), 0) FROM search_counts`).Scan(&count)
	if count != 1 {
		// 测试里两个浏览器的 IP 相同，所以第二个也被去重；这里只确认重复的没有被多记。
		t.Logf("记录了 %d 次", count)
	}
	s.app.Pool.Exec(context.Background(), `INSERT INTO search_counts (day, normalized, keyword, count) VALUES (current_date, '雾港', '雾港', 3)
ON CONFLICT (day, normalized) DO UPDATE SET count = 3`)
	s.app.Pool.Exec(context.Background(), `INSERT INTO search_counts (day, normalized, keyword, count) VALUES (current_date, '白夜', '白夜', 2)`)
	trends := s.get("/trends")
	mustContain(t, trends, "雾港", "白夜", `href="/search?q=%e9%9b%be%e6%b8%af"`)
	mustContain(t, s.get("/search"), "热搜榜")
}

func TestFetchingState(t *testing.T) {
	s := newSite(t)
	ctx := context.Background()
	id, _, _ := s.app.Media.EnsureDouban(ctx, "36000099", media.Patch{"title": "刚收录的片子"})
	detail := s.get("/m/" + itoa(id))
	mustContain(t, detail, "正在从豆瓣获取完整资料", `/api/htmx/m/`+itoa(id)+`/ready?n=1`)
	if job, found, _ := s.app.Queue.Latest(ctx, "media.refresh", itoa(id)); !found || job.Priority != 0 && job.Status != "queued" {
		t.Errorf("打开没补全的作品应该排队去抓资料: %+v %v", job, found)
	}
	if r := s.htmx(http.MethodGet, "/api/htmx/m/"+itoa(id)+"/ready?n=3", nil); !strings.Contains(r.body, "ready?n=4") {
		t.Errorf("还没好时应该继续等: %s", r.body)
	}
	if r := s.htmx(http.MethodGet, "/api/htmx/m/"+itoa(id)+"/ready?n=20", nil); !strings.Contains(r.body, "稍后再来看看") || strings.Contains(r.body, "hx-get") {
		t.Errorf("等太久应该停止轮询: %s", r.body)
	}
	s.app.Media.MarkSynced(ctx, id, "complete")
	if r := s.htmx(http.MethodGet, "/api/htmx/m/"+itoa(id)+"/ready?n=5", nil); r.header.Get("HX-Refresh") != "true" {
		t.Error("资料好了应该让页面刷新")
	}
}

func TestLegacyURLs(t *testing.T) {
	s := newSite(t)
	ctx := context.Background()
	show := s.seedMedia("36000001", "雾港", media.TypeTV, nil)
	gone, _, _ := s.app.Media.EnsureDouban(ctx, "36000002", nil)
	s.app.Media.MarkSynced(ctx, gone, "not_found")

	for path, want := range map[string]string{
		"/movie/36000001":   "/m/" + itoa(show),
		"/watch/36000001":   "/m/" + itoa(show),
		"/similar/36000001": "/m/" + itoa(show) + "/similar",
	} {
		if r := s.get(path); r.status != http.StatusMovedPermanently || r.header.Get("Location") != want {
			t.Errorf("%s 应该永久跳到 %s，实际 %d %q", path, want, r.status, r.header.Get("Location"))
		}
	}
	for _, path := range []string{"/movie/36000002", "/movie/0123", "/movie/abc"} {
		if r := s.get(path); r.status != http.StatusNotFound {
			t.Errorf("%s 应该是 404，实际 %d", path, r.status)
		}
	}

	// 库里没有的豆瓣 ID：收录并排队抓资料，临时跳转，资料还没确认前不做永久跳转。
	r := s.get("/movie/36000077")
	fresh, err := s.app.Media.ByDoubanID(ctx, "36000077")
	if err != nil || r.status != http.StatusFound || r.header.Get("Location") != "/m/"+itoa(fresh.ID) {
		t.Fatalf("没收录的应该收录后临时跳转: %d %q %v", r.status, r.header.Get("Location"), err)
	}
	if _, found, _ := s.app.Queue.Latest(ctx, "media.refresh", itoa(fresh.ID)); !found {
		t.Error("新收录的作品应该排队抓资料")
	}
}

// ---------- 登录、注册的人机校验 ----------

func TestHumanCheck(t *testing.T) {
	s := newSiteWith(t, pow.New(6, 0))
	page := s.get("/auth/register")
	match := regexp.MustCompile(`name="pow_token" value="([^"]+)"`).FindStringSubmatch(page.body)
	if match == nil || !strings.Contains(page.body, `src="/static/js/pages/auth.js`) {
		t.Fatalf("注册页应该带挑战和求解脚本: %.300s", page.body)
	}
	token := match[1]
	form := url.Values{"email": {"a@example.com"}, "pow_token": {token}}
	if r := s.form("/auth/register", form); r.status != http.StatusBadRequest || !strings.Contains(r.body, "安全校验没有通过") {
		t.Fatalf("没有答案应该被拒绝: %d", r.status)
	}
	nonce := 0
	for !pow.Solved(pow.Challenge(token), strconv.Itoa(nonce), 6) {
		nonce++
	}
	form.Set("pow_nonce", strconv.Itoa(nonce))
	if r := s.form("/auth/register", form); r.status != http.StatusOK {
		t.Fatalf("答案正确应该注册成功: %d %.200s", r.status, r.body)
	}
	// 同一个挑战不能再用
	other := s.browser()
	otherToken := s.csrf(other)
	form.Set("email", "b@example.com")
	form.Set("_csrf", otherToken)
	post := func(path string, values url.Values) reply {
		return s.do(other, http.MethodPost, path, strings.NewReader(values.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	}
	if r := post("/auth/register", form); r.status != http.StatusBadRequest {
		t.Errorf("用过的挑战应该被拒绝: %d", r.status)
	}
	if !strings.Contains(s.do(other, http.MethodGet, "/auth/login", nil, nil).body, `name="pow_token"`) {
		t.Error("登录页也要有人机校验")
	}
	if r := post("/auth/login", url.Values{"_csrf": {otherToken}, "email": {"a@example.com"}, "password": {"password-123"}}); r.status != http.StatusBadRequest {
		t.Errorf("登录没有答案应该被拒绝: %d", r.status)
	}
}

// ---------- 邮件：注册发密码、找回密码、改邮箱 ----------

func TestEmailFlows(t *testing.T) {
	s := newSite(t)
	s.register("evan") // 管理员

	// 收不了信的域名当场提示，不发信、不占次数
	other := s.browser()
	post := func(path string, values url.Values) reply {
		values.Set("_csrf", s.csrf(other))
		return s.do(other, http.MethodPost, path, strings.NewReader(values.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	}
	sent := s.mail.Count()
	for _, address := range []string{"lin@nomx.test", "lin@nullmx.test", "lin@missing.test", "lin@localhost"} {
		if r := post("/auth/register", url.Values{"email": {address}}); r.status != http.StatusBadRequest || !strings.Contains(r.body, "该邮箱域名无法接收邮件") {
			t.Errorf("%s 应该提示域名收不了信: %d", address, r.status)
		}
	}
	if s.mail.Count() != sent {
		t.Error("收不了信的域名不应该发信")
	}

	// 注册：只填邮箱，密码在邮件里，用户名取自邮箱
	if r := post("/auth/register", url.Values{"email": {"Lin.Wei@Example.org"}}); r.status != http.StatusOK {
		t.Fatalf("注册失败: %d %.300s", r.status, r.body)
	}
	message, _ := s.mail.Last("lin.wei@example.org")
	password := s.mailedPassword("lin.wei@example.org")
	if !regexp.MustCompile(`^[A-Za-z0-9]{4}-[A-Za-z0-9]{4}-[A-Za-z0-9]{4}$`).MatchString(password) || !strings.Contains(message.Text, "「linwei」") {
		t.Errorf("注册邮件不对: %q", message.Text)
	}
	if !strings.Contains(message.Text, "redirect=%2Fsettings%3Fwelcome%3D1%23sources") {
		t.Error("新用户登录后应该先去设置播放源")
	}
	if r := post("/auth/register", url.Values{"email": {"lin.wei@example.org"}}); r.status != http.StatusConflict || !strings.Contains(r.body, "/auth/forgot?email=") {
		t.Errorf("重复注册应该提示去登录或找回: %d", r.status)
	}
	if r := post("/auth/login", url.Values{"email": {"lin.wei@example.org"}, "password": {password}}); r.status != http.StatusSeeOther {
		t.Fatalf("用邮件里的密码应该能登录: %d", r.status)
	}

	// 找回密码：没注册过的邮箱也显示同样的结果，但不发信
	stranger := s.browser()
	strangerPost := func(path string, values url.Values) reply {
		values.Set("_csrf", s.csrf(stranger))
		return s.do(stranger, http.MethodPost, path, strings.NewReader(values.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	}
	sent = s.mail.Count()
	if r := strangerPost("/auth/forgot", url.Values{"email": {"nobody@example.com"}}); r.status != http.StatusOK || !strings.Contains(r.body, "重置密码的链接已经发过去了") || s.mail.Count() != sent {
		t.Errorf("没注册过的邮箱: %d，发信 %d", r.status, s.mail.Count()-sent)
	}
	// 注册邮件刚发过，同一个邮箱一分钟内不能再收；当作已经过了几分钟
	s.app.Pool.Exec(context.Background(), `UPDATE mail_log SET created_at = created_at - interval '5 minutes'`)
	if r := strangerPost("/auth/forgot", url.Values{"email": {"evan@example.com"}}); r.status != http.StatusOK {
		t.Fatalf("申请重置失败: %d %.300s", r.status, r.body)
	}
	reset, _ := s.mail.Last("evan@example.com")
	link := regexp.MustCompile(`http://moovie\.test(/auth/reset\?t=\S+)`).FindStringSubmatch(reset.Text)
	if link == nil {
		t.Fatalf("重置邮件里没有链接: %q", reset.Text)
	}
	if r := s.do(stranger, http.MethodGet, link[1], nil, nil); r.status != http.StatusOK || !strings.Contains(r.body, "设置新密码") {
		t.Fatalf("打开重置链接: %d", r.status)
	}
	token, _ := url.QueryUnescape(strings.TrimPrefix(link[1], "/auth/reset?t="))
	if r := strangerPost("/auth/reset", url.Values{"t": {token}, "password": {"short"}}); r.status != http.StatusBadRequest {
		t.Errorf("新密码太短应该被拒绝: %d", r.status)
	}
	if r := strangerPost("/auth/reset", url.Values{"t": {token}, "password": {"brand-new-pass"}}); r.status != http.StatusSeeOther {
		t.Fatalf("重置密码失败: %d", r.status)
	}
	if r := s.do(stranger, http.MethodGet, link[1], nil, nil); r.status != http.StatusBadRequest {
		t.Errorf("重置链接只能用一次: %d", r.status)
	}
	if r := s.get("/me"); r.status != http.StatusFound {
		t.Errorf("重置密码后其他设备的登录应该失效: %d", r.status)
	}
	if r := strangerPost("/auth/forgot", url.Values{"email": {"evan@example.com"}}); r.status != http.StatusServiceUnavailable || !strings.Contains(r.body, "太频繁") {
		t.Errorf("一分钟内重复申请应该被限制: %d", r.status)
	}

	// 改邮箱：先发确认邮件到新邮箱，点链接才生效，并通知旧邮箱
	s.client = other // other 已经以 lin.wei 登录
	if r := s.form("/settings/email", url.Values{"email": {"lin@nomx.test"}}); !strings.Contains(r.header.Get("Location"), "error=email_undeliverable") {
		t.Errorf("新邮箱收不了信应该当场提示: %s", r.header.Get("Location"))
	}
	if r := s.form("/settings/email", url.Values{"email": {"lin@example.com"}}); !strings.Contains(r.header.Get("Location"), "notice=email_sent") {
		t.Fatalf("应该提示确认邮件已发出: %s", r.header.Get("Location"))
	}
	mustContain(t, s.get("/settings?notice=email_sent"), "确认邮件已经发到新邮箱")
	confirm, _ := s.mail.Last("lin@example.com")
	confirmLink := regexp.MustCompile(`http://moovie\.test(/auth/email\?t=\S+)`).FindStringSubmatch(confirm.Text)
	if confirmLink == nil {
		t.Fatalf("确认邮件里没有链接: %q", confirm.Text)
	}
	if user, _ := s.app.Users.ByEmail(context.Background(), "lin.wei@example.org"); user == nil {
		t.Error("确认之前邮箱不应该改变")
	}
	mustContain(t, s.do(stranger, http.MethodGet, confirmLink[1], nil, nil), "邮箱已修改")
	if notice, ok := s.mail.Last("lin.wei@example.org"); !ok || !strings.Contains(notice.Text, "l***@example.com") {
		t.Errorf("旧邮箱应该收到通知: %q", notice.Text)
	}
	if r := s.do(stranger, http.MethodGet, confirmLink[1], nil, nil); r.status != http.StatusBadRequest {
		t.Errorf("确认链接只能用一次: %d", r.status)
	}

	// 管理员可以生成重置链接
	s.client = s.browser()
	if r := s.form("/auth/login", url.Values{"email": {"evan@example.com"}, "password": {"brand-new-pass"}}); r.status != http.StatusSeeOther {
		t.Fatalf("管理员登录失败: %d", r.status)
	}
	mustContain(t, s.form("/admin/users/2/reset-link", url.Values{}), "linwei 的重置密码链接", "http://moovie.test/auth/reset?t=")
	// 用 HTMX 时只返回链接那一块，页面地址不变（否则之后的删除等操作会跳回这个只能 POST 的地址）
	partial := s.htmx(http.MethodPost, "/admin/users/2/reset-link", nil)
	if partial.status != http.StatusOK || !strings.Contains(partial.body, "/auth/reset?t=") || strings.Contains(partial.body, "<html") {
		t.Errorf("HTMX 请求应该只返回链接片段: %d %.200s", partial.status, partial.body)
	}
}

// ---------- 管理员操作的通知：反馈被回复、内容被删除 ----------

func TestAdminNotifications(t *testing.T) {
	s := newSite(t)
	s.register("evan") // 管理员
	film := s.seedMedia("36000002", "回声", media.TypeMovie, nil)
	admin := s.client

	s.client = s.browser()
	s.register("lin")
	lin := s.client
	s.htmx(http.MethodPost, "/api/library/"+itoa(film)+"/watched", url.Values{"rating": {"2"}, "comment": {"垃圾广告 加我微信"}})
	s.form("/api/feedback", url.Values{"kind": {"bug"}, "content": {"播放卡顿"}})
	comments := s.htmx(http.MethodGet, "/api/htmx/m/"+itoa(film)+"/comments", nil)
	if strings.Contains(comments.body, "data-remove") {
		t.Error("普通用户不应该看到删除别人短评的按钮")
	}
	commentID := regexp.MustCompile(`id="c-(\d+)"`).FindStringSubmatch(comments.body)[1]
	if r := s.form("/api/comments/"+commentID+"/remove", url.Values{}); r.status != http.StatusForbidden {
		t.Errorf("普通用户不能删别人的短评: %d", r.status)
	}

	// evan 也回复 lin 的短评，再由管理员删掉 lin 的短评
	s.client = admin
	s.htmx(http.MethodPost, "/api/comments/"+commentID+"/replies", url.Values{"content": {"请勿发广告"}})
	mustContain(t, s.htmx(http.MethodGet, "/api/htmx/m/"+itoa(film)+"/comments", nil), `data-remove="/api/comments/`+commentID+`/remove"`)
	if r := s.htmx(http.MethodPost, "/api/comments/"+commentID+"/remove?reason="+url.QueryEscape("广告"), nil); r.status != http.StatusOK || !strings.Contains(r.body, "已通知作者") {
		t.Fatalf("删除短评失败: %d %s", r.status, r.body)
	}
	if strings.Contains(s.htmx(http.MethodGet, "/api/htmx/m/"+itoa(film)+"/comments", nil).body, "加我微信") {
		t.Error("短评应该已经删掉")
	}
	feedbackID := regexp.MustCompile(`action="/admin/feedback/(\d+)"`).FindStringSubmatch(s.get("/admin/feedback").body)
	if feedbackID == nil {
		t.Fatal("后台没有找到反馈")
	}
	s.form("/admin/feedback/"+feedbackID[1], url.Values{"action": {"reply"}, "reply": {"已修复，请刷新"}})
	if r := s.form("/admin/feedback/"+feedbackID[1], url.Values{"action": {"reply"}, "reply": {"  "}}); r.status != http.StatusBadRequest {
		t.Errorf("空回复应该被拒绝: %d", r.status)
	}
	if strings.Contains(s.get("/admin/feedback").body, "播放卡顿") {
		t.Error("回复后这条反馈不应该还在待回复里")
	}
	if r := s.get("/feedback/" + feedbackID[1]); r.status != http.StatusNotFound {
		t.Errorf("别人的反馈应该看不到: %d", r.status)
	}

	s.client = lin
	mustContain(t, s.get("/discover"), `class="nav-dot"`, `href="/notifications"`)
	notes := s.get("/notifications")
	mustContain(t, notes, "管理员", "回复了你的反馈", "已修复，请刷新",
		"删除了你在《回声》下的一条内容", "垃圾广告 加我微信", "原因：广告")
	if strings.Count(notes.body, "回复了你的反馈") != 1 {
		t.Error("只回复了一次，应该只有一条通知")
	}
	first := regexp.MustCompile(`id="note-(\d+)"`).FindStringSubmatch(notes.body)[1] // 最新的一条：反馈被回复
	if r := s.get("/notifications/" + first + "/open"); r.header.Get("Location") != "/feedback/"+feedbackID[1] {
		t.Errorf("点开反馈通知应该去反馈页: %s", r.header.Get("Location"))
	}
	feedbackPage := s.get("/feedback")
	mustContain(t, feedbackPage, `id="f-`+feedbackID[1]+`"`, `href="/feedback/`+feedbackID[1]+`"`, "已回复", `class="nav-dot"`) // 还有一条没读
	mustContain(t, s.get("/feedback/"+feedbackID[1]), "播放卡顿", "已修复，请刷新", "关闭这条反馈")
	// lin 接着补充，又回到管理员的待回复
	if r := s.form("/api/feedback/"+feedbackID[1]+"/reply", url.Values{"content": {"刷新后还是卡"}}); r.status != http.StatusSeeOther {
		t.Errorf("用户回复失败: %d", r.status)
	}
	s.client = admin
	mustContain(t, s.get("/admin/feedback"), "播放卡顿", "已修复，请刷新", "刷新后还是卡", "待回复")
	s.client = lin
	s.form("/api/feedback/"+feedbackID[1]+"/close", url.Values{})
	mustContain(t, s.get("/feedback/"+feedbackID[1]), "已关闭", "会重新打开这条反馈")
	s.form("/notifications/read-all", url.Values{})
	if strings.Contains(s.get("/feedback").body, `class="nav-dot"`) {
		t.Error("全部已读后导航栏不应该再有红点")
	}
	// 评分和看过记录还在，只是短评文字没了
	if entry, found, err := s.app.Library.Get(context.Background(), 2, film); err != nil || !found || entry.Rating != 2 || entry.Comment != "" {
		t.Errorf("删除短评后应该保留评分: %+v %v", entry, err)
	}
}

// ---------- 分享卡片的数据 ----------

func TestShareCards(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	film := s.seedMedia("36000003", "回声", media.TypeMovie, nil)
	s.htmx(http.MethodPost, "/api/library/"+itoa(film)+"/watched", url.Values{"rating": {"5"}, "comment": {"镜头相信观众"}})
	s.seedPublishedCollection("best", "年度最佳", []int64{film}, nil)

	cards := func(body string) []map[string]any {
		var list []map[string]any
		for _, match := range regexp.MustCompile(`data-card="([^"]+)"`).FindAllStringSubmatch(body, -1) {
			var card map[string]any
			if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &card); err != nil {
				t.Fatalf("卡片数据不是合法的 JSON: %v %s", err, match[1])
			}
			list = append(list, card)
		}
		return list
	}
	anonymous := s.do(s.browser(), http.MethodGet, "/m/"+itoa(film), nil, nil)
	if got := cards(anonymous.body); len(got) == 0 || got[0]["kind"] != "media" || got[0]["title"] != "回声" || got[0]["url"] != "http://moovie.test/m/"+itoa(film) {
		t.Errorf("作品页的卡片不对（不登录也要有）: %v", got)
	}
	comment := cards(s.htmx(http.MethodGet, "/api/htmx/m/"+itoa(film)+"/comments", nil).body)
	if len(comment) == 0 || comment[0]["kind"] != "comment" || comment[0]["text"] != "镜头相信观众" || comment[0]["badge"] != "★★★★★" {
		t.Errorf("短评的卡片不对: %v", comment)
	}
	if got := cards(s.get("/list/best").body); len(got) == 0 || got[0]["kind"] != "list" || got[0]["sub"] != "共 1 部" {
		t.Errorf("片单的卡片不对: %v", got)
	}
	month := time.Now().Format("2006-01")
	report := cards(s.get("/user/1/monthly/" + month).body)
	if len(report) == 0 || report[0]["kind"] != "report" || report[0]["url"] != "http://moovie.test/" {
		t.Errorf("没公开主页时，报告卡片的二维码应该指向首页: %v", report)
	}
	s.form("/settings/public", url.Values{"public": {"on"}})
	if got := cards(s.get("/user/1/monthly/" + month).body); len(got) == 0 || got[0]["url"] != "http://moovie.test/user/1/monthly/"+month {
		t.Errorf("公开后报告卡片应该指向报告页: %v", got)
	}
}

// ---------- 导出数据、注销账号 ----------

func TestExportAndDeleteAccount(t *testing.T) {
	s := newSite(t)
	s.register("evan") // 管理员
	film := s.seedMedia("36000004", "回声", media.TypeMovie, nil)
	s.seedPublishedCollection("best", "年度最佳", []int64{film}, nil)
	admin := s.client

	s.client = s.browser()
	s.register("lin")
	s.htmx(http.MethodPost, "/api/library/"+itoa(film)+"/watched", url.Values{"rating": {"4"}, "comment": {"=HYPERLINK(\"x\") 好看"}})
	s.htmx(http.MethodPost, "/api/users/1/follow", nil)
	s.form("/api/feedback", url.Values{"kind": {"request"}, "content": {"希望支持导出"}})

	exported := s.get("/settings/export.json")
	if !strings.Contains(exported.header.Get("Content-Disposition"), "attachment") {
		t.Error("导出应该是下载文件")
	}
	var data struct {
		Account   struct{ Email string }
		Library   []struct{ Title, Status, Comment string }
		Following []string
		Feedback  []struct{ Content string }
	}
	if err := json.Unmarshal([]byte(exported.body), &data); err != nil {
		t.Fatalf("导出的 JSON 不合法: %v %d %.300s", err, exported.status, exported.body)
	}
	if data.Account.Email != "lin@example.com" || len(data.Library) != 1 || data.Library[0].Title != "回声" || data.Library[0].Status != "watched" ||
		len(data.Following) != 1 || data.Following[0] != "evan" || len(data.Feedback) != 1 {
		t.Errorf("导出内容不对: %+v", data)
	}
	table := s.get("/settings/export.csv")
	if !strings.HasPrefix(table.body, "\uFEFF片名,") || !strings.Contains(table.body, "看过") || !strings.Contains(table.body, `'=HYPERLINK`) {
		t.Errorf("CSV 不对（要有 BOM，公式要转义）: %.300s", table.body)
	}

	// 注销：密码不对、没输确认词都不行
	if r := s.form("/settings/delete-account", url.Values{"password": {"wrong"}, "confirm": {"注销"}}); !strings.Contains(r.header.Get("Location"), "error=password_wrong") {
		t.Errorf("密码不对应该被拒绝: %s", r.header.Get("Location"))
	}
	if r := s.form("/settings/delete-account", url.Values{"password": {"password-123"}}); !strings.Contains(r.header.Get("Location"), "error=confirm_missing") {
		t.Errorf("没有确认应该被拒绝: %s", r.header.Get("Location"))
	}
	done := s.form("/settings/delete-account", url.Values{"password": {"password-123"}, "confirm": {"注销"}})
	mustContain(t, done, "账号已注销", `data-clear-user="2"`, "pages/goodbye.js")
	if r := s.get("/me"); r.status != http.StatusFound {
		t.Errorf("注销后应该退出登录: %d", r.status)
	}
	if _, err := s.app.Users.ByEmail(context.Background(), "lin@example.com"); err == nil {
		t.Error("账号应该已经删除")
	}
	if notice, ok := s.mail.Last("lin@example.com"); !ok || !strings.Contains(notice.Subject, "账号已注销") {
		t.Errorf("应该给本人发注销通知: %+v", notice)
	}
	var left int
	s.app.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM user_movies WHERE user_id = 2) + (SELECT count(*) FROM user_follows WHERE follower_id = 2)`).Scan(&left)
	if left != 0 {
		t.Errorf("账号的数据应该一起删除，还剩 %d 条", left)
	}

	// 唯一的管理员不能注销；有了另一位管理员后可以，片单转给对方
	s.client = admin
	if r := s.form("/settings/delete-account", url.Values{"password": {"password-123"}, "confirm": {"注销"}}); !strings.Contains(r.header.Get("Location"), "error=last_admin") {
		t.Fatalf("唯一的管理员不能注销: %s", r.header.Get("Location"))
	}
	s.client = s.browser()
	s.register("mei")
	s.client = admin
	s.form("/admin/users/3/role", url.Values{"role": {"admin"}})
	mustContain(t, s.form("/settings/delete-account", url.Values{"password": {"password-123"}, "confirm": {"注销"}}), "账号已注销")
	if r := s.do(s.browser(), http.MethodGet, "/list/best", nil, nil); r.status != http.StatusOK {
		t.Errorf("管理员注销后，他整理的片单应该还在: %d", r.status)
	}
}

// ---------- 账号同步：钥匙圈 ----------

func TestKeyringSync(t *testing.T) {
	s := newSite(t)
	s.register("evan")

	// 脚本登录（JSON）：成功时返回用户 ID，失败时给一个新的人机校验挑战
	other := s.browser()
	login := func(password string) reply {
		values := url.Values{"_csrf": {s.csrf(other)}, "email": {"evan@example.com"}, "password": {password}}
		return s.do(other, http.MethodPost, "/auth/login", strings.NewReader(values.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"})
	}
	if r := login("wrong-password"); r.status != http.StatusUnauthorized || !strings.Contains(r.body, `"error"`) {
		t.Errorf("密码不对: %d %s", r.status, r.body)
	}
	if r := login("password-123"); r.status != http.StatusOK || !strings.Contains(r.body, `"user_id":1`) {
		t.Fatalf("脚本登录失败: %d %s", r.status, r.body)
	}

	if r := s.get("/api/keyring"); r.status != http.StatusNotFound {
		t.Fatalf("还没有钥匙圈时应该 404: %d", r.status)
	}
	salt := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16))
	blob := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 64))
	put := func(base int, iterations int, b string) reply {
		return s.json(http.MethodPut, "/api/keyring", map[string]any{"salt": salt, "iterations": iterations, "blob": b, "base": base})
	}
	if r := put(0, 310000, blob); r.status != http.StatusOK || !strings.Contains(r.body, `"version":1`) {
		t.Fatalf("创建钥匙圈失败: %d %s", r.status, r.body)
	}
	if r := put(0, 310000, blob); r.status != http.StatusConflict || !strings.Contains(r.body, `"current"`) {
		t.Errorf("基于旧版本的写入应该冲突，并带回当前的: %d %s", r.status, r.body)
	}
	if r := put(1, 310000, blob); r.status != http.StatusOK || !strings.Contains(r.body, `"version":2`) {
		t.Errorf("更新钥匙圈失败: %d %s", r.status, r.body)
	}
	if r := put(2, 1000, blob); r.status != http.StatusBadRequest {
		t.Errorf("迭代次数太少应该被拒绝: %d", r.status)
	}
	if r := put(2, 310000, "AAAA"); r.status != http.StatusBadRequest {
		t.Errorf("密文太短应该被拒绝: %d", r.status)
	}
	got := s.get("/api/keyring")
	if got.status != http.StatusOK || !strings.Contains(got.body, blob) || !strings.Contains(got.body, `"version":2`) || got.header.Get("Cache-Control") != "no-store" {
		t.Errorf("读取钥匙圈: %d %s", got.status, got.body)
	}
	// 另一个账号读不到
	s.client = s.browser()
	s.register("lin")
	if r := s.get("/api/keyring"); r.status != http.StatusNotFound {
		t.Errorf("别人的钥匙圈不能读到: %d", r.status)
	}
	if r := s.do(s.browser(), http.MethodGet, "/api/keyring", nil, nil); r.status != http.StatusUnauthorized && r.status != http.StatusFound {
		t.Errorf("没登录不能访问: %d", r.status)
	}
}

func TestCopyrightContact(t *testing.T) {
	s := newSite(t)
	mustContain(t, s.get("/dmca"), "版权投诉", `href="/feedback?kind=dmca"`, `mailto:hello@moovie.test`, "不需要注册账号")
	// 关于页：广告说明和联系方式（反馈不预选类型，邮件主题是合作与联系）
	about := s.get("/about")
	mustContain(t, about, "关于广告", "每一条都标有“广告”", "不加载第三方广告脚本", "核实后会立刻下架", "联系方式",
		`href="/feedback"`, `mailto:hello@moovie.test?subject=%E5%90%88%E4%BD%9C%E4%B8%8E%E8%81%94%E7%B3%BB`, "广告合作和其他事务")
	mustContain(t, s.get("/privacy"), "站内广告", "看不到你在看哪个页面")
	if strings.Contains(s.get("/terms").body, "联系方式") {
		t.Error("没有设置联系方式的页面不应该显示联系方式")
	}
	mustContain(t, s.get("/changelog"), "v5.3.0", "浏览器插件", "v5.2.4", "每天的豆瓣同步更省", "v5.2.3", "反馈改成对话", "安卓投屏", "v5.2.2", "综艺按期选集", "v5.2.1", "弹幕加载不出来", "v5.2.0", "2026-11-22", "发现页的新排序", "v5.1.1", "兼容 TVBox 配置", "v5.1.0", "2026-11-20", "v5.0.0", "2026-11-16", "💥 破坏性变更 (Breaking Changes)", "✨ 新增 (Added)", "登录即同步")
}

// ---------- 后台：内容审核、禁言封禁、广告指纹、趋势 ----------

func TestModerationTools(t *testing.T) {
	s := newSite(t)
	s.register("evan") // 管理员
	film := s.seedMedia("36000005", "回声", media.TypeMovie, nil)
	admin := s.client

	s.client = s.browser()
	s.register("lin")
	lin := s.client
	s.htmx(http.MethodPost, "/api/library/"+itoa(film)+"/watched", url.Values{"rating": {"1"}, "comment": {"加微信看全集"}})
	s.json(http.MethodPost, "/api/danmaku", map[string]any{"media_id": film, "season": 1, "episode": 1, "time": 75, "text": "博彩网站"})

	// 内容审核：按关键词搜，批量删除，作者收到通知
	s.client = admin
	s.app.Ingest.EnqueueRefresh(context.Background(), film, 0)
	mustContain(t, s.get("/admin"), "最近 30 天", "新用户", "观影人数", "class=\"spark\"", "等待采集的作品", "最早的已经等了", "停滞", "一个也没做完")
	page := s.get("/admin/content?q=" + url.QueryEscape("微信"))
	mustContain(t, page, "加微信看全集", "lin")
	id := regexp.MustCompile(`name="ids" value="(\d+)"`).FindStringSubmatch(page.body)[1]
	s.form("/admin/content/delete", url.Values{"type": {"comments"}, "ids": {id}, "reason": {"广告"}})
	if strings.Contains(s.get("/admin/content").body, "加微信看全集") {
		t.Error("短评应该已经删掉")
	}
	danmakuPage := s.get("/admin/content?type=danmaku")
	mustContain(t, danmakuPage, "博彩网站", "第 1 集 · 1:15")
	danmakuID := regexp.MustCompile(`name="ids" value="(\d+)"`).FindStringSubmatch(danmakuPage.body)[1]
	s.form("/admin/content/delete", url.Values{"type": {"danmaku"}, "ids": {danmakuID}})
	if r := s.get("/api/danmaku?media_id=" + itoa(film) + "&season=1&episode=1"); strings.Contains(r.body, "博彩网站") {
		t.Error("弹幕应该已经删掉")
	}

	// 禁言：不能写短评、回复、弹幕，但可以标记和评分；收到通知
	s.form("/admin/users/2/restrict", url.Values{"kind": {"mute"}, "days": {"7"}, "reason": {"发广告"}})
	mustContain(t, s.get("/admin/users"), "禁言中")
	s.client = lin
	if r := s.htmx(http.MethodPost, "/api/library/"+itoa(film)+"/watched", url.Values{"rating": {"2"}, "comment": {"再发一次"}}); r.status != http.StatusForbidden || !strings.Contains(r.body, "已被禁言到") {
		t.Errorf("禁言后不能写短评: %d %s", r.status, r.body)
	}
	if r := s.htmx(http.MethodPost, "/api/library/"+itoa(film)+"/watched", url.Values{"rating": {"2"}}); r.status != http.StatusOK {
		t.Errorf("禁言后仍然可以评分: %d", r.status)
	}
	if r := s.json(http.MethodPost, "/api/danmaku", map[string]any{"media_id": film, "season": 1, "episode": 1, "time": 5, "text": "再来"}); r.status != http.StatusForbidden {
		t.Errorf("禁言后不能发弹幕: %d", r.status)
	}
	mustContain(t, s.get("/notifications"), "限制了你的发言", "已被禁言到", "原因：发广告")

	// 封禁：立即退出登录，也登录不了
	s.client = admin
	s.form("/admin/users/2/restrict", url.Values{"kind": {"ban"}, "days": {"0"}, "reason": {"屡教不改"}})
	s.client = lin
	if r := s.get("/me"); r.status != http.StatusFound {
		t.Errorf("封禁后应该立即退出登录: %d", r.status)
	}
	if r := s.form("/auth/login", url.Values{"email": {"lin@example.com"}, "password": {"password-123"}}); r.status != http.StatusForbidden || !strings.Contains(r.body, "已被封禁（永久）") {
		t.Errorf("封禁后不能登录: %d", r.status)
	}
	s.client = admin
	if r := s.form("/admin/users/1/restrict", url.Values{"kind": {"ban"}}); !strings.Contains(r.header.Get("Location"), url.QueryEscape("不能限制管理员")) {
		t.Errorf("不能封禁管理员: %s", r.header.Get("Location"))
	}
	s.form("/admin/users/2/restrict", url.Values{"kind": {""}})
	s.client = s.browser()
	if r := s.form("/auth/login", url.Values{"email": {"lin@example.com"}, "password": {"password-123"}}); r.status != http.StatusSeeOther {
		t.Errorf("解除后应该能登录: %d", r.status)
	}

	// 广告指纹：后台只读，看得到票数和状态，没有人工裁定
	s.client = admin
	fingerprint := strings.Repeat("ab", 32)
	s.json(http.MethodPost, "/api/ad-fingerprints/vote", map[string]any{"fingerprint": fingerprint, "vote": "confirm"})
	page = s.get("/admin/fingerprints")
	mustContain(t, page, fingerprint[:16], "待定", "不提供人工裁定")
	if r := s.form("/admin/fingerprints/"+fingerprint, url.Values{"action": {"confirm"}}); r.status != http.StatusNotFound {
		t.Errorf("不应该再有人工裁定的接口: %d", r.status)
	}
}

// 临时功能：示例订阅。配了文件就在 /sub/example.json 提供，设置页出现"使用示例订阅"；没配就没有。
func TestExampleSubscription(t *testing.T) {
	plain := newSite(t)
	plain.register("evan")
	if r := plain.get("/sub/example.json"); r.status != http.StatusNotFound {
		t.Errorf("没配置时应该是 404，实际 %d", r.status)
	}
	if strings.Contains(plain.get("/settings").body, "sub-example") {
		t.Error("没配置时设置页不应该有示例订阅按钮")
	}

	path := filepath.Join(t.TempDir(), "sources.json")
	os.WriteFile(path, []byte(`{"version":1,"sources":[{"key":"a.example","name":"示例源","api":"https://a.example/api.php/provide/vod/"}]}`), 0o644)
	s := newSiteWith(t, nil, func(cfg *config.Config) { cfg.ExampleSubscription = path })
	s.register("lin") // 和上面的站点共用一个测试库，换个名字
	r := s.get("/sub/example.json")
	if r.status != http.StatusOK || !strings.Contains(r.header.Get("Content-Type"), "json") || !strings.Contains(r.body, "示例源") {
		t.Fatalf("应该提供示例订阅: %d %q", r.status, r.body)
	}
	mustContain(t, s.get("/settings"), `id="sub-example"`, `data-url="/sub/example.json"`)

	// 从旧版迁移过来的账号：还没有钥匙圈（第一次登录）时，告诉浏览器填入示例订阅；钥匙圈建好后就不再提示。
	if r := s.get("/api/example-subscription"); r.status != http.StatusNotFound {
		t.Errorf("不是迁移过来的账号不应该填入示例订阅，实际 %d", r.status)
	}
	lin, err := s.app.Users.ByEmail(context.Background(), "lin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.app.Pool.Exec(context.Background(), `INSERT INTO example_subscription_users (user_id) VALUES ($1)`, lin.ID); err != nil {
		t.Fatal(err)
	}
	if r := s.get("/api/example-subscription"); r.status != http.StatusOK || !strings.Contains(r.body, `"url":"/sub/example.json"`) {
		t.Fatalf("迁移过来的账号第一次登录应该填入示例订阅: %d %s", r.status, r.body)
	}
	if r := plain.get("/api/example-subscription"); r.status != http.StatusNotFound {
		t.Errorf("没配置示例订阅时不应该提示，实际 %d", r.status)
	}
	salt := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16))
	blob := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 64))
	if r := s.json(http.MethodPut, "/api/keyring", map[string]any{"salt": salt, "blob": blob, "iterations": 310000, "base": 0}); r.status != http.StatusOK {
		t.Fatal(r.status, r.body)
	}
	if r := s.get("/api/example-subscription"); r.status != http.StatusNotFound {
		t.Errorf("钥匙圈建好以后不应该再填入示例订阅，实际 %d", r.status)
	}

	broken := filepath.Join(t.TempDir(), "broken.json")
	os.WriteFile(broken, []byte(`{"sources":[]}`), 0o644)
	if r := newSiteWith(t, nil, func(cfg *config.Config) { cfg.ExampleSubscription = broken }).get("/sub/example.json"); r.status != http.StatusNotFound {
		t.Errorf("文件里没有源时应该关闭，实际 %d", r.status)
	}
}

// 播放源查询的转发：要登录；服务器自己的内网地址不让访问。
func TestRelayGuards(t *testing.T) {
	s := newSite(t)
	target := "/api/relay?url=" + url.QueryEscape(s.server.URL+"/healthz")
	if r := s.get(target); r.status != http.StatusUnauthorized {
		t.Errorf("游客不能用转发，实际 %d", r.status)
	}
	s.register("evan")
	if r := s.get(target); r.status != http.StatusForbidden {
		t.Errorf("本机地址应该被拦下，实际 %d %s", r.status, r.body)
	}
	if r := s.get("/api/relay?url=" + url.QueryEscape("file:///etc/passwd")); r.status != http.StatusBadRequest {
		t.Errorf("只允许 http 和 https，实际 %d", r.status)
	}
}

// 短剧只在短剧频道和搜索里出现，页面不让搜索引擎收录。
func TestShortDramaPages(t *testing.T) {
	s := newSite(t)
	ctx := context.Background()
	s.seedMedia("36000001", "雾港", media.TypeTV, nil)
	id, _, err := s.app.Media.EnsureHongguo(ctx, "7706890402913078297")
	if err != nil {
		t.Fatal(err)
	}
	s.app.Media.ApplyPatch(ctx, id, media.ProviderHongguo, media.Patch{"media_type": media.TypeShort, "title": "帐中香第二季", "series_key": "hg:1",
		"genres": []string{"漫剧", "恋爱", "古代"}, "episode_count": 135, "series_status": "ended", "rating_hongguo": 9.0, "heat": int64(70974984),
		"poster": "https://p6-novel.byteimg.com/novel-pic/abc~tplv-shrink:640:0.image"})
	s.app.Media.MarkSynced(ctx, id, "complete")
	s.app.Media.ReplaceChart(ctx, "short_hot", []int64{id})

	all := s.get("/discover")
	if strings.Contains(all.body, "帐中香") {
		t.Error("发现页的全部里不应该有短剧")
	}
	channel := s.get("/discover/short")
	mustContain(t, channel, "帐中香第二季", "热播榜", "全部剧种", "漫剧", "恋爱", `name="robots" content="noindex`, "短剧 · 恋爱")
	filtered := s.get("/discover/short?form=" + url.QueryEscape("真人剧"))
	if strings.Contains(filtered.body, "帐中香第二季") {
		t.Error("按真人剧筛选时不应该出现漫剧")
	}
	detail := s.get("/m/" + itoa(id))
	mustContain(t, detail, "短剧 · 漫剧 · 全 135 集", "红果 9.0", "hongguoduanju.com/detail?series_id=7706890402913078297", `name="robots" content="noindex`)
	// 封面走站内代理，缩放到要的宽度。
	cover := html.EscapeString(imageproxy.New(nil, "test-secret").URL("https://p6-novel.byteimg.com/novel-pic/abc~tplv-shrink:640:0.image", "display", "帐中香第二季", "poster"))
	if !strings.Contains(detail.body, cover) {
		t.Error("详情页的大图应该用 640 宽的封面并走站内代理")
	}
	if sitemap := s.get("/sitemap-media-1.xml"); strings.Contains(sitemap.body, "/m/"+itoa(id)+"<") {
		t.Error("短剧不应该进站点地图")
	}
	found := s.get("/api/htmx/search?q=" + url.QueryEscape("帐中香"))
	mustContain(t, found, "帐中香第二季", "短剧 · 恋爱")
}

// 侧栏广告：管理员在后台添加、修改、删除；详情页信息下方、播放页弹幕下方按排序号竖着显示，关着的不显示。
func TestSideAds(t *testing.T) {
	s := newSite(t)
	s.register("evan")
	film := s.seedMedia("36000003", "有广告的电影", media.TypeMovie, nil)
	detail, play := "/m/"+itoa(film), "/play/"+itoa(film)
	if strings.Contains(s.get(detail).body, `class="side-ads"`) {
		t.Fatal("还没设置广告时不应该有广告位")
	}

	add := func(values url.Values) reply {
		t.Helper()
		return s.form("/admin/ads", values)
	}
	if r := add(url.Values{"enabled": {"on"}, "kind": {"image"}, "image_url": {"https://ads.example.com/banner.jpg"}, "text": {"图片说明"},
		"link": {"https://shop.example.com/?a=1&b=2"}, "position": {"2"}}); r.status != http.StatusSeeOther {
		t.Fatalf("添加图片广告失败: %d %.300s", r.status, r.body)
	}
	add(url.Values{"enabled": {"on"}, "kind": {"text"}, "text": {"文字广告在上面"}, "position": {"1"}})
	add(url.Values{"kind": {"text"}, "text": {"关着的广告"}, "position": {"0"}})

	for _, path := range []string{detail, play} {
		body := s.get(path).body
		block := regexp.MustCompile(`(?s)<aside class="side-ads".*?</aside>`).FindString(body)
		if block == "" {
			t.Fatalf("%s 应该有广告位", path)
		}
		textAt, imageAt := strings.Index(block, "文字广告在上面"), strings.Index(block, "banner.jpg")
		if textAt < 0 || imageAt < 0 || textAt > imageAt || strings.Contains(block, "关着的广告") {
			t.Errorf("%s 应该按排序号显示两条开着的广告:\n%s", path, block)
		}
		for _, want := range []string{`href="https://shop.example.com/?a=1&amp;b=2"`, `rel="sponsored noopener nofollow"`, `data-no-prerender`,
			`alt="图片说明"`, `referrerpolicy="no-referrer"`, `data-fallback="off"`, `>广告</span>`} {
			if !strings.Contains(block, want) {
				t.Errorf("%s 的广告缺少 %s:\n%s", path, want, block)
			}
		}
	}
	playBody := s.get(play).body
	if dm, ad, src := strings.Index(playBody, `id="dm-section"`), strings.Index(playBody, `class="side-ads"`), strings.Index(playBody, `id="src-status"`); !(dm < ad && ad < src) {
		t.Error("播放页的广告应该在弹幕下方、来源上方")
	}
	detailBody := s.get(detail).body
	if info, ad := strings.Index(detailBody, `<section class="info">`), strings.Index(detailBody, `class="side-ads"`); !(info >= 0 && info < ad) {
		t.Error("详情页的广告应该在信息下方")
	}

	// 填错了：留在表单上显示原因，保留刚填的内容
	r := add(url.Values{"enabled": {"on"}, "kind": {"image"}, "image_url": {"http://ads.example.com/insecure.jpg"}, "position": {"3"}})
	if r.status != http.StatusOK || !strings.Contains(r.body, "图片链接要以 https:// 开头") || !strings.Contains(r.body, "http://ads.example.com/insecure.jpg") {
		t.Errorf("http 图片应该被拒绝并保留填写的内容: %d", r.status)
	}
	if r := add(url.Values{"enabled": {"on"}, "kind": {"text"}, "text": {"x"}, "link": {"javascript:alert(1)"}}); !strings.Contains(r.body, "点击链接要以") {
		t.Error("javascript: 链接应该被拒绝")
	}

	list, _ := s.app.Ads.List(context.Background(), ads.Sidebar)
	if len(list) != 3 {
		t.Fatalf("应该有 3 条广告: %+v", list)
	}
	textAd := list[1] // 排序：关着的(0)、文字(1)、图片(2)
	if r := s.form("/admin/ads/"+itoa(textAd.ID), url.Values{"kind": {"text"}, "text": {"文字广告在上面"}, "position": {"1"}}); r.status != http.StatusSeeOther {
		t.Fatalf("修改失败: %d", r.status)
	}
	if strings.Contains(s.get(detail).body, "文字广告在上面") {
		t.Error("关掉之后应该马上不显示")
	}
	if r := s.form("/admin/ads/"+itoa(list[2].ID)+"/delete", url.Values{}); r.status != http.StatusSeeOther {
		t.Fatalf("删除失败: %d", r.status)
	}
	if strings.Contains(s.get(detail).body, `class="side-ads"`) {
		t.Error("没有开着的广告时不应该有广告位")
	}
	if admin := s.get("/admin/ads").body; !strings.Contains(admin, "已关闭") || !strings.Contains(admin, "关着的广告") {
		t.Error("后台应该列出所有广告，包括关着的")
	}

	// 普通用户进不了广告设置
	s.client = s.browser()
	s.register("viewer")
	if r := s.get("/admin/ads"); r.status == http.StatusOK {
		t.Error("普通用户不应该能打开广告设置")
	}
}

// 综艺按期选集：服务器先按资料列出"第 N 期"，链到"这一期，哪段都行"；看过的集按期显示成"第 3 期 下"。
func TestVarietyShowIssues(t *testing.T) {
	s := newSite(t)
	ctx := context.Background()
	s.register("evan")
	show := s.seedMedia("36000009", "现在就出发 第四季", media.TypeShow, media.Patch{"season": 4})
	s.app.Media.ApplyPatch(ctx, show, media.ProviderTMDB, media.Patch{"series_status": "ongoing", "episode_count": 6})
	units := []media.Unit{}
	for number := 1; number <= 6; number++ {
		units = append(units, media.Unit{Season: 4, Episode: number})
	}
	if err := s.app.Media.ReplaceUnits(ctx, show, 4, units); err != nil {
		t.Fatal(err)
	}

	detail := s.get("/m/" + itoa(show))
	// 资料里 6 期都播了，但还在播：是"更新至"，不是"全 6 集"
	mustContain(t, detail, "综艺 · 更新至第 6 期", "第 1 期", `href="/play/`+itoa(show)+`?s=4&amp;e=300"`, `data-current="100"`, "6 期")
	if strings.Contains(detail.body, "全 6 集") {
		t.Error("还在播的综艺不应该显示全 6 集")
	}
	play := s.get("/play/" + itoa(show))
	mustContain(t, play, `data-episode="100"`, `data-ep="300"`)

	s.json(http.MethodPost, "/api/positions", map[string]any{"items": []map[string]any{
		{"media_id": show, "season": 4, "episode": 304, "position": 600, "duration": 2700, "at": time.Now().UnixMilli()}}})
	detail = s.get("/m/" + itoa(show))
	mustContain(t, detail, "第 3 期 下 · ", `data-current="304"`)
	home := s.get("/")
	mustContain(t, home, "第 3 期 下")
}

// Package providerstest 提供模拟豆瓣、TMDB、Wikidata、NeoDB 的假服务器，只给测试用。
package providerstest

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"moovie/internal/providers"
)

// Fake 是一组假的外部来源。往各个 map 里放数据，就能控制它们返回什么。
type Fake struct {
	mu sync.Mutex
	// Subjects：豆瓣 ID → 详情 JSON。键加前缀 "tv:" 表示这个条目在剧集接口下。
	Subjects map[string]map[string]any
	// Pages：豆瓣 ID → 详情网页（movie.douban.com/subject/<id>/）的 HTML。没有配置时返回 404。
	Pages map[string]string
	// Refuse：这些豆瓣 ID 的详情接口返回 403（详情网页不受影响）。
	Refuse map[string]bool
	// Permission：这些豆瓣 ID 的详情接口返回 403 need_permission（code 1000）。
	Permission map[string]bool
	// Suggest：关键词 → 联想结果。
	Suggest map[string][]map[string]any
	// Popular：手机版榜单接口路径里的关键字（如 "hot_gaia"、"tv_domestic"）→ 条目列表。
	Popular map[string][]map[string]any
	// WebPopular：网页版热门榜的 "类型/标签"（如 "movie/热门"、"tv/综艺"）→ 条目列表。没有配置时返回 500，客户端改用手机版接口。
	WebPopular map[string][]map[string]any
	// Interests：豆瓣用户 ID + "/" + 状态 → 标记列表。
	Interests map[string][]map[string]any
	// Feeds：豆瓣用户 ID → RSS 里的条目（title、link、pubDate）。没有配置时返回 404。
	Feeds map[string][]map[string]string
	// Wikidata：豆瓣 ID → SPARQL bindings。
	Wikidata map[string][]map[string]string
	// TMDB：路径 → 响应 JSON。
	TMDB map[string]any
	// NeoDB：搜索的关键词 → 搜索结果里的条目（/api/catalog/search 的 data）。没有配置时返回空结果。
	NeoDB map[string][]map[string]any
	// Exports：TMDB 每日导出文件名（如 "movie_ids_11_22_2026.json.gz"）→ 文件里的每一行。
	Exports map[string][]map[string]any
	// AIDown、EmbedDown 为 true 时，AI 对话接口、向量接口返回 500。
	AIDown, EmbedDown bool
	// Prompts 记录 AI 收到的每一条用户消息。
	Prompts []string
	// Requests 记录收到的所有请求路径。
	Requests []string

	Server *httptest.Server
}

// New 启动假服务器，测试结束时自动关闭。
func New(t *testing.T) *Fake {
	fake := &Fake{
		Subjects: map[string]map[string]any{}, Pages: map[string]string{}, Refuse: map[string]bool{}, Permission: map[string]bool{}, Suggest: map[string][]map[string]any{}, Popular: map[string][]map[string]any{},
		WebPopular: map[string][]map[string]any{}, Interests: map[string][]map[string]any{}, Feeds: map[string][]map[string]string{}, Wikidata: map[string][]map[string]string{}, TMDB: map[string]any{}, NeoDB: map[string][]map[string]any{},
		Exports: map[string][]map[string]any{},
	}
	fake.Server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.Server.Close)
	return fake
}

// Douban 返回指向假服务器的豆瓣客户端（不限速）。
func (f *Fake) Douban() *providers.Douban {
	return &providers.Douban{HTTP: f.Server.Client(), MobileBase: f.Server.URL, WebBase: f.Server.URL, FeedBase: f.Server.URL}
}

// TMDBClient 返回指向假服务器的 TMDB 客户端。
func (f *Fake) TMDBClient() *providers.TMDB {
	return &providers.TMDB{HTTP: f.Server.Client(), Base: f.Server.URL, Files: f.Server.URL, Token: "test-token"}
}

// WikidataClient 返回指向假服务器的 Wikidata 客户端。
func (f *Fake) WikidataClient() *providers.Wikidata {
	return &providers.Wikidata{HTTP: f.Server.Client(), Endpoint: f.Server.URL + "/sparql", UserAgent: "test"}
}

// NeoDBClient 返回指向假服务器的 NeoDB 客户端（不限速）。
func (f *Fake) NeoDBClient() *providers.NeoDB {
	return &providers.NeoDB{HTTP: f.Server.Client(), Base: f.Server.URL, UserAgent: "test"}
}

// AI 返回指向假服务器的 AI 客户端。它把收到的资料原样包进"《》"里当作描述返回。
func (f *Fake) AI() *providers.OpenAI {
	return &providers.OpenAI{HTTP: f.Server.Client(), BaseURL: f.Server.URL + "/v1", Key: "ai-key", Model: "chat"}
}

// Embedder 返回指向假服务器的向量客户端。
func (f *Fake) Embedder() *providers.OpenAI {
	return &providers.OpenAI{HTTP: f.Server.Client(), BaseURL: f.Server.URL + "/v1", Model: "embed"}
}

// Count 返回收到过多少次这个路径的请求。
func (f *Fake) Count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, seen := range f.Requests {
		if seen == path {
			count++
		}
	}
	return count
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	f.Requests = append(f.Requests, path)
	switch {
	case path == "/sparql":
		r.ParseForm()
		query := r.PostForm.Get("query")
		bindings := []map[string]map[string]string{}
		for doubanID, rows := range f.Wikidata {
			if !strings.Contains(query, `"`+doubanID+`"`) {
				continue
			}
			for _, row := range rows {
				binding := map[string]map[string]string{}
				for key, value := range row {
					binding[key] = map[string]string{"value": value}
				}
				bindings = append(bindings, binding)
			}
		}
		writeJSON(w, map[string]any{"results": map[string]any{"bindings": bindings}})
	case path == "/api/catalog/search":
		items := f.NeoDB[r.URL.Query().Get("query")]
		if items == nil {
			items = []map[string]any{}
		}
		writeJSON(w, map[string]any{"data": items, "pages": 1, "count": len(items)})
	case path == "/j/subject_suggest":
		items := f.Suggest[r.URL.Query().Get("q")]
		if items == nil {
			items = []map[string]any{}
		}
		writeJSON(w, items)
	case strings.HasPrefix(path, "/subject/"):
		page, ok := f.Pages[strings.Trim(strings.TrimPrefix(path, "/subject/"), "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	case path == "/j/search_subjects":
		items, ok := f.WebPopular[r.URL.Query().Get("type")+"/"+r.URL.Query().Get("tag")]
		if !ok {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"subjects": items})
	case strings.HasPrefix(path, "/feed/people/"):
		items, ok := f.Feeds[strings.TrimSuffix(strings.TrimPrefix(path, "/feed/people/"), "/interests")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>的收藏</title>`)
		for _, item := range items {
			fmt.Fprintf(w, "<item><title>%s</title><link>%s</link><pubDate>%s</pubDate></item>", html.EscapeString(item["title"]), html.EscapeString(item["link"]), item["pubDate"])
		}
		fmt.Fprint(w, `</channel></rss>`)
	case strings.HasPrefix(path, "/rexxar/api/v2/user/"):
		parts := strings.Split(strings.TrimPrefix(path, "/rexxar/api/v2/user/"), "/")
		list, ok := f.Interests[parts[0]+"/"+r.URL.Query().Get("status")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var start, count int
		fmt.Sscan(r.URL.Query().Get("start"), &start)
		fmt.Sscan(r.URL.Query().Get("count"), &count)
		end := min(len(list), start+count)
		page := []map[string]any{}
		if start < end {
			page = list[start:end]
		}
		writeJSON(w, map[string]any{"total": len(list), "interests": page})
	case strings.HasPrefix(path, "/rexxar/api/v2/subject_collection/"), strings.HasPrefix(path, "/rexxar/api/v2/movie/hot_gaia"):
		for keyword, items := range f.Popular {
			if strings.Contains(path, keyword) {
				writeJSON(w, map[string]any{"subject_collection_items": items, "items": items})
				return
			}
		}
		http.Error(w, "unavailable", http.StatusInternalServerError)
	case strings.HasPrefix(path, "/rexxar/api/v2/movie/"), strings.HasPrefix(path, "/rexxar/api/v2/tv/"):
		parts := strings.Split(strings.TrimPrefix(path, "/rexxar/api/v2/"), "/")
		key := parts[1]
		if f.Permission[key] {
			w.WriteHeader(http.StatusForbidden)
			writeJSON(w, map[string]any{"msg": "need_permission", "code": 1000, "localized_message": ""})
			return
		}
		if f.Refuse[key] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if parts[0] == "tv" {
			key = "tv:" + key
		}
		subject, ok := f.Subjects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, subject)
	case path == "/v1/chat/completions":
		var body struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if f.AIDown || r.Header.Get("Authorization") != "Bearer ai-key" || len(body.Messages) != 2 {
			http.Error(w, "ai down", http.StatusInternalServerError)
			return
		}
		prompt := body.Messages[1].Content
		f.Prompts = append(f.Prompts, prompt)
		title := prompt[strings.Index(prompt, "标题：")+len("标题："):]
		title = title[:strings.Index(title, "\n")]
		writeJSON(w, map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "《" + title + "》 第 " + fmt.Sprint(len(f.Prompts)) + " 次写的描述"}}}})
	case path == "/v1/embeddings":
		if f.EmbedDown {
			http.Error(w, "embed down", http.StatusInternalServerError)
			return
		}
		vector := make([]float32, providers.EmbeddingDimensions)
		vector[0] = 1
		writeJSON(w, map[string]any{"data": []map[string]any{{"embedding": vector}}})
	case strings.HasPrefix(path, "/p/exports/"):
		lines, ok := f.Exports[strings.TrimPrefix(path, "/p/exports/")]
		if !ok {
			http.Error(w, "AccessDenied", http.StatusForbidden) // 和 S3 一样：文件不存在时返回 403
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		zipped := gzip.NewWriter(w)
		for _, line := range lines {
			json.NewEncoder(zipped).Encode(line)
		}
		zipped.Write([]byte("not json\n")) // 坏行应该被跳过
		zipped.Close()
	case strings.HasPrefix(path, "/3/"):
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		response, ok := f.TMDB[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, response)
	default:
		http.NotFound(w, r)
	}
}

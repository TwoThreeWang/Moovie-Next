// Package providerstest 提供模拟豆瓣、TMDB、Wikidata 的假服务器，只给测试用。
package providerstest

import (
	"encoding/json"
	"fmt"
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
	// Suggest：关键词 → 联想结果。
	Suggest map[string][]map[string]any
	// Popular：接口路径里的关键字（如 "hot_gaia"、"tv_domestic"）→ 条目列表。
	Popular map[string][]map[string]any
	// Interests：豆瓣用户 ID + "/" + 状态 → 标记列表。
	Interests map[string][]map[string]any
	// Wikidata：豆瓣 ID → SPARQL bindings。
	Wikidata map[string][]map[string]string
	// TMDB：路径 → 响应 JSON。
	TMDB map[string]any
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
		Subjects: map[string]map[string]any{}, Suggest: map[string][]map[string]any{}, Popular: map[string][]map[string]any{},
		Interests: map[string][]map[string]any{}, Wikidata: map[string][]map[string]string{}, TMDB: map[string]any{},
	}
	fake.Server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.Server.Close)
	return fake
}

// Douban 返回指向假服务器的豆瓣客户端（不限速）。
func (f *Fake) Douban() *providers.Douban {
	return &providers.Douban{HTTP: f.Server.Client(), MobileBase: f.Server.URL, WebBase: f.Server.URL}
}

// TMDBClient 返回指向假服务器的 TMDB 客户端。
func (f *Fake) TMDBClient() *providers.TMDB {
	return &providers.TMDB{HTTP: f.Server.Client(), Base: f.Server.URL, Token: "test-token"}
}

// WikidataClient 返回指向假服务器的 Wikidata 客户端。
func (f *Fake) WikidataClient() *providers.Wikidata {
	return &providers.Wikidata{HTTP: f.Server.Client(), Endpoint: f.Server.URL + "/sparql", UserAgent: "test"}
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
	case path == "/j/subject_suggest":
		items := f.Suggest[r.URL.Query().Get("q")]
		if items == nil {
			items = []map[string]any{}
		}
		writeJSON(w, items)
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

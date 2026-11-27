package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// ---------- Wikidata ----------

// Wikidata 用 SPARQL 查询条目的外部 ID 和各语言片名。
type Wikidata struct {
	HTTP      *http.Client
	Endpoint  string
	UserAgent string
}

// WikidataEntity 是 Wikidata 上的一个影视条目。
type WikidataEntity struct {
	QID         string
	IMDbID      string
	TMDBMovieID string
	TMDBTVID    string
	// Title 是中文片名（简体优先，其次繁体），没有中文标签时是英文标签，都没有时为空。
	Title  string
	Labels []string
}

// titleLanguages 是 Wikidata 标签作为片名时的先后。
var titleLanguages = []string{"zh-cn", "zh-hans", "zh", "zh-tw", "zh-hant", "zh-hk", "en"}

// ByDoubanID 用豆瓣 ID 找到 Wikidata 条目。Wikidata 上没有这个豆瓣 ID 时返回 ErrNotFound。
func (w *Wikidata) ByDoubanID(ctx context.Context, doubanID string) (*WikidataEntity, error) {
	for _, r := range doubanID {
		if r < '0' || r > '9' {
			return nil, fmt.Errorf("豆瓣 ID 不合法: %q", doubanID)
		}
	}
	query := fmt.Sprintf(`SELECT ?item ?imdb ?tmdbMovie ?tmdbTV ?label ?lang WHERE {
  ?item wdt:P4529 "%s".
  OPTIONAL { ?item wdt:P345 ?imdb. }
  OPTIONAL { ?item wdt:P4947 ?tmdbMovie. }
  OPTIONAL { ?item wdt:P4983 ?tmdbTV. }
  OPTIONAL { ?item rdfs:label ?label. FILTER(LANG(?label) IN ("zh", "zh-cn", "zh-hans", "zh-hant", "zh-tw", "zh-hk", "en", "ja", "ko")) BIND(LANG(?label) AS ?lang) }
}`, doubanID)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.Endpoint, strings.NewReader(url.Values{"query": {query}}.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/sparql-results+json")
	request.Header.Set("User-Agent", w.UserAgent)
	var response struct {
		Results struct {
			Bindings []map[string]struct {
				Value string `json:"value"`
			} `json:"bindings"`
		} `json:"results"`
	}
	if err := doJSON(w.HTTP, request, "Wikidata", &response); err != nil {
		return nil, err
	}
	if len(response.Results.Bindings) == 0 {
		return nil, ErrNotFound
	}
	entity := &WikidataEntity{}
	seen := map[string]bool{}
	best := len(titleLanguages)
	for _, binding := range response.Results.Bindings {
		if item := binding["item"].Value; item != "" {
			entity.QID = item[strings.LastIndexByte(item, '/')+1:]
		}
		if value := binding["imdb"].Value; value != "" {
			entity.IMDbID = value
		}
		if value := binding["tmdbMovie"].Value; value != "" {
			entity.TMDBMovieID = value
		}
		if value := binding["tmdbTV"].Value; value != "" {
			entity.TMDBTVID = value
		}
		label := binding["label"].Value
		if label == "" {
			continue
		}
		if rank := slices.Index(titleLanguages, binding["lang"].Value); rank >= 0 && rank < best {
			best, entity.Title = rank, label
		}
		if !seen[label] {
			seen[label] = true
			entity.Labels = append(entity.Labels, label)
		}
	}
	return entity, nil
}

// ---------- AI：OpenAI 格式的接口 ----------

// EmbeddingDimensions 是向量的维度，必须和数据库列一致。
const EmbeddingDimensions = 768

// maxEmbeddingRunes 是送进向量模型的最多字数。bge-base-zh 一次最多读 512 个 token，
// 中文差不多一字一个，多出来的会被模型悄悄丢掉，所以要留余量。和老系统保持一致。
const maxEmbeddingRunes = 450

// OpenAI 是 OpenAI 格式接口的客户端（/chat/completions、/embeddings），
// Cloudflare AI Gateway、各家大模型和 Ollama 的 /v1 都是这个格式。BaseURL 为空表示没有配置。
type OpenAI struct {
	HTTP    *http.Client
	BaseURL string // 不含 /chat/completions 这类后缀，比如 http://ollama:11434/v1
	Key     string
	Model   string
}

// Enabled 表示是否配置了接口地址和模型。
func (o *OpenAI) Enabled() bool { return o != nil && o.BaseURL != "" && o.Model != "" }

func (o *OpenAI) post(ctx context.Context, path string, body, destination any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if o.Key != "" {
		request.Header.Set("Authorization", "Bearer "+o.Key)
	}
	return doJSON(o.HTTP, request, "AI 接口", destination)
}

// Chat 发一轮对话，返回模型的回答。
func (o *OpenAI) Chat(ctx context.Context, system, user string) (string, error) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	body := map[string]any{"model": o.Model, "messages": []message{{"system", system}, {"user", user}}}
	if err := o.post(ctx, "/chat/completions", body, &response); err != nil {
		return "", err
	}
	if response.Error != nil {
		return "", fmt.Errorf("AI 接口返回错误: %s", response.Error.Message)
	}
	if len(response.Choices) == 0 || strings.TrimSpace(response.Choices[0].Message.Content) == "" {
		return "", errors.New("AI 接口没有返回内容")
	}
	return strings.TrimSpace(response.Choices[0].Message.Content), nil
}

// Embed 把一段文字转成向量。超过 450 字的部分不送进去。
func (o *OpenAI) Embed(ctx context.Context, text string) ([]float32, error) {
	if runes := []rune(text); len(runes) > maxEmbeddingRunes {
		text = string(runes[:maxEmbeddingRunes])
	}
	var response struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := o.post(ctx, "/embeddings", map[string]any{"model": o.Model, "input": text}, &response); err != nil {
		return nil, err
	}
	if len(response.Data) == 0 || len(response.Data[0].Embedding) != EmbeddingDimensions {
		got := 0
		if len(response.Data) > 0 {
			got = len(response.Data[0].Embedding)
		}
		return nil, fmt.Errorf("向量维度不对：需要 %d，实际 %d", EmbeddingDimensions, got)
	}
	return response.Data[0].Embedding, nil
}

// ---------- 第三方弹幕库（dandanplay 协议） ----------

// Danmu 是第三方弹幕库的客户端。它按"片名 + 季 + 集"查弹幕，和播放源无关。
type Danmu struct {
	HTTP *http.Client
	Base string
}

// Enabled 表示是否配置了弹幕库地址。
func (d *Danmu) Enabled() bool { return d != nil && d.Base != "" }

// DanmuComment 是一条弹幕。Mode：0 滚动，1 顶部，2 底部。
type DanmuComment struct {
	Time  float64 `json:"time"`
	Text  string  `json:"text"`
	Mode  int     `json:"mode"`
	Color string  `json:"color"`
}

// Fetch 分两步取弹幕：先用"片名 S01E01"匹配到剧集 ID，再按 ID 拉弹幕列表。匹配不到时返回空列表。
func (d *Danmu) Fetch(ctx context.Context, title string, season, episode int) ([]DanmuComment, error) {
	fileName := title
	if episode > 0 {
		fileName = fmt.Sprintf("%s S%02dE%02d", title, season, episode)
	}
	payload, _ := json.Marshal(map[string]string{"fileName": fileName})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Base+"/api/v2/match", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	var match struct {
		IsMatched bool `json:"isMatched"`
		Matches   []struct {
			EpisodeID int64 `json:"episodeId"`
		} `json:"matches"`
	}
	if err := doJSON(d.HTTP, request, "弹幕库", &match); err != nil {
		return nil, err
	}
	if !match.IsMatched || len(match.Matches) == 0 || match.Matches[0].EpisodeID == 0 {
		return []DanmuComment{}, nil
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, d.Base+"/api/v2/comment/"+strconv.FormatInt(match.Matches[0].EpisodeID, 10)+"?format=json", nil)
	if err != nil {
		return nil, err
	}
	var comments struct {
		Comments []struct {
			Parameters string `json:"p"`
			Message    string `json:"m"`
		} `json:"comments"`
	}
	if err := doJSON(d.HTTP, request, "弹幕库", &comments); err != nil {
		if err == ErrNotFound {
			return []DanmuComment{}, nil
		}
		return nil, err
	}
	result := make([]DanmuComment, 0, len(comments.Comments))
	for _, comment := range comments.Comments {
		// p 字段是"秒数,模式,颜色,..."的逗号串。
		text := strings.TrimSpace(comment.Message)
		parts := strings.Split(comment.Parameters, ",")
		if text == "" || len(parts) < 3 {
			continue
		}
		seconds, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		if err != nil || seconds < 0 {
			continue
		}
		mode := 0
		switch strings.TrimSpace(parts[1]) {
		case "4":
			mode = 2
		case "5":
			mode = 1
		}
		color := "#FFFFFF"
		if value, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64); err == nil && value >= 0 && value <= 0xFFFFFF {
			color = fmt.Sprintf("#%06X", value)
		}
		result = append(result, DanmuComment{Time: seconds, Text: text, Mode: mode, Color: color})
	}
	return result, nil
}

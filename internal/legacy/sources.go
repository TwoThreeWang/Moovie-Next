package legacy

// 临时功能：把旧版保存的播放源导出成一个订阅文件，网站在 /sub/example.json 提供（见 internal/app/example_subscription.go）。
// 只是为了让老用户切过来时有源可用，之后会整块删掉。删的时候一起删：这个文件、cmd/migrate-legacy 的 -sources 参数、
// internal/app/example_subscription.go、设置页的"使用示例订阅"按钮，以及配置 EXAMPLE_SUBSCRIPTION_FILE。

import (
	"context"
	"net/url"
	"strings"

	"moovie/internal/db"
)

// Source 是订阅文件里的一个源，格式和设置页说明的一样。
type Source struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	API  string `json:"api"`
}

// Subscription 是订阅文件。
type Subscription struct {
	Version int      `json:"version"`
	Sources []Source `json:"sources"`
}

// ExportSources 读出旧库里启用的源：同一个地址只留一个；地址不对的跳过，写进 skipped。
// HTTP 的也留着（查询由本站转发）。key 用接口的域名，保持稳定。
func ExportSources(ctx context.Context, src *db.Pool) (Subscription, []string, error) {
	rows, err := src.Query(ctx, `SELECT COALESCE(key, ''), COALESCE(base_url, '') FROM sites WHERE enabled ORDER BY id`)
	if err != nil {
		return Subscription{}, nil, err
	}
	defer rows.Close()
	subscription := Subscription{Version: 1, Sources: []Source{}}
	var skipped []string
	seenURL, seenKey := map[string]bool{}, map[string]int{}
	for rows.Next() {
		var name, api string
		if err := rows.Scan(&name, &api); err != nil {
			return Subscription{}, nil, err
		}
		name, api = strings.TrimSpace(name), strings.TrimSpace(api)
		parsed, err := url.Parse(api)
		if err != nil || parsed.Host == "" {
			skipped = append(skipped, name+"：接口地址不正确")
			continue
		}
		normalized := strings.ToLower(strings.TrimRight(api, "/"))
		if seenURL[normalized] {
			continue
		}
		seenURL[normalized] = true
		if parsed.Scheme != "https" && parsed.Scheme != "http" {
			skipped = append(skipped, name+"：接口地址不正确")
			continue
		}
		key := strings.ToLower(parsed.Hostname())
		if seenKey[key]++; seenKey[key] > 1 {
			key += "-" + string(rune('0'+seenKey[key]))
		}
		if name == "" {
			name = parsed.Hostname()
		}
		subscription.Sources = append(subscription.Sources, Source{Key: key, Name: name, API: api})
	}
	return subscription, skipped, rows.Err()
}

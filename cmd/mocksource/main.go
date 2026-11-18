// mocksource 是开发和验收用的"假资源站"：模拟苹果 CMS 接口和一份订阅文件。
//
// 它只用于本机测试浏览器端的查源引擎，不是产品的一部分，也不会被 web 服务调用。
// 视频用的是公开的 HLS 测试流（Big Buck Bunny）。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const stream = "https://test-streams.mux.dev/x36xhzz"

type vod struct {
	ID       int
	Name     string
	Year     string
	Type     string
	DoubanID string
	Episodes int // 0 表示电影
	Remarks  string
}

// 两个假源的片库。有的作品在本站的演示作品库里（能匹配上），有的不在（走加密历史）。
var catalog = map[string][]vod{
	"alpha": {
		{1, "雾港 第二季", "2026", "国产剧", "", 8, "更新至08集"},
		{2, "雾港", "2024", "国产剧", "", 12, "全12集"},
		{3, "回声", "2024", "剧情片", "", 0, "HD"},
		{4, "回声", "2010", "剧情片", "", 0, "HD"},
		{5, "山海食堂", "2025", "国产剧", "", 10, "全10集"},
		{6, "深夜便利店", "2023", "日韩剧", "", 6, "全06集"},
	},
	"beta": {
		{11, "雾港第二季", "2026", "连续剧", "", 8, "第08集"},
		{12, "长夜将尽", "2025", "动作片", "", 0, "正片"},
		{13, "星陨之地", "2026", "日韩动漫", "", 4, "更新至04集"},
	},
	"gamma": {
		{21, "雾港 第二季", "2026", "国产剧", "", 6, "更新至06集"},
		// 作品库里没有的作品，在别的源也有：测试"原来的源找不到时换源"。一部年份对得上，一部没写年份。
		{22, "深夜便利店", "2023", "日韩剧", "", 6, "全06集"},
		{23, "星陨之地", "", "日韩动漫", "", 4, "更新至04集"},
	},
}

func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next(w, r)
	}
}

func main() {
	port := "5210"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}
	base := "http://localhost:" + port
	mux := http.NewServeMux()

	mux.HandleFunc("/subscription.json", cors(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"version": 1, "sources": []map[string]string{
			{"key": "alpha", "name": "演示源 A", "api": base + "/alpha/api.php/provide/vod/"},
			{"key": "beta", "name": "演示源 B", "api": base + "/beta/api.php/provide/vod/"},
			{"key": "gamma", "name": "演示源 C（XML）", "api": base + "/gamma/api.php/provide/vod/at/xml/"},
			{"key": "down", "name": "演示源 D（故障）", "api": base + "/down/api.php/provide/vod/"},
			{"key": "plain", "name": "演示源 E（HTTP）", "api": "http://plain.example/api.php/provide/vod/"},
		}})
	}))

	mux.HandleFunc("/", cors(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 2 || parts[1] != "api.php" {
			http.NotFound(w, r)
			return
		}
		source := parts[0]
		if source == "down" {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		keyword := r.URL.Query().Get("wd")
		var hits []vod
		for _, item := range catalog[source] {
			if keyword == "" || strings.Contains(item.Name, keyword) {
				hits = append(hits, item)
			}
		}
		// B 源慢一点，用来看"先到先显示"。
		if source == "beta" {
			time.Sleep(700 * time.Millisecond)
		}
		playURL := func(item vod) string {
			if item.Episodes == 0 {
				return "HD$" + base + "/hls/plain.m3u8?v=" + strconv.Itoa(item.ID)
			}
			var episodes []string
			for n := 1; n <= item.Episodes; n++ {
				kind := "plain"
				if n == 1 {
					kind = "ads" // 第 1 集带片头和片中广告，用来测广告跳过
				}
				episodes = append(episodes, fmt.Sprintf("第%02d集$%s/hls/%s.m3u8?v=%d&e=%d", n, base, kind, item.ID, n))
			}
			return strings.Join(episodes, "#")
		}
		if strings.Contains(r.URL.Path, "/at/xml") {
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?><rss version="5.1"><list page="1" pagecount="1">`)
			for _, item := range hits {
				fmt.Fprintf(w, `<video><id>%d</id><name><![CDATA[%s]]></name><type>%s</type><year>%s</year><note><![CDATA[%s]]></note><dl><dd flag="xmlm3u8"><![CDATA[%s]]></dd></dl></video>`,
					item.ID, item.Name, item.Type, item.Year, item.Remarks, playURL(item))
			}
			fmt.Fprint(w, `</list></rss>`)
			return
		}
		list := []map[string]any{}
		for _, item := range hits {
			list = append(list, map[string]any{
				"vod_id": item.ID, "vod_name": item.Name, "vod_year": item.Year, "type_name": item.Type, "vod_douban_id": item.DoubanID,
				"vod_pic": fmt.Sprintf("https://picsum.photos/seed/mock%d/300/450.webp", item.ID), "vod_remarks": item.Remarks,
				// 故意带一段脚本：浏览器端必须把它当成纯文本处理。
				"vod_content":   `<p>演示数据</p><script>document.title='XSS'</script>`,
				"vod_play_from": "demo$$$backup", "vod_play_url": playURL(item) + "$$$" + playURL(item),
			})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{"code": 1, "page": 1, "pagecount": 1, "total": len(list), "list": list})
	}))

	// 普通的一集：直接转到公开测试流。
	mux.HandleFunc("/hls/plain.m3u8", cors(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, stream+"/x36xhzz.m3u8", http.StatusFound)
	}))
	// 带广告的一集：开头一段 20 秒的"广告"，片中另一段"广告"出现两次（用低清晰度的分片冒充），
	// 和正片之间有 DISCONTINUITY。开头那段的指纹在演示数据里已经预先确认。
	mux.HandleFunc("/hls/ads.m3u8", cors(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:11\n")
		ad := func(dir string) {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
			for n := 0; n < 2; n++ {
				fmt.Fprintf(&b, "#EXTINF:10.000,\n/hls/%s/%d.ts\n", dir, n)
			}
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		ad("ad")
		for n := 0; n < 30; n++ {
			if n == 6 || n == 20 {
				ad("promo")
			}
			fmt.Fprintf(&b, "#EXTINF:10.000,\n/hls/main/%d.ts\n", n)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, b.String())
	}))
	// 各个清晰度的分片编号起点不同：720p 从 462 开始，240p 从 526 开始。
	segment := func(level, file string, first int) http.HandlerFunc {
		return cors(func(w http.ResponseWriter, r *http.Request) {
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			n, err := strconv.Atoi(strings.TrimSuffix(name, ".ts"))
			if err != nil || n < 0 || n > 60 {
				http.NotFound(w, r)
				return
			}
			http.Redirect(w, r, fmt.Sprintf("%s/%s/url_%d/%s.ts", stream, level, first+n, file), http.StatusFound)
		})
	}
	mux.HandleFunc("/hls/main/", segment("url_0", "193039199_mp4_h264_aac_hd_7", 462))
	mux.HandleFunc("/hls/ad/", segment("url_2", "193039199_mp4_h264_aac_ld_7", 526))
	mux.HandleFunc("/hls/promo/", segment("url_2", "193039199_mp4_h264_aac_ld_7", 540))

	log.Printf("假资源站已启动：订阅地址 %s/subscription.json", base)
	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, mux))
}

package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io/fs"
	"sort"
	"strings"

	"moovie/web"
)

// moduleMap 让前端的 ES 模块都带上版本号，从而可以长期缓存。
//
// 模块之间用相对路径互相 import，地址里没有版本号；浏览器每次都得问服务器"有没有变"，
// 手机上一个页面要多出好几轮往返。import map 把每个模块的地址映射到带版本号的地址，
// 再用 modulepreload 把常用模块一次性并行下载，不必等上一层解析完才发现下一层。
type moduleMap struct {
	head template.HTML // 放进 <head> 的 import map、预加载标签和预渲染规则
	hash string        // 这几段内联脚本的哈希（空格分隔），加进 CSP 才能执行
}

// speculationRules 让浏览器在用户把手指或鼠标停在链接上时提前渲染下一页。
// 播放页、登录、后台和通知不预渲染（有副作用或很重）。
// 较旧的 Chromium 不认 CSP 的 'inline-speculation-rules'，所以和 import map 一样用哈希放行。
const speculationRules = `{"prerender":[{"where":{"and":[{"href_matches":"/*"},{"not":{"href_matches":"/play/*"}},{"not":{"href_matches":"/auth/*"}},{"not":{"href_matches":"/admin*"}},{"not":{"href_matches":"/notifications"}},{"not":{"selector_matches":"[data-no-prerender]"}}]},"eagerness":"moderate"}]}`

// imageGuard 是放在 <head> 最前面的一小段脚本，比页面里的任何图片都早开始监听：
//   - 图片加载完成时加上 is-loaded，大图据此从透明渐显（样式见 site.css 的 img.fade）；
//   - 图片加载失败时换成站内的默认图（<picture> 里的其他尺寸一并去掉），不显示破图图标。
//
// 放在页面脚本里就晚了：首屏的图片可能在它运行之前就已经加载完或失败了。
const imageGuard = `(function(d){d.documentElement.classList.add("js");` +
	`d.addEventListener("load",function(e){var t=e.target;if(t.tagName==="IMG")t.classList.add("is-loaded")},true);` +
	`d.addEventListener("error",function(e){var t=e.target;if(t.tagName!=="IMG"||t.dataset.fallback)return;t.dataset.fallback="1";` +
	`var p=t.parentNode;if(p&&p.tagName==="PICTURE")p.querySelectorAll("source").forEach(function(s){s.remove()});` +
	`t.removeAttribute("srcset");t.src="/static/img/fallback.svg"},true)})(document)`

func scriptHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// preloaded 是几乎每个页面都会用到的模块，提前并行下载。
var preloaded = []string{"app.js", "core/api.js", "core/idb.js", "core/keys.js", "core/sources.js", "core/history.js",
	"lib/title.js", "lib/episode.js", "lib/cms.js", "lib/match.js", "lib/vault.js"}

func buildModuleMap(version string) (moduleMap, error) {
	imports := map[string]string{}
	err := fs.WalkDir(web.Files, "static/js", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".min.js") {
			return err
		}
		url := "/" + path
		imports[url] = url + "?v=" + version
		return nil
	})
	if err != nil {
		return moduleMap{}, err
	}
	encoded, err := json.Marshal(map[string]any{"imports": imports})
	if err != nil {
		return moduleMap{}, err
	}
	var head strings.Builder
	head.WriteString("<script>" + imageGuard + "</script>\n")
	head.WriteString(`<script type="importmap">`)
	head.Write(encoded)
	head.WriteString("</script>\n")
	keys := make([]string, 0, len(preloaded))
	for _, name := range preloaded {
		if _, ok := imports["/static/js/"+name]; ok {
			keys = append(keys, "/static/js/"+name)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		head.WriteString(`<link rel="modulepreload" href="` + template.HTMLEscapeString(imports[key]) + `">` + "\n")
	}
	head.WriteString(`<script type="speculationrules">` + speculationRules + "</script>\n")
	return moduleMap{head: template.HTML(head.String()), hash: scriptHash([]byte(imageGuard)) + " " + scriptHash(encoded) + " " + scriptHash([]byte(speculationRules))}, nil
}

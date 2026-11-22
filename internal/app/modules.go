package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io/fs"
	"path"
	"regexp"
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
	head template.HTML // 放进 <head> 的 import map、app.js 整棵依赖树的预加载标签和预渲染规则
	hash string        // 这几段内联脚本的哈希（空格分隔），加进 CSP 才能执行
	// pages 是每个页面脚本（如 "pages/home.js"）还需要预加载的模块（带版本号的地址），已经在 head 里的不重复。
	pages map[string][]string
}

// preload 返回这些页面脚本需要额外预加载的模块地址。
func (m moduleMap) preload(scripts []string) []string {
	seen := map[string]bool{}
	list := []string{}
	for _, script := range scripts {
		for _, url := range m.pages[script] {
			if !seen[url] {
				seen[url] = true
				list = append(list, url)
			}
		}
	}
	return list
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

// staticImport 匹配模块开头的静态 import / export … from（可以跨行）。动态的 import() 不算：那些是用到时才加载的。
var staticImport = regexp.MustCompile(`(?m)^\s*(?:import|export)\s[^'"();]*?['"](\.{1,2}/[^'"]+\.js)['"]`)

// moduleGraph 读出每个模块静态 import 了哪些模块（键和值都是 static/js 下的相对路径）。
func moduleGraph(files fs.FS) (map[string][]string, error) {
	graph := map[string][]string{}
	err := fs.WalkDir(files, "static/js", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".min.js") {
			return err
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		module := strings.TrimPrefix(name, "static/js/")
		for _, match := range staticImport.FindAllSubmatch(body, -1) {
			graph[module] = append(graph[module], path.Join(path.Dir(module), string(match[1])))
		}
		return nil
	})
	return graph, err
}

// dependencies 返回 entry 直接和间接 import 的全部模块（不含 entry 自己），按名字排序。
func dependencies(graph map[string][]string, entry string) []string {
	seen := map[string]bool{entry: true}
	stack := []string{entry}
	for len(stack) > 0 {
		module := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, next := range graph[module] {
			if !seen[next] {
				seen[next] = true
				stack = append(stack, next)
			}
		}
	}
	delete(seen, entry)
	list := make([]string, 0, len(seen))
	for module := range seen {
		list = append(list, module)
	}
	sort.Strings(list)
	return list
}

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
	// app.js 每个页面都加载：把它整棵依赖树一次性并行下载，不必等上一层下载、解析完才发现下一层。
	// 页面脚本的依赖树按页面另外预加载（见 preload）。依赖关系从源码里读，加了新的 import 不用手动维护列表。
	graph, err := moduleGraph(web.Files)
	if err != nil {
		return moduleMap{}, err
	}
	versioned := func(module string) string { return imports["/static/js/"+module] }
	common := map[string]bool{"app.js": true}
	for _, module := range dependencies(graph, "app.js") {
		common[module] = true
		if url := versioned(module); url != "" {
			head.WriteString(`<link rel="modulepreload" href="` + template.HTMLEscapeString(url) + `">` + "\n")
		}
	}
	pages := map[string][]string{}
	for module := range graph {
		if !strings.HasPrefix(module, "pages/") {
			continue
		}
		for _, dep := range dependencies(graph, module) {
			if url := versioned(dep); url != "" && !common[dep] {
				pages[module] = append(pages[module], url)
			}
		}
	}
	head.WriteString(`<script type="speculationrules">` + speculationRules + "</script>\n")
	return moduleMap{pages: pages, head: template.HTML(head.String()), hash: scriptHash([]byte(imageGuard)) + " " + scriptHash(encoded) + " " + scriptHash([]byte(speculationRules))}, nil
}

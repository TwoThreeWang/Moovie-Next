package mail

import (
	"html/template"
	"net/url"
	"strings"
)

// 所有邮件共用的外观：站名、大标题、正文、按钮、页脚。
// 样式尽量少（邮件客户端支持有限，邮件也不宜太大），只用内联样式。

var layout = template.Must(template.New("mail").Funcs(template.FuncMap{"paras": paras, "host": host}).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"></head><body style="margin:0">
<div style="max-width:520px;margin:0 auto;padding:40px 20px;font:15px/1.7 -apple-system,'PingFang SC','Microsoft YaHei',sans-serif;color:#333">
<p><b style="font-size:17px;color:#e2605d">{{.Site.Name}}</b></p>
<h2 style="margin:28px 0 12px;font-size:22px;color:#111">{{.Title}}</h2>
{{range paras .Text}}<p>{{.}}</p>
{{end}}{{if .Button.URL}}<p style="margin:28px 0"><a href="{{.Button.URL}}" style="padding:10px 24px;border-radius:99px;background:#e2605d;color:#fff;text-decoration:none">{{.Button.Label}}</a></p>
{{end}}{{range paras .Note}}<p>{{.}}</p>
{{end}}{{if .Button.URL}}<p style="font-size:13px;color:#999;word-break:break-all">按钮点不了？复制这个链接到浏览器打开：<br>{{.Button.URL}}</p>
{{end}}<p style="margin-top:36px;padding-top:16px;border-top:1px solid #eee;font-size:12px;color:#999">这是自动发送的邮件，请不要直接回复。{{with .Site.URL}}<br><a href="{{.}}" style="color:#999">{{host .}}</a>{{end}}</p>
</div>
</body></html>
`))

// host 是页脚显示的网址：去掉 https://。
func host(s string) string {
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Host
	}
	return s
}

// paras 把空行分隔的正文拆成段落，段内换行变成 <br>。
func paras(text string) []template.HTML {
	var out []template.HTML
	for _, p := range strings.Split(strings.TrimSpace(text), "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, template.HTML(strings.ReplaceAll(template.HTMLEscapeString(p), "\n", "<br>")))
		}
	}
	return out
}

// html 是邮件的 HTML 版本。
func (m Message) html(site Site) string {
	var b strings.Builder
	if err := layout.Execute(&b, struct {
		Message
		Site Site
	}{m, site}); err != nil {
		return "" // 模板是固定的，不会出错
	}
	return b.String()
}

// plain 是邮件的纯文本版本，内容和 HTML 版本一致。
func (m Message) plain() string {
	parts := []string{m.Title, strings.TrimSpace(m.Text)}
	if m.Button.URL != "" {
		parts = append(parts, m.Button.Label+"：\n"+m.Button.URL)
	}
	var out []string
	for _, p := range append(parts, strings.TrimSpace(m.Note)) {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n") + "\n"
}

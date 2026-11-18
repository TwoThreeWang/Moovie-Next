// Package web 把模板和静态文件打包进可执行文件。
package web

import "embed"

// Files 包含 templates/ 和 static/ 两个目录。
//
//go:embed templates static
var Files embed.FS

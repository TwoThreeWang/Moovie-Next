// Package extension 是影牛的浏览器插件。插件本身在 src/ 目录，开发时可以直接在 Chrome 里"加载已解压的扩展程序"选这个目录；
// 网站把它打包成压缩包提供下载（/extension/download），打包时把开发用的站点地址换成网站的正式地址。
// 设计见 docs/extension.md，发布记录见 releases.json。
package extension

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
)

//go:embed src
var files embed.FS

//go:embed releases.json
var releasesJSON []byte

// DevOrigin 是 src/ 里写死的开发站点地址。打包时换成正式地址；直接加载 src/ 目录时连的就是本机的开发服务器。
const DevOrigin = "http://localhost:5200"

// Folder 是压缩包里的目录名。不带版本号：更新时解压到同一个位置覆盖，浏览器里点"重新加载"就行，设置和连接都还在。
const Folder = "moovie-extension"

// Release 是插件的一个版本。
type Release struct {
	Version string   `json:"version"`
	Date    string   `json:"date"`
	Notes   []string `json:"notes"`
}

// Releases 返回发布记录，最新的在前。
func Releases() []Release {
	var releases []Release
	if err := json.Unmarshal(releasesJSON, &releases); err != nil {
		panic("extension/releases.json 格式不对: " + err.Error())
	}
	return releases
}

// Version 返回插件当前的版本号（manifest.json 里的 version）。
func Version() string {
	body, err := fs.ReadFile(files, "src/manifest.json")
	if err != nil {
		panic(err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		panic("extension/src/manifest.json 格式不对: " + err.Error())
	}
	return manifest.Version
}

// textFile 判断打包时要不要替换站点地址。
func textFile(name string) bool {
	switch path.Ext(name) {
	case ".js", ".json", ".html", ".css":
		return true
	}
	return false
}

// Zip 打包插件，把开发站点地址换成 origin（形如 https://example.com，不带路径）。
// 文件时间固定为发布日期，同一个版本、同一个地址打出来的包每次都一样。
func Zip(origin string) ([]byte, error) {
	origin = strings.TrimRight(origin, "/")
	if !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://") {
		return nil, fmt.Errorf("站点地址不对: %q", origin)
	}
	modified := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if releases := Releases(); len(releases) > 0 {
		if date, err := time.Parse("2006-01-02", releases[0].Date); err == nil {
			modified = date
		}
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	err := fs.WalkDir(files, "src", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		if textFile(name) {
			body = bytes.ReplaceAll(body, []byte(DevOrigin), []byte(origin))
		}
		writer, err := archive.CreateHeader(&zip.FileHeader{Name: Folder + "/" + strings.TrimPrefix(name, "src/"), Method: zip.Deflate, Modified: modified})
		if err != nil {
			return err
		}
		_, err = writer.Write(body)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

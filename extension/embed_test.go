package extension

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"strings"
	"testing"
)

func TestReleaseMatchesManifest(t *testing.T) {
	releases := Releases()
	if len(releases) == 0 || releases[0].Version != Version() {
		t.Fatalf("releases.json 最新的版本要和 manifest.json 一致: %v vs %s", releases, Version())
	}
	if releases[0].Date == "" || len(releases[0].Notes) == 0 {
		t.Error("每个版本要写日期和更新内容，下载页和插件的更新提醒会显示")
	}
}

// 清单里提到的文件都要存在，否则 Chrome 加载插件时直接报错。
func TestManifestFilesExist(t *testing.T) {
	body, _ := fs.ReadFile(files, "src/manifest.json")
	var manifest struct {
		Icons      map[string]string `json:"icons"`
		Background struct {
			ServiceWorker string `json:"service_worker"`
		} `json:"background"`
		Action struct {
			Popup string `json:"default_popup"`
		} `json:"action"`
		Options        string `json:"options_page"`
		ContentScripts []struct {
			JS  []string `json:"js"`
			CSS []string `json:"css"`
		} `json:"content_scripts"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	paths := []string{manifest.Background.ServiceWorker, manifest.Action.Popup, manifest.Options}
	for _, icon := range manifest.Icons {
		paths = append(paths, icon)
	}
	for _, script := range manifest.ContentScripts {
		paths = append(paths, script.JS...)
		paths = append(paths, script.CSS...)
	}
	for _, path := range paths {
		if _, err := fs.Stat(files, "src/"+path); err != nil {
			t.Errorf("manifest.json 提到的 %s 不存在", path)
		}
	}
}

func TestZipReplacesOrigin(t *testing.T) {
	first, err := Zip("https://moovie.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Zip("https://moovie.example.com")
	if !bytes.Equal(first, second) {
		t.Error("同一个地址打出来的包应该一模一样")
	}
	archive, err := zip.NewReader(bytes.NewReader(first), int64(len(first)))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, file := range archive.File {
		if !strings.HasPrefix(file.Name, Folder+"/") {
			t.Errorf("文件要放在 %s/ 目录里: %s", Folder, file.Name)
		}
		seen[strings.TrimPrefix(file.Name, Folder+"/")] = true
		reader, _ := file.Open()
		body, _ := io.ReadAll(reader)
		if strings.Contains(string(body), DevOrigin) {
			t.Errorf("%s 里还有开发地址", file.Name)
		}
	}
	for _, name := range []string{"manifest.json", "config.js", "background.js", "icons/icon-128.png"} {
		if !seen[name] {
			t.Errorf("压缩包里缺 %s", name)
		}
	}
	if _, err := Zip("moovie.example.com"); err == nil {
		t.Error("不是 http(s) 地址应该报错")
	}
}

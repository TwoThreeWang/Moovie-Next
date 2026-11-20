package imageproxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// imageRef 使用数据库里的片名和素材用途，不使用访客提供的文件名。
// 原始地址与元数据一起签名；相同作品、素材和规格在各个页面得到相同链接。
type imageRef struct {
	URL     string `json:"u"`
	Title   string `json:"t"`
	Purpose string `json:"p"`
}

func (h *Handler) signature(encoded, size string) []byte {
	mac := hmac.New(sha256.New, h.key)
	mac.Write([]byte("moovie-image-v1\n" + encoded + "\n" + size))
	return mac.Sum(nil)
}

// URL 生成稳定的签名地址。展示图不依赖登录、IP 或短期令牌，便于搜索抓取和 CDN 缓存。
func (h *Handler) URL(raw, size, title, purpose string) string {
	if raw == "" || len(h.key) == 0 || !validSize(size) || !validPurpose(purpose) {
		return ""
	}
	if !Allowed(raw) {
		// 其他 HTTPS 图片保持直连，不把代理开放给任意域名。
		parsed, err := url.Parse(raw)
		if err == nil && parsed.Scheme == "https" && parsed.User == nil {
			return raw
		}
		return ""
	}
	ref := imageRef{URL: raw, Title: cleanTitle(title), Purpose: purpose}
	data, _ := json.Marshal(ref)
	encoded := base64.RawURLEncoding.EncodeToString(data)
	if len(encoded) > 8192 {
		return ""
	}
	query := url.Values{"size": {size}, "sig": {base64.RawURLEncoding.EncodeToString(h.signature(encoded, size))}}
	return "/img/" + encoded + "?" + query.Encode()
}

func (h *Handler) verify(encoded string, query url.Values) (imageRef, string, error) {
	var ref imageRef
	size := query.Get("size")
	if len(h.key) == 0 || len(encoded) > 8192 || len(query) != 2 || len(query["size"]) != 1 || len(query["sig"]) != 1 || !validSize(size) {
		return ref, "", errors.New("invalid image parameters")
	}
	signature, err := base64.RawURLEncoding.DecodeString(query.Get("sig"))
	if err != nil || !hmac.Equal(signature, h.signature(encoded, size)) {
		return ref, "", errors.New("invalid image signature")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || json.Unmarshal(data, &ref) != nil || !Allowed(ref.URL) || !validPurpose(ref.Purpose) || ref.Title != cleanTitle(ref.Title) {
		return ref, "", errors.New("invalid image reference")
	}
	return ref, size, nil
}

func validSize(size string) bool       { return size == "thumb" || size == "display" || size == "original" }
func validPurpose(purpose string) bool { return purpose == "poster" || purpose == "still" }

func cleanTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\:"<>|?*`, r) {
			return '_'
		}
		return r
	}, title)
	title = strings.Trim(strings.Join(strings.Fields(title), " "), " ._")
	runes := []rune(title)
	if len(runes) > 80 {
		title = string(runes[:80])
	}
	// 留出用途、规格和扩展名的空间，同时不截断 UTF-8 字符。
	for len(title) > 192 {
		_, n := utf8.DecodeLastRuneInString(title)
		title = title[:len(title)-n]
	}
	title = strings.Trim(title, " ._")
	if title == "" {
		return "未命名作品"
	}
	return title
}

func (r imageRef) filename(size, contentType, key string) string {
	purpose := map[string]string{"poster": "海报", "still": "剧照"}[r.Purpose]
	label := map[string]string{"thumb": "缩略图", "display": "展示图", "original": "原图"}[size]
	return r.Title + "_" + purpose + "_" + label + "_" + hex.EncodeToString(hashURL(key))[:8] + extension(contentType)
}

var (
	doubanSize  = regexp.MustCompile(`/(s|m|l)_ratio_poster/|/(s|m|l)/public/`)
	tmdbSize    = regexp.MustCompile(`/t/p/[a-z0-9]+/`)
	hongguoSize = regexp.MustCompile(`tplv-shrink:[0-9]+:[0-9]+`)
)

// source 优先选用上游尺寸，避免本机解码；没有尺寸接口时返回本机缩图的最大边长。
// original 表示来源提供的版本，不把小图放大冒充原图。
func (r imageRef) source(size string) (target string, resize int) {
	parsed, _ := url.Parse(r.URL)
	host := strings.ToLower(parsed.Hostname())
	target = r.URL
	switch {
	case strings.HasSuffix(host, ".doubanio.com") && doubanSize.MatchString(parsed.Path):
		letter := "l"
		if size == "thumb" {
			letter = "m"
		}
		parsed.Path = doubanSize.ReplaceAllStringFunc(parsed.Path, func(match string) string {
			if strings.Contains(match, "ratio_poster") {
				return "/" + letter + "_ratio_poster/"
			}
			return "/" + letter + "/public/"
		})
		return parsed.String(), 0
	case host == "image.tmdb.org" && tmdbSize.MatchString(parsed.Path):
		width := "original"
		if size != "original" {
			if r.Purpose == "still" {
				width = map[string]string{"thumb": "w780", "display": "w1280"}[size]
			} else {
				width = map[string]string{"thumb": "w342", "display": "w780"}[size]
			}
		}
		parsed.Path = tmdbSize.ReplaceAllString(parsed.Path, "/t/p/"+width+"/")
		return parsed.String(), 0
	case hongguoImageHost.MatchString(host) && hongguoSize.MatchString(parsed.Path):
		if size == "thumb" {
			parsed.Path = hongguoSize.ReplaceAllString(parsed.Path, "tplv-shrink:320:0")
			return parsed.String(), 0
		}
		return target, 0
	}
	return target, resizeEdge(size)
}

func resizeEdge(size string) int {
	switch size {
	case "thumb":
		return 480
	case "display":
		return 1280
	}
	return 0
}

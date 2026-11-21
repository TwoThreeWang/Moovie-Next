package imageproxy

import (
	"bytes"
	"errors"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// readImage 读取完整图片，最多 limit 字节（不超过 maxImageBytes）。只在当前请求中使用，不做内容缓存。
func readImage(body io.Reader, limit int64) ([]byte, error) {
	limit = min(limit, maxImageBytes)
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || int64(len(data)) > limit {
		return nil, errors.New("image size out of range")
	}
	return data, nil
}

// jpegQuality 是本机缩图后的 JPEG 质量。手机用的两种图压得更狠：卡片在屏幕上只有一百多像素宽，
// 大海报上面还叠着渐变和文字，看不出区别。实测 12 张豆瓣海报，card（约 240 宽、质量 60）合计是原来 thumb 的 19%；
// 3 张详情页大海报，hero（约 720 宽、质量 65）是原来 display 的 38%。
func jpegQuality(size string) int {
	switch size {
	case "card":
		return 60
	case "hero":
		return 65
	}
	return 82
}

// resizeImage 把图片缩到最长边 edge 以内，JPEG 质量 82。
func resizeImage(data []byte, contentType string, edge int, reserve func(int64) error) ([]byte, string, error) {
	return resizeImageQuality(data, contentType, edge, 82, reserve)
}

// resizeImageQuality 把图片缩到最长边 edge 以内。图片本来就不大于 edge，或者格式本机解不了时，原样返回。
// reserve 在解码前为位图预留内存。
func resizeImageQuality(data []byte, contentType string, edge, quality int, reserve func(int64) error) ([]byte, string, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if errors.Is(err, image.ErrFormat) {
		return data, contentType, nil
	}
	if err != nil {
		return nil, "", err
	}
	// 压缩文件很小也可能展开成巨大的位图，必须先检查尺寸再解码。
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 12_000_000 {
		return nil, "", errors.New("image dimensions out of range")
	}
	w, h := config.Width, config.Height
	largest := max(w, h)
	if largest <= edge {
		return data, contentType, nil
	}
	w, h = max(1, w*edge/largest), max(1, h*edge/largest)
	// 解码后的位图按每像素 4 字节估算，加上缩小后的目标位图。
	if err := reserve(int64(config.Width)*int64(config.Height)*4 + int64(w)*int64(h)*4); err != nil {
		return nil, "", err
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	dest := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dest, dest.Bounds(), source, source.Bounds(), draw.Src, nil)
	var out bytes.Buffer
	if !dest.Opaque() {
		err = png.Encode(&out, dest)
		return out.Bytes(), "image/png", err
	}
	err = jpeg.Encode(&out, dest, &jpeg.Options{Quality: quality})
	return out.Bytes(), "image/jpeg", err
}

package imageproxy

import "testing"

// 图片代理只能访问豆瓣的图片域名，不能被当成任意地址的代理。
func TestAllowed(t *testing.T) {
	cases := map[string]bool{
		"https://img9.doubanio.com/view/photo/l/public/p1.jpg":             true,
		"https://IMG1.DOUBANIO.COM/a.jpg":                                  true,
		"http://img9.doubanio.com/a.jpg":                                   false, // 不是 HTTPS
		"https://doubanio.com.evil.example/a.jpg":                          false,
		"https://evil.example/?x=.doubanio.com":                            false,
		"https://user:pass@img9.doubanio.com/a.jpg":                        false,
		"https://img9.doubanio.com:8443/a.jpg":                             false,
		"https://127.0.0.1/a.jpg":                                          false,
		"https://source.example/api.php/provide/vod/":                      false,
		"https://image.tmdb.org/t/p/w342/a.jpg":                            true,
		"https://p6-novel.byteimg.com/novel-pic/a~tplv-shrink:640:0.image": true,
		"https://p6-novel.byteimg.com.evil.example/a.image":                false,
		"https://evil.byteimg.com/a.image":                                 false,
		"https://image.tmdb.org.evil.example/a.jpg":                        false,
		"": false,
	}
	for input, want := range cases {
		if got := Allowed(input); got != want {
			t.Errorf("Allowed(%q) = %v，期望 %v", input, got, want)
		}
	}
}

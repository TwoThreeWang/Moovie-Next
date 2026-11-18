package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestAllowed(t *testing.T) {
	for address, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true, "198.18.6.150": true, // 最后一个是代理软件 fake-ip 的假地址
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.18": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fd00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false, "224.0.0.1": false,
	} {
		if got := allowed(netip.MustParseAddr(address)); got != want {
			t.Errorf("%s: 应该是 %v，实际 %v", address, want, got)
		}
	}
}

func TestFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api.php/provide/vod/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8") // 很多资源站把 JSON 标成 text/html
			w.Write([]byte(`{"list":[{"vod_name":"雾港"}]}`))
		case "/video.m3u8":
			w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n"))
		case "/poster.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("jpeg"))
		case "/big":
			w.Write([]byte(strings.Repeat("x", maxBody+10)))
		case "/jump":
			http.Redirect(w, r, "/api.php/provide/vod/", http.StatusFound)
		}
	}))
	defer server.Close()
	ctx := context.Background()

	open := newRelay(true)
	status, contentType, body, err := open.fetch(ctx, server.URL+"/jump")
	if err != nil || status != 200 || !strings.Contains(string(body), "雾港") || !strings.Contains(contentType, "text/html") {
		t.Fatalf("应该取到查询结果（跟随跳转）: %d %q %v", status, body, err)
	}
	for path, want := range map[string]error{"/video.m3u8": errNotText, "/poster.jpg": errNotText, "/big": errTooLarge} {
		if _, _, _, err := open.fetch(ctx, server.URL+path); !errors.Is(err, want) {
			t.Errorf("%s 应该被拒绝（%v），实际 %v", path, want, err)
		}
	}

	// 正式的转发器不让访问本机：测试服务器就在 127.0.0.1 上。
	if _, _, _, err := New().fetch(ctx, server.URL+"/api.php/provide/vod/"); !errors.Is(err, ErrBlocked) {
		t.Errorf("本机地址应该被拦下，实际 %v", err)
	}
}

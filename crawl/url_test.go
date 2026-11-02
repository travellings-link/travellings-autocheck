package crawl

import "testing"

// 线上有历史遗留的不规范记录（id=855 blog.joker2yue.com 只有裸域名）。
// 必须补全 scheme，否则抓取和渲染都会失败，好站被误判。
func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"blog.joker2yue.com":     "https://blog.joker2yue.com",
		"https://a.example":      "https://a.example",
		"http://b.example":       "http://b.example",
		"//c.example":            "https://c.example",
		"  blog.joker2yue.com  ": "https://blog.joker2yue.com",
		"ftp://d.example":        "ftp://d.example", // 保持原样，下游判非法
		"":                       "",
	}
	for in, want := range cases {
		if got := normalizeURL(in); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// Raw 对裸域名必须能真正发起请求，而不是立即返回 invalid url scheme。
func TestRawAcceptsBareDomain(t *testing.T) {
	// 用一个必然失败但不合法的 scheme 反证：补全后应当进入网络层
	p := Raw(nil, "ftp://x.example", 0)
	if p.FetchErr != "invalid url scheme" {
		t.Fatalf("ftp 应判非法 scheme，实际 %q", p.FetchErr)
	}
}

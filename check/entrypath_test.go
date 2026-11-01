package check

import "testing"

// 成员站挂的入口带不带 .html 都算合法：
// 实测 https://www.travellings.cn/go.html 返回 307 跳转、/go 直接 200，两种都可用。
func TestEntryPathWithAndWithoutExt(t *testing.T) {
	paths := []string{"go", "plain", "coder-1024", "go-by-clouds", "go-robots",
		"train", "train-star", "typewriter", "go-start", "arsvine"}
	for _, p := range paths {
		for _, form := range []string{"/" + p, "/" + p + ".html", "/" + p + "/"} {
			u := "https://www.travellings.cn" + form
			if _, ok := matchURL(u); !ok {
				t.Errorf("应命中入口：%s", u)
			}
		}
	}
}

// 非白名单路径仍必须拒绝，避免归一化把匹配放宽过头。
func TestEntryPathRejectsUnknown(t *testing.T) {
	bad := []string{
		"https://www.travellings.cn/blog/go.html",
		"https://www.travellings.cn/go.html.evil",
		"https://travellings.cn/",
		"https://travellings.cn.evil.com/go.html",
	}
	for _, u := range bad {
		if _, ok := matchURL(u); ok {
			t.Errorf("不应命中：%s", u)
		}
	}
}

// 归一化本身的行为
func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/go.html": "/go",
		"/go":      "/go",
		"/go/":     "/go",
		"":         "/",
		"/":        "/",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

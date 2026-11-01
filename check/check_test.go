package check

import (
	"strings"
	"testing"
)

func TestMatchURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://www.travellings.cn/go.html", true},
		{"https://travellings.cn/go.html", true},
		{"https://www.travellings.cn/plain.html", true},
		{"https://www.travellings.cn/coder-1024.html", true},
		{"https://www.travellings.cn/go-by-clouds.html", true},
		{"http://www.travellings.cn/go.html", true},
		{"https://travellings.link", true},
		{"https://travellings.link/anything", true},
		{"https://github.com/travellings-link/travellings", true},
		{"https://github.com/Travellings-Link/Travellings/", true},
		{"https://github.com/travellings-link/travellings.git", true},
		// 拒绝面
		{"https://travellings.cn/", false},                 // 无入口路径
		{"https://travellings.cn/blog/go.html", false},     // 路径不对
		{"https://travellings.cn.evil.com/go.html", false}, // 后缀欺骗
		{"https://evil.com/?x=https://travellings.cn/go.html", false},
		{"https://evil.com/travellings.link", false},
		{"ftp://travellings.cn/go.html", false},
		{"", false},
	}
	for _, c := range cases {
		if _, got := matchURL(c.in); got != c.want {
			t.Errorf("matchURL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRedirectParam(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://blog.example.com/go.php?url=https%3A%2F%2Fwww.travellings.cn%2Fgo.html", true},
		{"https://blog.example.com/redirect?target=https://travellings.cn/plain.html", true},
		{"https://blog.example.com/jump.php?to=%2F%2Fwww.travellings.cn%2Fgo.html", true}, // 协议相对
		{"https://blog.example.com/go?next=https://example.com", false},
	}
	for _, c := range cases {
		if _, got := matchRedirect(c.in); got != c.want {
			t.Errorf("matchRedirect(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestExtractLinksAbsolutizes(t *testing.T) {
	base := "https://www.travellings.cn/"
	html := `<a href="/go.html">x</a><a href='//www.travellings.cn/plain.html'>y</a>
<a href="https://khbit.cn/">z</a><a href="#top">s</a>`
	got := ExtractLinks(base, html)
	want := map[string]bool{
		"https://www.travellings.cn/go.html":    false,
		"https://www.travellings.cn/plain.html": false,
		"https://khbit.cn/":                     false,
	}
	for _, g := range got {
		if _, ok := want[g]; ok {
			want[g] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("ExtractLinks missing %q, got %v", k, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("ExtractLinks count = %d, want 3 (%v)", len(got), got)
	}
}

// wayjam.me 实例（2026-10-31 真实对照抓到）：href=xxx 无引号，HTML5 合法，
// 带引号的正则提不到 → 旧规则靠整页子串歪打正着，结构匹配必须兼容。
func TestUnquotedHref(t *testing.T) {
	html := `<a class=x href=https://www.travellings.cn/go.html title=开往>go</a>`
	p := Page{Status: 200, HTML: html, Links: ExtractLinks("https://wayjam.me/", html)}
	if _, ok := MatchTravellings(p); !ok {
		t.Fatalf("unquoted href must match, links=%v", p.Links)
	}
}

// 官网自测案例（2026-10-31 实测：旧子串规则把官网自己判成 LOST）：
// travellings.cn 页面上的相对 "/go.html" 绝对化后指向开往入口，必须命中。
func TestRelativeHrefRegression(t *testing.T) {
	base := "https://www.travellings.cn/"
	html := `<nav><a href="/go.html">开往</a></nav><footer>©</footer>`
	p := Page{Status: 200, HTML: html, Links: ExtractLinks(base, html)}
	ev, ok := MatchTravellings(p)
	if !ok {
		t.Fatalf("relative /go.html on travellings.cn must match, links=%v", p.Links)
	}
	if !strings.Contains(ev, "www.travellings.cn/go.html") {
		t.Errorf("evidence = %q, want absolutized travellings.cn URL", ev)
	}
}

// 反向守卫：第三方博客上的 "/go.html" 绝对化后指向博客自己，不是开往入口。
func TestRelativeHrefOwnDomainNotMatch(t *testing.T) {
	html := `<a href="/go.html">x</a>`
	p := Page{Status: 200, HTML: html, Links: ExtractLinks("https://blog.example.org/", html)}
	if _, ok := MatchTravellings(p); ok {
		t.Errorf("blog-own relative /go.html must not match: %v", p.Links)
	}
}

func TestSpecialCaseSignatures(t *testing.T) {
	for _, s := range []string{
		`<script>anzhiyu.totraveling()</script>`,
		`onclick="totraveling()"`,
		`<a href="https%3A%2F%2Fwww.travellings.cn%2Fplain.html">go</a>`,
	} {
		p := Page{Status: 200, HTML: s}
		if _, ok := MatchTravellings(p); !ok {
			t.Errorf("signature miss: %s", s)
		}
	}
	// 关键词不再是判据：裸“开往”文本必须拒绝
	p := Page{Status: 200, HTML: "<p>我很喜欢 开往 travelling 这个活动</p>"}
	if _, ok := MatchTravellings(p); ok {
		t.Error("keyword-only page must NOT match")
	}
}

func TestEvaluateRouting(t *testing.T) {
	hit := Page{Status: 200, Links: []string{"https://www.travellings.cn/go.html"}}
	if r := Evaluate(hit, Raw); r.Verdict != Run || r.Escalate {
		t.Errorf("hit/raw = %+v", r)
	}
	miss := Page{Status: 200, HTML: "<p>empty blog</p>"}
	r := Evaluate(miss, Raw)
	if r.Verdict != Lost || !r.Escalate {
		t.Errorf("miss/raw must escalate, got %+v", r)
	}
	r = Evaluate(miss, JS)
	if r.Verdict != Lost || r.Escalate {
		t.Errorf("miss/js must terminate, got %+v", r) // 防环
	}
	// 526 等非 2xx 一律终判，不升级：
	// 状态码是服务端事实，浏览器拿到同一个响应，再渲染一次纯属白烧预算。
	// （2026-10-31 修订：原断言为"应升级"，实测证明渲染改变不了结论。）
	code := Page{Status: 526}
	if r = Evaluate(code, Raw); r.Escalate {
		t.Errorf("526/raw must not escalate, got %+v", r)
	}
	if r.Verdict != Verdict("526") {
		t.Errorf("526 verdict = %+v, want 526", r)
	}
	if r = Evaluate(code, JS); r.Verdict != Verdict("526") {
		t.Errorf("526/js = %+v, want verdict 526", r)
	}
	dns := Page{Status: 0, FetchErr: "dial: no such host (dns)"}
	if r = Evaluate(dns, Raw); r.Escalate || r.Verdict != Error {
		t.Errorf("DNS-dead must not waste browser: %+v", r)
	}
	// 超时不再升级：抓取阶段已用满超时预算，浏览器面对同一张网只会重演一遍等待。
	// （2026-10-31 修订：原断言为"应升级"，实测 15s+30s=45s 花在一个站上且结论相同。）
	to := Page{Status: 0, FetchErr: "context deadline exceeded: timeout"}
	if r = Evaluate(to, Raw); r.Escalate {
		t.Errorf("timeout/raw must not escalate, got %+v", r)
	}
	if r.Verdict != Timeout {
		t.Errorf("timeout/raw verdict = %v, want TIMEOUT", r.Verdict)
	}
}

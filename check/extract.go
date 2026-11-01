package check

import (
	"net/url"
	"regexp"
	"strings"
)

var hrefRe = regexp.MustCompile(`(?i)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>]+))`)

// ExtractLinks 从 HTML 原文提 href 并按 base 绝对化。
// ponytail: 正则级提取，不处理 HTML 实体与注释内假 href；渲染阶段以 a.href 集合为准，
// 此函数只是抓取阶段的近似。需要树级精确时换 golang.org/x/net/html，接口不变。
func ExtractLinks(base string, html string) []string {
	bu, err := url.Parse(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range hrefRe.FindAllStringSubmatch(html, -1) {
		sel := m[1]
		if sel == "" {
			sel = m[2]
		}
		if sel == "" {
			sel = m[3]
		}
		raw := htmlUnescapeMinimal(sel)
		if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "javascript:") {
			continue
		}
		ru, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if ru.Scheme == "" && strings.HasPrefix(raw, "//") {
			ru.Scheme = bu.Scheme
		}
		out = append(out, bu.ResolveReference(ru).String())
	}
	return out
}

func htmlUnescapeMinimal(s string) string {
	r := strings.NewReplacer("&amp;", "&", "&#38;", "&", "&quot;", "\"", "&#34;", "\"")
	return r.Replace(s)
}

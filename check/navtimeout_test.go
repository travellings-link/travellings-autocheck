package check

import "testing"

// 渲染阶段必须能区分「站点侧失败」与「我们的工具链故障」。
// 归类错了会让运维查错方向：站慢说成 CDP 坏了，白查一场。
func TestIsSiteNavTimeout(t *testing.T) {
	// 站点侧：浏览器跑起来了，但访问这个站失败
	site := []string{
		`navigate: Network error: navigation exceeded 30000ms deadline (-32000)`,
		`navigate: Network error: https://x/: error sending request: operation timed out (-32000)`,
		// 实测：DNS 解析失败曾被误标成工具链故障
		`navigate: Network error: https://iktao.cn/: client error (Connect): dns error: 不知道这样的主机。 (os error 11001) (-32000)`,
		`navigate: Network error: https://x/: client error (Connect): [CERTIFICATE_VERIFY_FAILED] (-32000)`,
	}
	for _, e := range site {
		if !IsSiteNavTimeout(e) {
			t.Errorf("应判为站点侧失败：%s", e)
		}
	}

	// 工具链侧：CDP 连不上、浏览器崩了、target 丢了
	infra := []string{
		`attach cdp ws://127.0.0.1:9222/devtools/browser: connection refused`,
		`executor doesn\u0027t exist`,
		`target closed`,
		`websocket: close 1006`,
	}
	for _, e := range infra {
		if IsSiteNavTimeout(e) {
			t.Errorf("应判为工具链故障，不是站点侧：%s", e)
		}
	}
}

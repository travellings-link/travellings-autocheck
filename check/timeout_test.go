package check

import (
	"testing"
)

// 超时判定必须覆盖各家措辞。曾因只匹配 "timeout" 而漏判——
// chromedp 报的是 "operation timed out"（带空格），结果真超时被标成 ERROR。
func TestIsTimeoutErrCoversVariants(t *testing.T) {
	yes := []string{
		`navigate: Network error: operation timed out (-32000)`,
		`navigation exceeded 30000ms deadline`,
		`context deadline exceeded`,
		`Get "https://x": dial tcp: i/o timeout`,
		`net/http: request canceled (Client.Timeout exceeded while awaiting headers)`,
	}
	for _, e := range yes {
		if !isTimeoutErr(e) {
			t.Errorf("应判为超时：%s", e)
		}
	}
	no := []string{
		`x509: certificate has expired`,
		`no such host`,
		`connection refused`,
	}
	for _, e := range no {
		if isTimeoutErr(e) {
			t.Errorf("不应判为超时：%s", e)
		}
	}
}

// js 阶段的工具链超时必须产出 TIMEOUT，且 Evidence 保留完整原文。
func TestEvaluateTimeoutAtJS(t *testing.T) {
	msg := `navigate: Network error: Network error: https://x/: error sending request: operation timed out (-32000)`
	res := Evaluate(Page{FetchErr: msg}, JS)
	if res.Verdict != Timeout {
		t.Fatalf("应判 TIMEOUT，实际 %v", res.Verdict)
	}
	if res.Evidence != msg {
		t.Errorf("Evidence 必须保留完整原文，实际 %q", res.Evidence)
	}
}

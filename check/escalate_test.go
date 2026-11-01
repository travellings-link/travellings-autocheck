package check

import (
	"strings"
	"testing"
)

// TLS 证书错误 100% 可复现，浏览器也救不了：
// 必须在 raw 阶段直接终判，不能升级到渲染（白烧 C 腿预算）。
// 真实案例：blog.oalo.cn 证书过期，实测 64ms 即报错。
func TestTLSErrorDoesNotEscalate(t *testing.T) {
	cases := []string{
		`ErrTransport: Get "https://blog.oalo.cn": tls: failed to verify certificate: x509: certificate has expired or is not yet valid`,
		`tls: failed to verify certificate: x509: certificate signed by unknown authority`,
		`tls: handshake failure`,
	}
	for _, e := range cases {
		p := Page{FetchErr: e} // Status=0
		res := Evaluate(p, Raw)
		if res.Escalate {
			t.Errorf("TLS 错误不应升级，实际 Escalate=true，err=%s", e)
		}
		if res.Verdict != Error {
			t.Errorf("应判 ERROR，实际 %v", res.Verdict)
		}
	}
}

// DNS 不存在、连接被拒同样属确定性失败，不升级。
func TestTerminalTransportDoesNotEscalate(t *testing.T) {
	for _, e := range []string{
		"dial tcp: lookup nope.example: no such host",
		"dial tcp 127.0.0.1:1: connect: connection refused",
	} {
		if Evaluate(Page{FetchErr: e}, Raw).Escalate {
			t.Errorf("确定性失败不应升级：%s", e)
		}
	}
}

// 超时在抓取阶段就终判 TIMEOUT，不再进渲染。
// 理由：抓取已用满超时预算，浏览器面对同一张网只会重演一遍等待，
// 且此前会被误标成 "infra js-leg"（工具链故障），帽子扣错了。
func TestTimeoutTerminatesAtRaw(t *testing.T) {
	for _, e := range []string{
		"timeout: context deadline exceeded",
		"navigate: operation timed out (-32000)",
		"navigation exceeded 30000ms deadline",
	} {
		res := Evaluate(Page{FetchErr: e}, Raw)
		if res.Escalate {
			t.Errorf("超时应终判不升级：%s", e)
		}
		if res.Verdict != Timeout {
			t.Errorf("超时应判 TIMEOUT，实际 %v：%s", res.Verdict, e)
		}
	}
}

// EOF / 连接重置属**可能一过性**的失败，仍应升级——浏览器重试有机会成功。
func TestTransientErrorStillEscalates(t *testing.T) {
	for _, e := range []string{
		"EOF",
		"read tcp: connection reset by peer",
	} {
		if !Evaluate(Page{FetchErr: e}, Raw).Escalate {
			t.Errorf("一过性失败应升级重试：%s", e)
		}
	}
}

// 526 是 Cloudflare 的 SSL 证书无效，与一过性 5xx 不同，直接终判。
func TestTerminalStatus526(t *testing.T) {
	res := Evaluate(Page{Status: 526}, Raw)
	if res.Escalate {
		t.Error("526 不应升级")
	}
	if string(res.Verdict) != "526" {
		t.Errorf("应保留状态码 526，实际 %v", res.Verdict)
	}
}

// 所有非 2xx 都终判：状态码是服务端事实，渲染拿到同一响应，升级无意义。
func TestServerErrorDoesNotEscalate(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504, 526, 404, 403} {
		res := Evaluate(Page{Status: code}, Raw)
		if res.Escalate {
			t.Errorf("HTTP %d 不应升级到渲染", code)
		}
		if string(res.Verdict) != itoa(code) {
			t.Errorf("HTTP %d 应保留状态码，实际 %v", code, res.Verdict)
		}
	}
}

// terminalErr 的语义边界
func TestTerminalErrClassification(t *testing.T) {
	yes := []string{"x509: certificate expired", "tls: handshake failure", "no such host"}
	no := []string{"timeout", "EOF", "i/o timeout"}
	for _, s := range yes {
		if !terminalErr(s) {
			t.Errorf("应判为确定性失败：%s", s)
		}
	}
	for _, s := range no {
		if terminalErr(s) {
			t.Errorf("不应判为确定性失败：%s", s)
		}
	}
	_ = strings.ToLower("")
}

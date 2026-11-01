package pipeline

import (
	"testing"

	"travellings-bot-go/check"
)

// 工具链超时必须判成 TIMEOUT，不能一律 ERROR——
// 判定语汇要和 match.go 的 Evaluate 保持一致，
// 否则同一类失败在 raw 腿是 TIMEOUT、在 js 腿变 ERROR，口径就乱了。
func TestInfraTimeoutYieldsTimeoutVerdict(t *testing.T) {
	in := Incoming{
		Site:     Site{ID: 15, URL: "https://wznmickey.com"},
		InfraErr: "navigate: navigation exceeded 30000ms deadline",
	}
	v := evalRendered(in)
	if v.Status != check.Timeout {
		t.Fatalf("超时应判 TIMEOUT，实际 %v", v.Status)
	}
	if v.InfraErr == "" {
		t.Fatal("必须带 InfraErr 标记，写库环节据此跳过")
	}
}

// 非超时的工具链故障（如 CDP 挂掉）判 ERROR。
func TestInfraNonTimeoutYieldsError(t *testing.T) {
	in := Incoming{
		Site:     Site{ID: 1, URL: "https://x.example"},
		InfraErr: "attach cdp ws://127.0.0.1:9222: connection refused",
	}
	v := evalRendered(in)
	if v.Status != check.Error {
		t.Fatalf("非超时故障应判 ERROR，实际 %v", v.Status)
	}
}

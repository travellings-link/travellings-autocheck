package pipeline

import (
	"context"
	"testing"
	"time"

	"travellings-bot-go/check"
)

// 三种 stage 模式都必须能正常收口、不漏站。
// 之前用 inFlight 计数收口时，raw 模式会死锁（渲染池根本没启动，计数恒为 0）。
func TestRunAllStagesTerminate(t *testing.T) {
	sites := []Site{
		{ID: 1, URL: "https://example.com"},
		{ID: 2, URL: "https://example.org"},
	}
	for _, stage := range []string{"raw", "both", "js"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			opts := Options{
				HTTPWorkers: 2, BrowserWorkers: 2, FetchQueue: 4,
				TimeoutMS: 8000, JSSettleMS: 3000, Stage: stage,
				CDPURL: "ws://127.0.0.1:9222/devtools/browser",
			}
			vs := Run(ctx, sites, opts)
			if len(vs) != len(sites) {
				t.Fatalf("stage=%s 应产出 %d 条判定，实际 %d（有站点被丢弃）", stage, len(sites), len(vs))
			}
		})
	}
}

// withNextAttempt 必须递增尝试次数，并丢弃大 body。
func TestWithNextAttempt(t *testing.T) {
	in := Incoming{Site: Site{ID: 7, URL: "https://x.example"}, Attempt: 2,
		Page: check.Page{HTML: "BIG"}}
	out := in.withNextAttempt()
	if out.Attempt != 3 {
		t.Fatalf("Attempt 应为 3，实际 %d", out.Attempt)
	}
	if out.Page.HTML != "" {
		t.Error("重试不应再带大 body")
	}
	if out.Page.FinalURL != "https://x.example" {
		t.Errorf("应保留 URL，实际 %q", out.Page.FinalURL)
	}
}

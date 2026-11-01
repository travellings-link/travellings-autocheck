package pipeline

import (
	"context"
	"testing"
	"time"
)

// 进度计数的分子绝不能超过分母。
// 重试会让同一站点被渲染多次，若按"渲染次数"计数就会溢出（实测出现 渲染 11/10）。
func TestRenderCountNeverExceedsEscalate(t *testing.T) {
	sites := []Site{
		{ID: 1, URL: "https://example.com"},
		{ID: 2, URL: "https://example.org"},
	}
	stats := &StageStats{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opts := Options{
		HTTPWorkers: 2, BrowserWorkers: 2, FetchQueue: 4,
		TimeoutMS: 8000, JSSettleMS: 3000, Stage: "both", RenderRetries: 2,
		CDPURL: "ws://127.0.0.1:9222/devtools/browser", Stats: stats,
	}
	Run(ctx, sites, opts)

	fetched, judged, rendered, escalate, _ := stats.Snapshot()
	t.Logf("fetched=%d judged=%d rendered=%d escalate=%d", fetched, judged, rendered, escalate)
	if rendered > escalate && escalate > 0 {
		t.Errorf("渲染分子(%d) 超过分母(%d)：重试被重复计数了", rendered, escalate)
	}
	if judged > len(sites) {
		t.Errorf("判定计数(%d) 超过站点数(%d)", judged, len(sites))
	}
}

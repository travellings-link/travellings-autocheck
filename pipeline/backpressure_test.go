package pipeline

import (
	"context"
	"testing"
	"time"

	"travellings-bot-go/check"
)

// 核心诉求：等渲染的时候，抓取必须继续推进。
// 若 Judge 阻塞在投递上，fetchCh 会迅速填满，Fetcher 全部停摆，
// 表现为"抓取数长时间卡住不动"。
func TestFetchContinuesWhileRendering(t *testing.T) {
	sites := make([]Site, 0, 12)
	for i := 0; i < 12; i++ {
		sites = append(sites, Site{ID: i + 1, URL: "https://example.com"})
	}
	stats := &StageStats{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	opts := Options{
		HTTPWorkers: 8, BrowserWorkers: 2, FetchQueue: 4,
		TimeoutMS: 6000, JSSettleMS: 2000, Stage: "both", RenderRetries: 1,
		CDPURL: "ws://127.0.0.1:9222/devtools/browser", Stats: stats,
	}

	// 采样：抓取计数应当持续推进，而不是长时间卡住
	done := make(chan []Verdict, 1)
	go func() { done <- Run(ctx, sites, opts) }()

	samples := []int{}
	deadline := time.After(70 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case v := <-done:
			if len(v) != len(sites) {
				t.Fatalf("应产出 %d 条判定，实际 %d", len(sites), len(v))
			}
			fetched, _, _, _, _ := stats.Snapshot()
			if fetched < len(sites) {
				t.Errorf("抓取未完成：%d/%d", fetched, len(sites))
			}
			break loop
		case <-tick.C:
			ft, _, _, _, _ := stats.Snapshot()
			samples = append(samples, ft)
		case <-deadline:
			t.Fatalf("管线超时未完成，抓取采样序列=%v", samples)
		}
	}

	// 断言：至少有一次采样时，抓取数已经明显推进（>2），
	// 说明判定期没有把抓取完全卡死。
	advanced := false
	for _, n := range samples {
		if n > 2 {
			advanced = true
			break
		}
	}
	if len(samples) > 3 && !advanced {
		t.Fatalf("抓取疑似停滞（判定期未能继续抓取）：%v", samples)
	}
}

// pending 通道必须有界：不能随站点数无限增长。
func TestPendingIsBounded(t *testing.T) {
	opts := Options{BrowserWorkers: 4}
	cap := opts.RenderQueue() + opts.BrowserWorkers
	if cap >= 100 {
		t.Fatalf("pending 容量 %d 过大，失去反压意义", cap)
	}
	_ = check.Run
}

package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ^C 中断必须让 Run 立刻收口：带着已有判定返回，不等剩余站点跑完。
// 慢站点保证「中断那一刻一定还有大量站点没判」，否则测不出提前退出。
func TestRunReturnsPromptlyOnCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	const n = 40
	sites := make([]Site, 0, n)
	for i := 0; i < n; i++ {
		sites = append(sites, Site{ID: i + 1, URL: srv.URL})
	}
	opts := Options{
		HTTPWorkers: 2, BrowserWorkers: 1, FetchQueue: 2,
		TimeoutMS: 5000, JSSettleMS: 1000, Stage: "raw", Stats: &StageStats{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(800 * time.Millisecond) // 全跑完要 ~8s，此刻才判了几站
		cancel()
	}()

	t0 := time.Now()
	done := make(chan []Verdict, 1)
	go func() { done <- Run(ctx, sites, opts) }()

	select {
	case vs := <-done:
		if elapsed := time.Since(t0); elapsed > 3*time.Second {
			t.Fatalf("中断后迟迟不返回：%v", elapsed)
		}
		if len(vs) >= n {
			t.Fatalf("中断本该少判，实际 %d/%d 全判完了", len(vs), n)
		}
		if len(vs) == 0 {
			t.Fatal("中断后应保留已判定的部分，实际 0 条")
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("中断后 Run 未在 5s 内返回：有 goroutine 卡在阻塞发送上")
	}
}

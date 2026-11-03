package worker_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	"travellings-bot-go/master"
	mspb "travellings-bot-go/proto/mspb"
	"travellings-bot-go/worker"
)

const entryPage = "<html><body><a href=\"https://www.travellings.cn/go.html\">go</a></body></html>"

// M4b：批内并发 + refill 补拉。8 站交 HTTPWorkers=4 的 worker——
// 在手 < 4×25% 时补拉下一批；验证并发路径不重不漏、全部判 RUN、峰值真并行、正常收口。
func TestConcurrentBatchRefill(t *testing.T) {
	var concurrent, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := concurrent.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond) // 制造可观测的重叠窗口
		concurrent.Add(-1)
		_, _ = w.Write([]byte(entryPage))
	}))
	defer srv.Close()

	tasks := make([]*mspb.Task, 8)
	for i := range tasks {
		tasks[i] = &mspb.Task{Id: int32(i + 1), Url: srv.URL, Name: "s"}
	}
	lis := bufconn.Listen(1 << 20)
	round := master.NewRound("good-token", tasks, 0)
	s := grpc.NewServer()
	master.Register(s, round)
	go func() { _ = s.Serve(lis) }()
	defer s.Stop()

	conn, err := worker.Dial(context.Background(), worker.Options{
		Master: "passthrough:///bufnet", Token: "good-token",
		Dialer: func(c context.Context, _ string) (net.Conn, error) { return lis.DialContext(c) },
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := worker.RunOnceVia(ctx, conn, worker.Options{
		HTTPWorkers: 4, Token: "good-token", TimeoutMS: 3000, RefillPct: 25,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 8 {
		t.Fatalf("want 8 回传，got %d", n)
	}
	fs := round.Verdicts()
	if len(fs) != 8 {
		t.Fatalf("master 终判应 8 条，got %d", len(fs))
	}
	seen := map[int32]bool{}
	for _, v := range fs {
		if v.GetStatus() != "RUN" {
			t.Errorf("id=%d 应判 RUN, got %s", v.GetSiteId(), v.GetStatus())
		}
		if seen[v.GetSiteId()] {
			t.Fatalf("id=%d 重复回传（并发漏了去重/租约）", v.GetSiteId())
		}
		seen[v.GetSiteId()] = true
	}
	if peak.Load() < 2 {
		t.Errorf("并发峰值=%d，want≥2（批内没真并行，workers×1 语义落空）", peak.Load())
	}
}

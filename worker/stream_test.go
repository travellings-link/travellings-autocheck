package worker_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	"travellings-bot-go/master"
	mspb "travellings-bot-go/proto/mspb"
	"travellings-bot-go/worker"
)

// 证据测：协议层回传是「完成一个回传一个」还是「攒一批统一回」？
// /fast 秒回、/slow 服务端睡 800ms，两个 consumer 并发领走。
// 逐条流式 → fast 的 Report 先到、两条间隔≈800ms；攒批 → 两条几乎同时到。
func TestReportsStreamPerTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(800 * time.Millisecond)
		}
		_, _ = w.Write([]byte(entryPage))
	}))
	defer srv.Close()

	var mu sync.Mutex
	type arrival struct {
		site int32
		at   time.Duration
	}
	var arrivals []arrival
	t0 := time.Now()
	// 服务端拦截器掐表：只观测，不改生产 API。
	spy := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if strings.HasSuffix(info.FullMethod, "/Report") {
			if rr, ok := req.(*mspb.ReportRequest); ok {
				mu.Lock()
				arrivals = append(arrivals, arrival{rr.GetVerdict().GetSiteId(), time.Since(t0)})
				mu.Unlock()
			}
		}
		return h(ctx, req)
	}

	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer(grpc.UnaryInterceptor(spy))
	master.Register(s, master.NewRound("good-token", []*mspb.Task{
		{Id: 1, Url: srv.URL + "/slow", Name: "slow"},
		{Id: 2, Url: srv.URL + "/fast", Name: "fast"},
	}, 0))
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
	if _, err := worker.RunOnceVia(ctx, conn, worker.Options{
		HTTPWorkers: 2, Token: "good-token", TimeoutMS: 5000,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != 2 {
		t.Fatalf("want 2 条独立 Report RPC, got %d", len(arrivals))
	}
	first, second := arrivals[0], arrivals[1]
	t.Logf("到达顺序: id=%d@%v → id=%d@%v", first.site, first.at.Round(time.Millisecond), second.site, second.at.Round(time.Millisecond))
	if first.site != 2 {
		t.Fatalf("先完成的 fast(id=2) 没有先回传，疑似攒批：first=%d", first.site)
	}
	if gap := second.at - first.at; gap < 400*time.Millisecond {
		t.Fatalf("两条 Report 间隔仅 %v（<400ms），疑似一批做完统一回传", gap)
	}
}

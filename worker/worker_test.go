package worker_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"travellings-bot-go/master"
	mspb "travellings-bot-go/proto/mspb"
	"travellings-bot-go/worker"
)

func seedRound(t *testing.T, batch int) (*grpc.ClientConn, func()) {
	t.Helper()
	seed := []*mspb.Task{{Id: 1, Url: "https://a.example"}, {Id: 2, Url: "https://b.example"}}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	master.Register(srv, master.NewRound("good-token", seed, batch))
	go func() { _ = srv.Serve(lis) }()
	conn, err := worker.Dial(context.Background(), worker.Options{
		Master: "passthrough:///bufnet",
		Token:  "good-token",
		Dialer: func(c context.Context, _ string) (net.Conn, error) { return lis.DialContext(c) },
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn, func() { conn.Close(); srv.Stop() }
}

// M1 验收①：好 token 领任务（batch=workers×1）；坏 token Unauthenticated；排空后哨兵。
func TestHandshakeSentinelToken(t *testing.T) {
	conn, stop := seedRound(t, 1) // batch 封顶 1 → 一次只给一条
	defer stop()
	ctx := context.Background()
	cli := mspb.NewCheckClient(conn)
	withTok := func(tok string) context.Context {
		return metadata.AppendToOutgoingContext(ctx, "authorization", tok)
	}

	pr, err := cli.Pull(withTok("good-token"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if err != nil {
		t.Fatalf("good pull: %v", err)
	}
	if pr.GetAction() != mspb.Action_ASSIGN || len(pr.GetTasks()) != 1 {
		t.Fatalf("want ASSIGN×1 (workers×1), got action=%v n=%d", pr.GetAction(), len(pr.GetTasks()))
	}

	if _, err := cli.Pull(withTok("bad-token"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token: want Unauthenticated, got %v (code=%v)", err, status.Code(err))
	}

	// 领尽队列（task2），制造「队列空但 2 条在途未收口」状态。
	if p2, err := cli.Pull(withTok("good-token"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1}); err != nil || len(p2.GetTasks()) != 1 {
		t.Fatalf("pull task2: err=%v n=%d", err, len(p2.GetTasks()))
	}
	// 队列暂空但未收口：必须回空批 ASSIGN、绝不发哨兵——冒烟抓到的语义 bug 回归锁。
	idle, err := cli.Pull(withTok("good-token"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if err != nil {
		t.Fatalf("pull while inflight: %v", err)
	}
	if idle.GetAction() != mspb.Action_ASSIGN || len(idle.GetTasks()) != 0 {
		t.Fatalf("want empty ASSIGN while inflight, got action=%v n=%d", idle.GetAction(), len(idle.GetTasks()))
	}
	// 未领完：把 2 条全部 Report 掉，收口 responded==total → 此后 Pull 必发哨兵
	for i := 0; i < 2; i++ {
		if _, err := cli.Report(withTok("good-token"), &mspb.ReportRequest{
			Lane:    mspb.Lane_LANE_HTTP,
			Verdict: &mspb.Verdict{SiteId: int32(i + 1), Status: "RUN", Stage: "raw"},
		}); err != nil {
			t.Fatalf("report %d: %v", i, err)
		}
	}
	done, err := cli.Pull(withTok("good-token"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if err != nil {
		t.Fatalf("pull after close: %v", err)
	}
	if done.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("want ROUND_DONE sentinel, got %v", done.GetAction())
	}
}

// M2：worker 完整循环——真实 crawl.Raw 抓本地假站、check.Evaluate 判定、逐条回传、
// 收哨兵干净返回。httptest 本地起服：走完整抓取判定代码路径，零 DNS/外网依赖（CI 可跑）。
func TestRunDrainsAndExits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body><a href=\"https://www.travellings.cn/go.html\">开往</a></body></html>"))
	}))
	defer srv.Close()

	lis := bufconn.Listen(1 << 20)
	round := master.NewRound("good-token", []*mspb.Task{
		{Id: 1, Url: srv.URL + "/one"}, {Id: 2, Url: srv.URL + "/two"},
	}, 0)
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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	n, err := worker.RunOnceVia(ctx, conn, worker.Options{HTTPWorkers: 1, Token: "good-token", TimeoutMS: 5000})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 verdicts reported, got %d", n)
	}
	// M2 判定正确性：假站含开往入口 → 真 check.Evaluate 判 RUN，master 终判集收 2 条。
	fs := round.Verdicts()
	if len(fs) != 2 {
		t.Fatalf("master 终判集应 2 条, got %d", len(fs))
	}
	for _, v := range fs {
		if v.GetStatus() != "RUN" {
			t.Fatalf("含入口页应判 RUN, got %q (%s)", v.GetStatus(), v.GetEvidence())
		}
	}
}

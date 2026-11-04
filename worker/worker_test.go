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

func seedRound(t *testing.T, batch int) (*grpc.ClientConn, *master.Round, func()) {
	t.Helper()
	seed := []*mspb.Task{{Id: 1, Url: "https://a.example"}, {Id: 2, Url: "https://b.example"}}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	round := master.NewRound("good-token", seed, batch)
	master.Register(srv, round)
	go func() { _ = srv.Serve(lis) }()
	conn, err := worker.Dial(context.Background(), worker.Options{
		Master: "passthrough:///bufnet",
		Token:  "good-token",
		Dialer: func(c context.Context, _ string) (net.Conn, error) { return lis.DialContext(c) },
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn, round, func() { conn.Close(); srv.Stop() }
}

// M1 验收①（惊群语义更新版）：好 token 领任务（batch=workers×1）；坏 token 拒；
// 空腿未收口的 Pull 必须挂起到 pullMax 才回空批（旧契约秒回空批才是逼轮询的根因）；
// 回传须带 nonce；收口后 Pull 立即拿到哨兵（广播，不等 pullMax）。
func TestHandshakeSentinelToken(t *testing.T) {
	conn, round, stop := seedRound(t, 1)
	defer stop()
	round.SetPullMaxWait(80 * time.Millisecond) // 否则空腿按生产默认挂满 10 分钟
	ctx := context.Background()
	cli := mspb.NewCheckClient(conn)
	withTok := func(tk string) context.Context {
		return metadata.AppendToOutgoingContext(ctx, "authorization", tk)
	}

	pr, err := cli.Pull(withTok("good-token"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if err != nil {
		t.Fatalf("good pull: %v", err)
	}
	if pr.GetAction() != mspb.Action_ASSIGN || len(pr.GetTasks()) != 1 {
		t.Fatalf("want ASSIGN×1 (workers×1), got action=%v n=%d", pr.GetAction(), len(pr.GetTasks()))
	}
	task1 := pr.GetTasks()[0]

	if _, err := cli.Pull(withTok("bad-token"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token: want Unauthenticated, got %v (code=%v)", err, status.Code(err))
	}

	p2, err := cli.Pull(withTok("good-token"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if err != nil || len(p2.GetTasks()) != 1 {
		t.Fatalf("pull task2: err=%v n=%d", err, len(p2.GetTasks()))
	}
	task2 := p2.GetTasks()[0]

	st := time.Now()
	idle, err := cli.Pull(withTok("good-token"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if err != nil {
		t.Fatalf("pull while inflight: %v", err)
	}
	if idle.GetAction() != mspb.Action_ASSIGN || len(idle.GetTasks()) != 0 {
		t.Fatalf("want empty ASSIGN while inflight, got action=%v n=%d", idle.GetAction(), len(idle.GetTasks()))
	}
	if held := time.Since(st); held < 60*time.Millisecond {
		t.Fatalf("空腿 Pull 没挂起（elapsed=%v），惊群未消", held)
	}

	for _, task := range []*mspb.Task{task1, task2} {
		if _, err := cli.Report(withTok("good-token"), &mspb.ReportRequest{
			Lane:    mspb.Lane_LANE_HTTP,
			Verdict: &mspb.Verdict{SiteId: task.GetId(), Nonce: task.GetNonce(), Status: "RUN", Stage: "raw"},
		}); err != nil {
			t.Fatalf("report: %v", err)
		}
	}
	st2 := time.Now()
	done, err := cli.Pull(withTok("good-token"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if err != nil {
		t.Fatalf("pull after close: %v", err)
	}
	if done.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("want ROUND_DONE sentinel, got %v", done.GetAction())
	}
	if time.Since(st2) > 50*time.Millisecond {
		t.Fatal("收口后的 Pull 应被广播立即唤醒，不该等 pullMax")
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

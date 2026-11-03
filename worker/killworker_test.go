package worker_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"travellings-bot-go/master"
	mspb "travellings-bot-go/proto/mspb"
	"travellings-bot-go/worker"
)

// M4c：kill-worker 全链路。两个站，僵尸 worker 领走 s1 后死掉不回传，
// 租约到期 master 回收→真 worker 重领 s1→两站判 RUN→收口发哨兵→worker 正常退出。
// 这条锁死提案核心承诺「从节点运行一半炸了，任务被哈希表记录、超时自动回待派队列」，
// 且把 M4a(Reclaim)+M4b(并发 producer) 串成真实场景（此前各自只有单元测）。
func TestKilledWorkerReclaimsAndCompletes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(entryPage))
	}))
	defer srv.Close()

	lis := bufconn.Listen(1 << 20)
	round := master.NewRound("good-token", []*mspb.Task{
		{Id: 1, Url: srv.URL, Name: "s1"}, {Id: 2, Url: srv.URL, Name: "s2"},
	}, 0)
	round.SetLease(300*time.Millisecond, 300*time.Millisecond, nil)
	s := grpc.NewServer()
	master.Register(s, round)
	go func() { _ = s.Serve(lis) }()
	defer s.Stop()

	dial := func(c context.Context, _ string) (net.Conn, error) { return lis.DialContext(c) }
	dialer := worker.Options{Master: "passthrough:///bufnet", Token: "good-token", Dialer: dial}

	// 僵尸 worker：领走 1 条就"崩溃"（不回传、连接留着但不再 Report）。
	ghostConn, err := worker.Dial(context.Background(), dialer)
	if err != nil {
		t.Fatalf("dial ghost: %v", err)
	}
	defer ghostConn.Close()
	gcli := mspb.NewCheckClient(ghostConn)
	ctx0 := context.Background()
	gctx, gcancel := context.WithTimeout(ctx0, 5*time.Second)
	defer gcancel()
	// 领走即“冻结”：此后 ghost 不再 Report，s1 悬在 master 的在途租约里。
	gpr, err := gcli.Pull(metadata.AppendToOutgoingContext(gctx, "authorization", "good-token"),
		&mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if err != nil || len(gpr.GetTasks()) != 1 {
		t.Fatalf("ghost pull: err=%v n=%d", err, len(gpr.GetTasks()))
	}
	// 到此为止——ghost 手里那 1 站永不回传。

	// master 侧后台回收扫描（等价 mscmd 的 ticker；测试里手动驱动避免时序耦合真 ticker）。
	stopReclaim := make(chan struct{})
	go func() {
		tk := time.NewTicker(80 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopReclaim:
				return
			case <-tk.C:
				round.Reclaim(time.Now())
			}
		}
	}()
	defer close(stopReclaim)

	// 真 worker：处理完 s2，回收后捡到 s1，凑齐收口。
	conn, err := worker.Dial(context.Background(), dialer)
	if err != nil {
		t.Fatalf("dial live: %v", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	n, err := worker.RunOnceVia(ctx, conn, worker.Options{
		HTTPWorkers: 1, Token: "good-token", TimeoutMS: 3000,
	})
	if err != nil {
		t.Fatalf("live run: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 回传（含回收重派的 s1），got %d", n)
	}
	if !round.Completed() {
		t.Fatal("僵尸站被回收重派后本轮应收口")
	}
	seen := map[int32]bool{}
	for _, v := range round.Verdicts() {
		if v.GetStatus() != "RUN" {
			t.Errorf("id=%d 应判 RUN, got %s", v.GetSiteId(), v.GetStatus())
		}
		seen[v.GetSiteId()] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("两站都应出终判: %v", seen)
	}
}

// ghostCtx 给僵尸 client 一个独立短 ctx（领完即弃，不回传）。
func ghostCtx(parent context.Context) context.Context {
	c, cancel := context.WithTimeout(parent, 2*time.Second)
	_ = cancel // 故意不 defer：领走任务后连接“冻结”，模拟进程还在但卡死
	return c
}

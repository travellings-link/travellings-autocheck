package worker_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	"travellings-bot-go/crawl"
	"travellings-bot-go/master"
	mspb "travellings-bot-go/proto/mspb"
	"travellings-bot-go/worker"
)

// M3 端到端：假 SPA 站（静态 HTML 无 <a>，JS 注入开往链接）交 both-capable worker。
//
//	HTTP 腿 crawl.Raw 判 LOST+Escalate → master 路由入 Browser 队列（D14，worker 不本地升级）
//	→ Browser 腿 crawl.Rendered 提取注入链接判 RUN → master 收口发哨兵。
//
// 同时锁死「两腿并发」这一 M3 正确性根因：若 run() 串行跑两腿，HTTP 腿拿到 escalate 后
// 队列空、进空转轮询等一个依赖 Browser 腿（尚未起跑）的哨兵 → 死锁。并发才解。
// 本机无头 Chrome 不可用时 skip（对齐 crawl 包既有约定），不硬绑 CI 环境。
func TestBrowserRenderEndToEnd(t *testing.T) {
	// 假 SPA：静态无 <a href>（Raw 提不到→LOST+Escalate），JS 注入开往链接（Rendered 命中→RUN）。
	spa := "<html><body><script>document.body.insertAdjacentHTML('beforeend'," +
		"'<a href=\"https://www.travellings.cn/go.html\">go</a>')</script></body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(spa))
	}))
	defer srv.Close()

	// 前置探测：本机无头 Chrome 能否渲染该 httptest 页。实测本机 Chrome 对 loopback
	// 报 ERR_BLOCKED_BY_CLIENT（扩展/代理拦本地地址），而外网开往页可正常渲染——
	// 环境限制非代码缺陷：不拦 loopback 的干净环境（如 CI 镜像）本测会真跑。
	probeCtx, pcancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer pcancel()
	if _, err := crawl.Rendered(probeCtx, "", srv.URL, time.Second, nil); err != nil {
		t.Skipf("本机无法建立无头 Chrome，跳过渲染端到端：%v", err)
	}

	url := srv.URL + "/spa"
	lis := bufconn.Listen(1 << 20)
	round := master.NewRound("good-token", []*mspb.Task{{Id: 1, Url: url, Name: "SPA"}}, 0)
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// both 能力：两腿并发。HTTPWorkers=1 处理初审、BrowserWorkers=1 消化 escalate。
	n, err := worker.RunOnceVia(ctx, conn, worker.Options{
		HTTPWorkers: 1, BrowserWorkers: 1, Token: "good-token",
		TimeoutMS: 5000, JSSettleMS: 2000, CDPURL: "",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n < 2 {
		t.Fatalf("want ≥2 回传（LOST+Escalate 一次、渲染 RUN 一次），got %d", n)
	}
	// master 终判：该站经渲染腿判 RUN（escalate 中间态不进 final）。
	fs := round.Verdicts()
	if len(fs) != 1 {
		t.Fatalf("终判应 1 条（渲染后 RUN），got %d", len(fs))
	}
	if fs[0].GetStatus() != "RUN" || fs[0].GetStage() != "js" {
		t.Fatalf("SPA 站渲染后应判 RUN/js，got %s/%s (%s)", fs[0].GetStatus(), fs[0].GetStage(), fs[0].GetEvidence())
	}
	if !round.Completed() {
		t.Fatal("渲染终判后本轮应收口")
	}
}

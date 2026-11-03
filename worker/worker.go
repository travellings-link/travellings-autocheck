// Package worker 是主从架构的 worker 侧：能力自适应拉流循环（gRPC client）。
//
// 按 --http-workers / --browser-workers 决定拉哪条流（D5）；批次 = workers×1（D10）；
// 逐条回传（不等整批，提案原文口径）；收到 ROUND_DONE 丢弃在途、干净返回（D7）。
// M1 跑握手：领任务→回传假 verdict→收哨兵。真实 crawl/check 在 M2–M3 接入。
package worker

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"travellings-bot-go/check"
	"travellings-bot-go/crawl"
	mspb "travellings-bot-go/proto/mspb"
)

// Options 是 worker 拉流参数（对齐 MS_TASK §6 命令面）。
type Options struct {
	Master         string // host:port，worker 主动连入（NAT 友好）
	Token          string // pre-shared token（D6）
	HTTPWorkers    int    // >0 拉 HTTP 流；同时 = HTTP 批次量（D5/D10）
	BrowserWorkers int    // >0 拉 Browser 流；同时 = Browser 批次量
	TimeoutMS      int    // HTTP 抓取单请求预算（crawl.Raw）
	// Dialer 仅测试注入（bufconn 进程内），留空走默认 TCP。
	Dialer func(ctx context.Context, addr string) (net.Conn, error)
}

// Dial 连 master（v1 不加密、insecure 传输，内网/隧道口径 D6）。conn 由调用方 Close。
func Dial(ctx context.Context, o Options) (*grpc.ClientConn, error) {
	dopts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if o.Dialer != nil {
		dopts = append(dopts, grpc.WithContextDialer(o.Dialer))
	}
	return grpc.NewClient(o.Master, dopts...)
}

func withToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", token)
}

// RunOnceVia 在既有连接上跑拉流循环（命令层与测试共用，避免各写一套）。
func RunOnceVia(ctx context.Context, conn *grpc.ClientConn, o Options) (int, error) {
	return run(ctx, mspb.NewCheckClient(conn), o)
}

// RunOnce 连一次 master 跑完拉流循环；返回回传出去的 verdict 数。
// Unauthenticated 是配置错误（token 不对），直接上抛；其余错误同样上抛，
// 由命令层按 D8 决定退避重试。
func RunOnce(ctx context.Context, o Options) (int, error) {
	conn, err := Dial(ctx, o)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return run(ctx, mspb.NewCheckClient(conn), o)
}

func run(ctx context.Context, cli mspb.CheckClient, o Options) (int, error) {
	n := 0
	lanes := []mspb.Lane{}
	if o.HTTPWorkers > 0 {
		lanes = append(lanes, mspb.Lane_LANE_HTTP)
	}
	if o.BrowserWorkers > 0 {
		lanes = append(lanes, mspb.Lane_LANE_BROWSER)
	}
	if len(lanes) == 0 {
		return 0, fmt.Errorf("worker 无能力：--http-workers 与 --browser-workers 都是 0")
	}

	for _, ln := range lanes {
		want := int32(o.HTTPWorkers)
		if ln == mspb.Lane_LANE_BROWSER {
			want = int32(o.BrowserWorkers)
		}
		for {
			if err := ctx.Err(); err != nil {
				return n, err
			}
			pr, err := cli.Pull(withToken(ctx, o.Token), &mspb.PullRequest{Lane: ln, Want: want})
			if err != nil {
				return n, fmt.Errorf("pull lane=%v: %w", ln, err)
			}
			if pr.GetAction() == mspb.Action_ROUND_DONE {
				return n, nil // D7：本轮结束，丢弃在途、干净退出
			}
			for _, t := range pr.GetTasks() {
				// 真实干活按腿分：HTTP 腿 crawl.Raw+check.Evaluate（M2）；
				// Browser 腿 crawl.Rendered 留 M3。escalate 一律如实回传、交 master 路由
				// （D14 铁律：worker 绝不原地升级）。
				// ponytail: 此处串行逐条抓取——批次=workers×1 的并发语义 + D9 背压
				// 随 M4 一起落地（并发才有「剩余<阈值再拉」的意义）；M2 只验判定/写库正确性。
				var vr *mspb.Verdict
				if ln == mspb.Lane_LANE_HTTP {
					vr = judgeHTTP(ctx, o, t)
				} else {
					vr = &mspb.Verdict{SiteId: t.GetId(), Url: t.GetUrl(),
						Status: "RUN", Stage: "js", Evidence: "m3-pending"}
				}
				rr, err := cli.Report(withToken(ctx, o.Token), &mspb.ReportRequest{Lane: ln, Verdict: vr})
				if err != nil {
					return n, fmt.Errorf("report id=%d: %w", t.GetId(), err)
				}
				n++
				if rr.GetAction() == mspb.Action_ROUND_DONE {
					return n, nil
				}
			}
			// 队列暂空但未收口：有同伴在途（M4 起由租约回收兜底）。
			// 等一拍再拉，绝不在此退出——退出信号只有哨兵（D7）。
			if len(pr.GetTasks()) == 0 {
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
					return n, ctx.Err()
				}
			}
		}
	}
	return n, nil
}

// judgeHTTP 是 worker 的 HTTP 初审腿：与 bot run 的 Fetcher+Judge 用同一套
// crawl.Raw / check.Evaluate，判定语汇逐字节对齐单机（MS_TASK §5，不复制实现）。
func judgeHTTP(ctx context.Context, o Options, t *mspb.Task) *mspb.Verdict {
	timeout := time.Duration(o.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	page := crawl.Raw(ctx, t.GetUrl(), timeout)
	res := check.Evaluate(page, check.Raw)
	return &mspb.Verdict{
		SiteId:   t.GetId(),
		Url:      t.GetUrl(),
		Status:   string(res.Verdict),
		Stage:    string(check.Raw),
		Evidence: res.Evidence,
		Escalate: res.Escalate, // 交 master 决定是否入 Browser 队列（D14）
	}
}

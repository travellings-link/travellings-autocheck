// Package worker 是主从架构的 worker 侧：能力自适应拉流循环（gRPC client）。
//
// 按 --http-workers / --browser-workers 决定拉哪条流（D5），两腿并发跑
// （escalate 的渲染任务由 Browser 腿消化，串行会死锁——见 run 注释）；
// 批次 = workers×1（D10）；逐条回传（不等整批，提案原文口径）；
// 收到 ROUND_DONE 丢弃在途、干净返回（D7）。失败一律如实回传交 master 重派
// （D14 铁律：worker 绝不原地重试/升级）。
package worker

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
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
	JSSettleMS     int    // 渲染等 SPA 挂载上限（crawl.Rendered）
	CDPURL         string // 浏览器调试端点；空/连不上 → crawl 回退本地无头 Chrome
	RefillPct      int    // 在手剩余 < 批次×该百分比时 producer 补拉（D9）；<=0 按 25
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

	// 两腿必须并发：HTTP 初审 escalate 出的渲染任务要等 Browser 腿消化，
	// 哨兵又要等渲染终判回来。若串行跑两腿，both 能力 worker 的 HTTP 腿会
	// 空转等一个依赖 Browser 腿（还没起跑）的哨兵——死锁（M3 侦察实测）。
	var (
		mu   sync.Mutex
		n    int
		errs []error
	)
	var sentinel atomic.Bool
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	fail := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
		cancel() // 一腿故障收掉另一腿：RunOnce 上抛，由命令层按 D8 退避重试
	}

	// 批内并发：producer 按 refill-pct 阈值补拉一整批进有界 pending，want 个 consumer
	// 并发消化。want = --http-workers/--browser-workers 才真正兑现「workers×1 并发」
	// （D10）——此前一次领 want 条却串行处理，线程数形同虚设。
	// pending 容量 2×want：一个批在处理、一个批排队，满了即反压 producer 不再拉。
	runLane := func(ln mspb.Lane) {
		want := o.HTTPWorkers
		if ln == mspb.Lane_LANE_BROWSER {
			want = o.BrowserWorkers
		}
		if want <= 0 {
			want = 1
		}
		pct := o.RefillPct
		if pct <= 0 {
			pct = 25
		}
		// 补拉触发点（D9）：在手剩余 < want×pct%，向上取整避免 want 小时恒不补。
		refillAt := (want*pct + 99) / 100

		pending := make(chan *mspb.Task, 2*want)
		var inflight atomic.Int64 // 已领未回传 = 在手（含排队 + 正在处理）

		// consumer×want：crawl.Raw 纯函数、crawl.Rendered 走连接池（池大小=BrowserWorkers，
		// 与本处 want 一致），并发安全。
		var cwg sync.WaitGroup
		for i := 0; i < want; i++ {
			cwg.Add(1)
			go func() {
				defer cwg.Done()
				for t := range pending {
					if ctx.Err() != nil {
						return // D7：见哨兵/中断后丢弃在途，不回传（master 按租约/重派处理）
					}
					var vr *mspb.Verdict
					if ln == mspb.Lane_LANE_HTTP {
						vr = judgeHTTP(ctx, o, t)
					} else {
						vr = judgeBrowser(ctx, o, t)
					}
					if ctx.Err() != nil {
						inflight.Add(-1)
						return
					}
					rr, err := cli.Report(withToken(ctx, o.Token), &mspb.ReportRequest{Lane: ln, Verdict: vr})
					if err != nil {
						if ctx.Err() == nil {
							fail(fmt.Errorf("report id=%d: %w", t.GetId(), err))
						}
						return
					}
					inflight.Add(-1)
					mu.Lock()
					n++
					mu.Unlock()
					if rr.GetAction() == mspb.Action_ROUND_DONE {
						sentinel.Store(true)
						cancel()
						return
					}
				}
			}()
		}

		// producer：在手低于阈值才补拉一整批；哨兵/空队列/中断分别处理。
		var pwg sync.WaitGroup
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			defer close(pending) // producer 退出即关，consumer range 自然收摊
			for ctx.Err() == nil {
				if int(inflight.Load()) >= refillAt {
					// 手头够忙，等 consumer 消化下来再补拉（refill-pct 语义）。
					select {
					case <-time.After(200 * time.Millisecond):
					case <-ctx.Done():
						return
					}
					continue
				}
				pr, err := cli.Pull(withToken(ctx, o.Token), &mspb.PullRequest{Lane: ln, Want: int32(want)})
				if err != nil {
					if ctx.Err() == nil { // 兄弟腿见哨兵/外部 ^C：不算故障
						fail(fmt.Errorf("pull lane=%v: %w", ln, err))
					}
					return
				}
				if pr.GetAction() == mspb.Action_ROUND_DONE {
					sentinel.Store(true) // D7：本轮结束
					cancel()
					return
				}
				tasks := pr.GetTasks()
				if len(tasks) == 0 {
					// 队列暂空但未收口：活还捏在别的腿/worker 手里（M4a 租约回收兜底）。
					// 等一拍再拉，绝不在此退出——退出信号只有哨兵（D7）。
					select {
					case <-time.After(time.Second):
					case <-ctx.Done():
						return
					}
					continue
				}
				inflight.Add(int64(len(tasks)))
				for _, t := range tasks {
					select {
					case pending <- t:
					case <-ctx.Done():
						return // 中断：未入队的丢弃（D7）
					}
				}
			}
		}()

		pwg.Wait() // producer 结束（已 close(pending)）
		cwg.Wait() // 消费者排空剩余
	}

	var wg sync.WaitGroup
	for _, ln := range lanes {
		wg.Add(1)
		go func(l mspb.Lane) {
			defer wg.Done()
			runLane(l)
		}(ln)
	}
	wg.Wait()

	if len(errs) > 0 {
		return n, errs[0]
	}
	if sentinel.Load() {
		return n, nil
	}
	return n, ctx.Err() // 外部取消（^C）：按中断语义上抛
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
		Name:     t.GetName(),
		Url:      t.GetUrl(),
		Status:   string(res.Verdict),
		Stage:    string(check.Raw),
		Evidence: res.Evidence,
		Escalate: res.Escalate, // 交 master 决定是否入 Browser 队列（D14）
	}
}

// judgeBrowser 是 worker 的渲染腿：与单机管线 Renderer+evalRendered 同语汇——
// 成功走 check.Evaluate(page, JS)；站点导航超时打 NavTimeout；其余（CDP 断连、
// 浏览器崩溃）打 InfraErr。两者都非空 = 不写库契约，由 master 按 D15 决定
// 是否重派 Browser 腿重试——worker 绝不本地重试（D14）。
// 渲染阶段不升级（防环，与单机 Judge 一致），mspb 上 Escalate 恒为 false。
func judgeBrowser(ctx context.Context, o Options, t *mspb.Task) *mspb.Verdict {
	settle := time.Duration(o.JSSettleMS) * time.Millisecond
	if settle <= 0 {
		settle = 8 * time.Second
	}
	v := &mspb.Verdict{SiteId: t.GetId(), Name: t.GetName(), Url: t.GetUrl(), Stage: string(check.JS)}
	page, err := crawl.Rendered(ctx, o.CDPURL, t.GetUrl(), settle,
		func(pp check.Page) bool { _, ok := check.MatchTravellings(pp); return ok })
	if err != nil {
		msg := err.Error()
		if check.IsSiteNavTimeout(msg) {
			v.Status = string(check.Timeout)
			v.Evidence = "nav timeout: " + msg
			v.NavTimeout = msg
		} else {
			// 语汇仍交给 Evaluate（超时→TIMEOUT、其余→ERROR），与线上状态串一致；
			// InfraErr 标记让写库环节跳过——那是我们自己的故障，不是站点的死刑。
			v.Status = string(check.Evaluate(check.Page{FetchErr: msg}, check.JS).Verdict)
			v.Evidence = "infra js-leg: " + msg
			v.InfraErr = msg
		}
		return v
	}
	res := check.Evaluate(page, check.JS)
	v.Status = string(res.Verdict)
	v.Evidence = res.Evidence
	return v
}

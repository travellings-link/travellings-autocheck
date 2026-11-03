// Package master 是主从架构的 master 侧：内存四结构 + gRPC CheckServer + 路由核。
//
// 铁律（用户 2026-11-04 补充）：worker 绝不原地重试、绝不原地升级；一切回主节点重分配。
// master 收到每条 Report 后，按「腿 + 判定」决定：终结 / 转 Browser 队列(升级) /
// 回原队列(重试)。收口 = 每个站都拿到终判（responded==total）。
package master

import (
	"context"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	mspb "travellings-bot-go/proto/mspb"
)

// 重试预算（用户裁决：ERROR/TIMEOUT 重派 1 次、IGNORE 重派 5 次）。
const (
	retryTransient = 1 // 仅源于 HTTP 腿的 ERROR/TIMEOUT
	retryIgnore    = 5 // 仅源于 Browser 腿的 IGNORE（工具链故障/站点超时，皆不写库）
)

func laneIdx(l mspb.Lane) int {
	switch l {
	case mspb.Lane_LANE_HTTP:
		return 0
	case mspb.Lane_LANE_BROWSER:
		return 1
	}
	return -1
}

// siteState 记一个站在两条腿上被派发过几次，用于重试预算。
type siteState struct {
	httpDisp  int  // 被派进 HTTP 队列并领走的次数
	brDisp    int  // 被派进 Browser 队列并领走的次数
	escalated bool // 是否已因 Escalate 转过一次 Browser（HTTP 初判→渲染复核）
}

// Round 是一次巡查轮 master 的全部状态（MS_TASK §3 四结构），纯内存、轮末即焚（D4）。
type Round struct {
	mspb.UnimplementedCheckServer

	token string
	batch int // >0 封顶单次派发（测试用）；0 由 worker want 决定（D10）

	mu        sync.Mutex
	queue     [2][]*mspb.Task // 0=HTTP 待派发、1=Browser 待派发
	sites     map[int32]*siteState
	responded int             // 已终结的站数
	reports   int             // 累计有效回传次数（进度「所有任务」分子，含重试各计一次）
	total     int             // 本轮站点总数
	final     []*mspb.Verdict // 已响应队列：本轮终判集（MS_TASK §3），收口后供轮末写库
	// 存指针不存值：protoimpl.MessageState 含 mutex，按值拷贝会踩 vet copylocks

	onClose     func() // 收口回调（见 OnClose）
	closedFired bool   // 回调只触发一次
}

// NewRound 建一轮：seed 灌入 HTTP 待派发队列（本轮清单，D13 只走 DB / M1 支持 --from-api）。
func NewRound(token string, seed []*mspb.Task, batch int) *Round {
	q := make([]*mspb.Task, 0, len(seed)) // cap 预分配；用 len 会塞进 nil 任务（测试实测踩过）
	r := &Round{token: token, batch: batch, sites: make(map[int32]*siteState), final: make([]*mspb.Verdict, 0, len(seed))}
	for _, t := range seed {
		if t == nil {
			continue
		}
		q = append(q, t)
		r.sites[t.GetId()] = &siteState{}
	}
	r.total = len(q)
	r.queue[0] = q
	return r
}

func (r *Round) checkToken(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	v := md.Get("authorization")
	if len(v) != 1 || v[0] != r.token {
		return status.Error(codes.Unauthenticated, "token mismatch")
	}
	return nil
}

func (r *Round) closedLocked() bool { return r.responded >= r.total }

// Pull：worker 按腿领一批。
//   - 已收口 → ROUND_DONE（D7，唯一退出信号）
//   - 该腿队列暂空但未收口 → 空批 ASSIGN（有站仍在他处/待重试回灌），worker 等一拍重试。
//     绝不把「队列空」当收口——那会丢在途站（M1 冒烟实测过）。
func (r *Round) Pull(ctx context.Context, req *mspb.PullRequest) (*mspb.PullResponse, error) {
	if err := r.checkToken(ctx); err != nil {
		return nil, err
	}
	i := laneIdx(req.GetLane())
	if i < 0 {
		return nil, status.Error(codes.InvalidArgument, "lane 未指定")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closedLocked() {
		return &mspb.PullResponse{Action: mspb.Action_ROUND_DONE}, nil
	}
	q := r.queue[i]
	if len(q) == 0 {
		return &mspb.PullResponse{Action: mspb.Action_ASSIGN}, nil
	}
	n := int(req.GetWant())
	if n <= 0 {
		n = 1
	}
	if r.batch > 0 && n > r.batch {
		n = r.batch
	}
	if n > len(q) {
		n = len(q)
	}
	out := &mspb.PullResponse{Action: mspb.Action_ASSIGN, Tasks: make([]*mspb.Task, n)}
	copy(out.Tasks, q[:n])
	r.queue[i] = q[n:]
	for _, t := range out.Tasks {
		if s := r.sites[t.GetId()]; s != nil {
			if i == 0 {
				s.httpDisp++
			} else {
				s.brDisp++
			}
		}
	}
	return out, nil
}

// Report：worker 逐条回传（提案口径：不等整批攒齐）。master 在此做路由/重试决策。
// 关键：escalate / retry 都是「回灌对应队列」，worker 侧不自己处理（用户铁律）。
// OnClose 在收口瞬间触发一次（M2 在此接闸门→写库→通知；M1 只用于停进度、优雅退出）。
// 须在 NewRound 后、Serve 前设置；回调在独立 goroutine 执行，不持 r.mu。
func (r *Round) OnClose(f func()) { r.onClose = f }

func (r *Round) Report(ctx context.Context, req *mspb.ReportRequest) (*mspb.ReportResponse, error) {
	if err := r.checkToken(ctx); err != nil {
		return nil, err
	}
	v := req.GetVerdict()
	if v == nil {
		return nil, status.Error(codes.InvalidArgument, "verdict 为空")
	}
	i := laneIdx(req.GetLane())
	if i < 0 {
		return nil, status.Error(codes.InvalidArgument, "lane 未指定")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	s := r.sites[v.GetSiteId()]
	if s == nil {
		// 未知站：可能是回收后重复回传或注入。计数丢弃，不拖垮收口。
		return &mspb.ReportResponse{Action: assignOrDoneLocked(r)}, nil
	}
	// 已知站的有效回传：无论后续走终结/重试/升级，都计入进度「所有任务」分子。
	r.reports++

	// IGNORE（工具链/站点超时）——只可能在 Browser 腿出现，回 Browser 队列重试 ≤5。
	if v.GetInfraErr() != "" || v.GetNavTimeout() != "" {
		if s.brDisp-1 < retryIgnore { // brDisp-1 = 已重试次数（首次不算重试）
			r.queue[1] = append(r.queue[1], &mspb.Task{Id: v.GetSiteId(), Url: v.GetUrl()})
			return &mspb.ReportResponse{Action: assignOrDoneLocked(r)}, nil
		}
		r.responded++ // 预算耗尽：按 IGNORE 终结（M2 起不写库）
		r.final = append(r.final, v)
		return &mspb.ReportResponse{Action: assignOrDoneLocked(r)}, nil
	}

	// 升级（SPA/非确定错误）：escalate=true 且尚未升级过 → 转 Browser 队列。
	if v.GetEscalate() && !s.escalated {
		s.escalated = true
		r.queue[1] = append(r.queue[1], &mspb.Task{Id: v.GetSiteId(), Url: v.GetUrl()})
		return &mspb.ReportResponse{Action: assignOrDoneLocked(r)}, nil
	}

	// HTTP 腿提前断言的 ERROR/TIMEOUT → 回 HTTP 队列重试 ≤1。
	if i == 0 && (v.GetStatus() == "ERROR" || v.GetStatus() == "TIMEOUT") {
		if s.httpDisp-1 < retryTransient {
			r.queue[0] = append(r.queue[0], &mspb.Task{Id: v.GetSiteId(), Url: v.GetUrl()})
			return &mspb.ReportResponse{Action: assignOrDoneLocked(r)}, nil
		}
	}

	// 其余（RUN / LOST 终判 / 裸 HTTP 码 / 预算耗尽的 ERROR/TIMEOUT）→ 终结。
	r.responded++
	r.final = append(r.final, v)
	return &mspb.ReportResponse{Action: assignOrDoneLocked(r)}, nil
}

// Verdicts 返回本轮终判快照（已响应队列）。收口后由轮末逻辑读取转写库。
// 拷贝返回：调用方拿到的是不可变视图，不受后续 Report 影响。
func (r *Round) Verdicts() []*mspb.Verdict {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*mspb.Verdict, len(r.final))
	copy(out, r.final)
	return out
}

// assignOrDoneLocked：收口了就发哨兵，否则让 worker 继续拉。调用方持锁。
// 首次观察到收口时异步触发 OnClose（不持锁执行，避免与 srv.Stop 死锁）。
func assignOrDoneLocked(r *Round) mspb.Action {
	if r.closedLocked() {
		if r.onClose != nil && !r.closedFired {
			f := r.onClose
			r.closedFired = true
			go f()
		}
		return mspb.Action_ROUND_DONE
	}
	return mspb.Action_ASSIGN
}

// Snapshot 供进度条读，两条腿各自「已派发 / 累计任务数」+ 累计回传。
// 口径 = 需求原话：分子「已派出去」= 该腿累计派发；分母「累计任务」= 已派出去 + 还没派出去的（队列剩余）。
// 升级/重试回灌会让分母随新增派发增长（重试也算一次派发），符合「本轮迄今产生过多少任务」。
func (r *Round) Snapshot() (httpDisp, httpTotal, brDisp, brTotal, reports int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sites {
		httpDisp += s.httpDisp
		brDisp += s.brDisp
	}
	httpTotal = httpDisp + len(r.queue[0])
	brTotal = brDisp + len(r.queue[1])
	return httpDisp, httpTotal, brDisp, brTotal, r.reports
}

// Register 把一轮挂到 grpc 服务上，命令与测试共用同一份实现（D1 复用）。
func Register(s grpc.ServiceRegistrar, r *Round) { mspb.RegisterCheckServer(s, r) }

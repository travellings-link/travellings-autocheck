// Package master 是主从架构的 master 侧：内存四结构 + gRPC CheckServer + 路由核。
//
// 铁律（用户 2026-11-04 补充）：worker 绝不原地重试、绝不原地升级；一切回主节点重分配。
// master 收到每条 Report 后，按「腿 + 判定」决定：终结 / 转 Browser 队列(升级) /
// 回原队列(重试)。收口 = 每个站都拿到终判（responded==total）。
package master

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"travellings-bot-go/check"
	"travellings-bot-go/pipeline"
	mspb "travellings-bot-go/proto/mspb"
)

// 重试预算（用户裁决：ERROR/TIMEOUT 重派 1 次、IGNORE 重派 5 次）。
const (
	retryTransient = 1 // 仅源于 HTTP 腿的 ERROR/TIMEOUT
	retryIgnore    = 5 // 仅源于 Browser 腿的 IGNORE（工具链故障/站点超时，皆不写库）

	// defaultPullMaxWait：空队列时 Pull 的最长挂起时间（开发组裁决：10 分钟）。
	// 到点回空批，防中间设备掐断/静默断流把 worker 永久吊住；worker 收到空批立即再拉
	// （新 Pull 又在 master 侧挂起），故无需任何轮询计时器。
	defaultPullMaxWait = 10 * time.Minute
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

// siteState 记一个站在两条腿上被派发过几次，用于重试预算与租约回收。
type siteState struct {
	httpDisp  int  // 被派进 HTTP 队列并领走的次数
	brDisp    int  // 被派进 Browser 队列并领走的次数
	escalated bool // 是否已因 Escalate 转过一次 Browser（HTTP 初判→渲染复核）
	recycles  int  // 已因租约超时回收重派的次数（防 worker 反复死导致的无限循环）

	// 租约（§3 已派发未响应哈希表）。leased=0 表示该站当前不在途。
	url     string
	name    string
	leased  int8      // 当前在途腿：0=无、1=HTTP、2=Browser
	leaseAt time.Time // 该腿被领走的时刻
	nonce   uint64    // 当前派发凭据（per-dispatch token），随 Pull 下发
}

// Round 是一次巡查轮 master 的全部状态（MS_TASK §3 四结构），纯内存、轮末即焚（D4）。
type Round struct {
	mspb.UnimplementedCheckServer

	token string
	batch int // >0 封顶单次派发（测试用）；0 由 worker want 决定（D10）

	// 租约时长（D11，按腿）。<=0 = 该腿不回收（不调 SetLease 时的安全默认，
	// 确保未接线的运行/测试行为零变化）。
	leaseHTTP, leaseBrowser time.Duration
	now                     func() time.Time // 时钟，测试可注入假时钟
	pullMax                 time.Duration    // 空队列 Pull 的最长挂起
	dispatchSeq             uint64           // per-dispatch 计数器（持锁递增）

	// 惊群治理的唤醒原语（开发组裁决：挂起 Pull 而非让其轮询）。
	wake            chan struct{} // 容量 1 的广播：队列有变化 / 收口
	closedCh        chan struct{} // 收口瞬间关闭，唤醒全部挂起 Pull
	closedAnnounced bool          // closedCh 只关一次

	pulls atomic.Int64 // 累计 Pull RPC 次数：对照回归里量化「轮询风暴是否消掉」

	mu        sync.Mutex
	queue     [2][]*mspb.Task // 0=HTTP 待派发、1=Browser 待派发
	sites     map[int32]*siteState
	responded int             // 已终结的站数
	reports   int             // 累计有效回传次数（进度「所有任务」分子，含重试各计一次）
	httpDone  int             // 爬取腿已响应（认账即了结，其后转升级/重试算新任务）
	brDone    int             // 渲染腿已响应（同口径）
	total     int             // 本轮站点总数
	final     []*mspb.Verdict // 已响应队列：本轮终判集（MS_TASK §3），收口后供轮末写库
	// 存指针不存值：protoimpl.MessageState 含 mutex，按值拷贝会踩 vet copylocks

	onClose    func() // 收口回调（见 OnClose）
	closeFired bool   // 回调只触发一次
}

// NewRound 建一轮：seed 灌入 HTTP 待派发队列（本轮清单，D13 只走 DB / M1 支持 --from-api）。
func NewRound(token string, seed []*mspb.Task, batch int) *Round {
	q := make([]*mspb.Task, 0, len(seed)) // cap 预分配；用 len 会塞进 nil 任务（测试实测踩过）
	r := &Round{
		token: token, batch: batch,
		sites:    make(map[int32]*siteState),
		final:    make([]*mspb.Verdict, 0, len(seed)),
		now:      time.Now,
		pullMax:  defaultPullMaxWait,
		wake:     make(chan struct{}, 1),
		closedCh: make(chan struct{}),
	}
	for _, t := range seed {
		if t == nil {
			continue
		}
		q = append(q, t)
		r.sites[t.GetId()] = &siteState{url: t.GetUrl(), name: t.GetName()}
	}
	r.total = len(q)
	r.queue[0] = q
	return r
}

// OnClose 在收口瞬间触发一次（M2 在此接闸门→写库→通知；须 NewRound 后、Serve 前设置；
// 回调在独立 goroutine 执行，不持 r.mu）。
func (r *Round) OnClose(f func()) { r.onClose = f }

// SetLease 设定两腿租约时长与可选时钟（mscmd 在 Serve 前调用；测试注入假时钟）。
// d<=0 的腿不参与回收。未调用则两腿都=0、永不回收（默认安全）。
func (r *Round) SetLease(httpLease, browserLease time.Duration, now func() time.Time) {
	r.leaseHTTP, r.leaseBrowser = httpLease, browserLease
	if now != nil {
		r.now = now
	}
}

// SetPullMaxWait 覆盖空队列 Pull 的挂起上限（mscmd 的 --pull-max-wait）；<=0 忽略。
func (r *Round) SetPullMaxWait(d time.Duration) {
	if d > 0 {
		r.pullMax = d
	}
}

// PullCount 返回累计 Pull RPC 次数。对照回归用它量化惊群：轮询时代次数≈站数×轮询频率，
// 长挂时代只随「真正有活可派/收口」增长。
func (r *Round) PullCount() int64 { return r.pulls.Load() }

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

// signalLocked 非阻塞广播「队列有变化」，唤醒挂起的 Pull。调用方须持锁。
// 容量 1 + default 分支：多次入队合并成一次唤醒，唤醒后各 Pull 重查自己的腿，
// 拿不到就再挂起——内部唤醒不是 RPC，不存在放大。
func (r *Round) signalLocked() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// announceClosedLocked：首次观察到收口时关闭 closedCh 唤醒全部挂起 Pull（GracefulStop
// 要等在途 RPC 结束，不唤醒就会死锁）。调用方须持锁。
func (r *Round) announceClosedLocked() {
	if !r.closedLocked() {
		return
	}
	if !r.closedAnnounced {
		r.closedAnnounced = true
		close(r.closedCh)
	}
	if r.onClose != nil && !r.closeFired {
		f := r.onClose
		r.closeFired = true
		go f()
	}
}

// enqueueLocked 把任务放回某腿待派队列并广播唤醒。调用方须持锁。
func (r *Round) enqueueLocked(lane int, t *mspb.Task) {
	r.queue[lane] = append(r.queue[lane], t)
	r.signalLocked()
}

// Pull：worker 按腿领一批。
//   - 已收口 → ROUND_DONE（D7，唯一退出信号）
//   - 该腿队列暂空但未收口 → **挂起**本次 Pull，直到有新任务回灌（唤醒回循环）、
//     收口（→哨兵）、挂起上限 pullMax 到点（→空批）或 worker 侧取消。
//     这就是惊群治理：把「worker 每秒轮询空队列」换成「master 有状态变化才回应」。
//     空批只可能是 pullMax 超时，worker 收到后立即重拉（新 Pull 又挂起），零轮询计时器。
//   - 绝不把「队列空」当收口——那会丢在途站（M1 冒烟实测过）。
func (r *Round) Pull(ctx context.Context, req *mspb.PullRequest) (*mspb.PullResponse, error) {
	r.pulls.Add(1)
	if err := r.checkToken(ctx); err != nil {
		return nil, err
	}
	i := laneIdx(req.GetLane())
	if i < 0 {
		return nil, status.Error(codes.InvalidArgument, "lane 未指定")
	}

	for {
		r.mu.Lock()
		if r.closedLocked() {
			r.mu.Unlock()
			return &mspb.PullResponse{Action: mspb.Action_ROUND_DONE}, nil
		}
		if len(r.queue[i]) > 0 {
			out := r.assignLocked(i, int(req.GetWant()))
			r.mu.Unlock()
			return out, nil
		}
		maxWait := r.pullMax
		r.mu.Unlock() // 挂起期间绝不持锁，否则 Report/Reclaim 全部堵死

		timer := time.NewTimer(maxWait)
		select {
		case <-r.wake: // 队列变化：重查
		case <-r.closedCh: // 收口：下一轮循环立即返回哨兵
		case <-timer.C: // 挂起上限：回空批，worker 立即重拉（防静默断流永久吊住）
			return &mspb.PullResponse{Action: mspb.Action_ASSIGN}, nil
		case <-ctx.Done(): // worker 断开/进程退出：让 RPC 正常收尾
			timer.Stop()
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		timer.Stop()
	}
}

// assignLocked 从第 i 腿队头派发至多 want 条，登记租约与 per-dispatch nonce。
// 调用方须持锁。
func (r *Round) assignLocked(i, want int) *mspb.PullResponse {
	q := r.queue[i]
	n := want
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
	at := r.now()
	for _, t := range out.Tasks {
		r.dispatchSeq++
		t.Nonce = r.dispatchSeq
		if s := r.sites[t.GetId()]; s != nil {
			if i == 0 {
				s.httpDisp++
			} else {
				s.brDisp++
			}
			s.leased = int8(i + 1) // 登记租约：该站在途，超时由 Reclaim 回收
			s.leaseAt = at
			s.nonce = t.Nonce // 本次派发凭据；重派会覆盖，老 worker 的迟到因此失效
		}
	}
	return out
}

// Report：worker 逐条回传（提案口径：不等整批攒齐）。master 在此做路由/重试决策。
// 关键：escalate / retry 都是「回灌对应队列」，worker 侧不自己处理（用户铁律）。
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
		return &mspb.ReportResponse{Action: r.assignOrDoneLocked()}, nil
	}
	// per-dispatch token：回传必须带「当前这次派发」的 nonce 且腿号匹配才认账。
	// 彻底关掉此前唯一的残余窄窗——回收→同腿重派→老 worker 迟到回传：那轮回传的
	// nonce 属于上一轮派发，与 s.nonce 不等即丢弃。迟到/重复/跨腿/注入统一这一条守卫。
	if s.leased != int8(i+1) || v.GetNonce() == 0 || v.GetNonce() != s.nonce {
		return &mspb.ReportResponse{Action: r.assignOrDoneLocked()}, nil
	}
	s.leased = 0 // 认账即撤销在途租约
	s.nonce = 0
	// 已知站的有效回传：本腿该任务即告了结（其后终结/重试/升级都是另一条任务）。
	// 腿响应与「所有任务」分子同点递增，故两腿响应相加=总响应、各自 ≤ 本腿派发。
	r.reports++
	if i == 0 {
		r.httpDone++
	} else {
		r.brDone++
	}

	// IGNORE（工具链/站点超时）——只可能在 Browser 腿出现，回 Browser 队列重试 ≤5。
	if v.GetInfraErr() != "" || v.GetNavTimeout() != "" {
		if s.brDisp-1 < retryIgnore { // brDisp-1 = 已重试次数（首次不算重试）
			r.enqueueLocked(1, &mspb.Task{Id: v.GetSiteId(), Url: v.GetUrl(), Name: s.name})
			return &mspb.ReportResponse{Action: r.assignOrDoneLocked()}, nil
		}
		r.responded++ // 预算耗尽：按 IGNORE 终结（M2 起不写库）
		r.final = append(r.final, v)
		return &mspb.ReportResponse{Action: r.assignOrDoneLocked()}, nil
	}

	// 升级（SPA/非确定错误）：escalate=true 且尚未升级过 → 转 Browser 队列。
	if v.GetEscalate() && !s.escalated {
		s.escalated = true
		r.enqueueLocked(1, &mspb.Task{Id: v.GetSiteId(), Url: v.GetUrl(), Name: s.name})
		return &mspb.ReportResponse{Action: r.assignOrDoneLocked()}, nil
	}

	// HTTP 腿提前断言的 ERROR/TIMEOUT → 回 HTTP 队列重试 ≤1。
	if i == 0 && (v.GetStatus() == "ERROR" || v.GetStatus() == "TIMEOUT") {
		if s.httpDisp-1 < retryTransient {
			r.enqueueLocked(0, &mspb.Task{Id: v.GetSiteId(), Url: v.GetUrl(), Name: s.name})
			return &mspb.ReportResponse{Action: r.assignOrDoneLocked()}, nil
		}
	}

	// 其余（RUN / LOST 终判 / 裸 HTTP 码 / 预算耗尽的 ERROR/TIMEOUT）→ 终结。
	r.responded++
	r.final = append(r.final, v)
	return &mspb.ReportResponse{Action: r.assignOrDoneLocked()}, nil
}

// Reclaim 扫描在途租约，把「领走后超时未回传」的任务回收（提案承诺的哈希表兜底）。
// 由 masterCmd 的后台 ticker 按小于最短租约的间隔驱动。返回本轮回收数。
//   - 未超租约预算：回灌对应待派队列（唤醒挂起的 Pull，等别的 worker 重领）；
//   - 超预算（worker 反复死）：按不写库终结（InfraErr 标记），防单站无限回收拖垮收口。
func (r *Round) Reclaim(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	reclaimed := 0
	for id, s := range r.sites {
		if s.leased == 0 {
			continue
		}
		lane := int(s.leased) - 1
		lease := r.leaseHTTP
		if lane == 1 {
			lease = r.leaseBrowser
		}
		if lease <= 0 || now.Sub(s.leaseAt) < lease {
			continue
		}
		s.leased = 0
		s.nonce = 0 // 作废本次派发凭据：老 worker 之后回传一律不认
		s.recycles++
		budget := retryTransient
		if lane == 1 {
			budget = retryIgnore
		}
		if s.recycles > budget {
			// 反复没人做成：按不写库终结，收口才能推进（D12：master 无 deadline，靠此兜底）。
			st := check.Raw
			if lane == 1 {
				st = check.JS
			}
			r.final = append(r.final, &mspb.Verdict{
				SiteId: id, Name: s.name, Url: s.url,
				Status: string(check.Error), Stage: string(st),
				Evidence: "lease timeout: worker 领走后未响应",
				InfraErr: "lease timeout: worker 领走后未响应",
			})
			r.responded++
		} else {
			r.enqueueLocked(lane, &mspb.Task{Id: id, Url: s.url, Name: s.name})
		}
		reclaimed++
	}
	if reclaimed > 0 {
		r.announceClosedLocked() // 若因回收终结凑齐收口，唤醒挂起 Pull + 轮末退出
	}
	return reclaimed
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

// assignOrDoneLocked：收口了就发哨兵（并唤醒全部挂起 Pull），否则让 worker 继续拉。
// 调用方须持锁。
func (r *Round) assignOrDoneLocked() mspb.Action {
	if r.closedLocked() {
		r.announceClosedLocked()
		return mspb.Action_ROUND_DONE
	}
	return mspb.Action_ASSIGN
}

// Progress 是主从进度条五行的一致快照（爬取派发/爬取响应/渲染派发/渲染响应/所有任务）。
type Progress struct {
	HTTPDisp, HTTPTotal                    int // 派发：分子=该腿累计已派发；分母=已派发+队列剩余（用户口径）
	HTTPDone                               int // 响应：该腿已认账数；派发-响应=该腿在途（哈希表规模）
	BrowserDisp, BrowserTotal, BrowserDone int
	Reports                                int // 总响应 = HTTPDone + BrowserDone（「所有任务」分子）
}

// Snapshot 供进度条读。重试/升级回灌让分母随新增派发增长（一次派发算一个任务），
// 所以「所有任务」分母可能大于首轮清单——这是设计口径。
func (r *Round) Snapshot() Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	var p Progress
	for _, s := range r.sites {
		p.HTTPDisp += s.httpDisp
		p.BrowserDisp += s.brDisp
	}
	p.HTTPTotal = p.HTTPDisp + len(r.queue[0])
	p.BrowserTotal = p.BrowserDisp + len(r.queue[1])
	p.HTTPDone = r.httpDone
	p.BrowserDone = r.brDone
	p.Reports = r.reports
	return p
}

// Register 把一轮挂到 grpc 服务上，命令与测试共用同一份实现（D1 复用）。
func Register(s grpc.ServiceRegistrar, r *Round) { mspb.RegisterCheckServer(s, r) }

// Completed 报告本轮是否已收口（每个站都拿到终判）。masterCmd 据此决定写库/丢弃。
func (r *Round) Completed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closedLocked()
}

// ToVerdicts 把终判集转成 []pipeline.Verdict——轮末 report.Finalize/Write/Print 与
// feishu 的既有契约都按 pipeline.Verdict 定义，这里只做字段搬运，判定语汇不复制。
func (r *Round) ToVerdicts() []pipeline.Verdict {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]pipeline.Verdict, 0, len(r.final))
	for _, v := range r.final {
		out = append(out, pipeline.Verdict{
			SiteID:     int(v.GetSiteId()),
			Name:       v.GetName(),
			URL:        v.GetUrl(),
			Status:     check.Verdict(v.GetStatus()),
			Stage:      check.Stage(v.GetStage()),
			Evidence:   v.GetEvidence(),
			InfraErr:   v.GetInfraErr(),
			NavTimeout: v.GetNavTimeout(),
		})
	}
	return out
}

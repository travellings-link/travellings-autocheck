package pipeline

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"travellings-bot-go/check"
	"travellings-bot-go/crawl"

	"github.com/rs/zerolog"
)

// pkglog 由 SetLogger 注入；未注入时静默（测试与库用法不受影响）。
var pkglog = zerolog.Nop()

// SetLogger 让调用方把配置好的 logger 传进管线，使 -v 能覆盖抓取/渲染细节。
func SetLogger(l zerolog.Logger) { pkglog = l }

// 管线三段式命名（旧文档里的 A/B/C 腿即此三者）：
//   Fetcher —— 并发 HTTP 抓取原始页面，不做判定
//   Judge   —— 单 goroutine 收口：判定、升级路由、防环
//   Renderer—— 并发浏览器渲染（CDP），为 JS 站点补一次终判机会

// Verdict 是单个站点的最终判定（内存结果）。不碰 DB。
type Verdict struct {
	SiteID int
	// Name 供通知明细表展示；--from-api 模式无名字（该模式不写库也不通知）。
	Name     string
	URL      string
	Status   check.Verdict
	Stage    check.Stage
	Evidence string
	// InfraErr 非空表示失败出在**我们自己的工具链**（CDP 挂掉、浏览器崩溃）。
	// 既定约定：这类结果不得写库，否则等于用自己的故障判站点死刑。
	InfraErr string
	// NavTimeout 非空表示**站点**在导航预算内没加载完。
	// 它同理不写库（预算不足以证明站点坏了），但与 InfraErr 分开计数——
	// 前者说明该查 CDP，后者说明该调 js-settle 或该站点确实慢。
	NavTimeout string
}

// Ignored 判断该条判定是否属于"不写库"：结论可信度不足
// （我方工具链故障 InfraErr 或站点超时 NavTimeout），既不写库
// 也不该混进正常的 ERROR/TIMEOUT 统计。原先散在 main.isIgnore，
// 收敛到数据自身供 report/feishu 共用（MS 重构，单一事实源）。
func (v Verdict) Ignored() bool { return v.InfraErr != "" || v.NavTimeout != "" }

// Options 是 run 管线参数（与 addRunFlags 注册的命令面一一对应）。
type Options struct {
	// HTTPWorkers 是 Fetcher 阶段的并发数。
	HTTPWorkers int
	// BrowserWorkers 是 Renderer 阶段的并发数。
	BrowserWorkers int
	// FetchQueue 是 Fetcher→Judge 的队列深度，用于反压。
	FetchQueue int
	// TimeoutMS 是 Fetcher 单请求预算。
	TimeoutMS int
	// JSSettleMS 是 Renderer 等待 SPA 挂载的上限。
	JSSettleMS int
	// CDPURL 是浏览器调试端点。
	CDPURL string
	// Limit 限制本次巡查站数，0 表示全量。
	Limit int
	// Stage 取 both|raw|js，决定是否进入 Renderer 阶段。
	Stage string
	// RenderRetries 是渲染失败后的重试次数上限。
	// 背景：抓取成功但渲染超时，说明更像是浏览器侧偶发问题，
	// 丢到队列尾部重试往往就能过；直接判 TIMEOUT 会误伤好站。
	// 0 表示不重试（兼容旧行为）。
	RenderRetries int
	// Stats 是可选的三阶段进度回调，供上层显示分段进度。
	// 各阶段语义不同，**不要**指望分母一致：
	//   Fetched  —— 已完成 HTTP 抓取（分母 = 站点总数，单调到满）
	//   Judged   —— 已有判定结论（分母 = 站点总数，单调到满）
	//   Rendered —— 已完成浏览器渲染（分母动态增长，仅统计被升级的那部分）
	Stats *StageStats
}

// StageStats 汇总实时计数，由调用方提供并自行加锁读取。
//
// 除了阶段进度，还按**最终判定**分类计数——这是运维最关心的部分：
// 一眼看出这轮有几个站坏了、几个结论不可信。
type StageStats struct {
	mu       sync.Mutex
	fetched  int
	judged   int
	rendered int
	escalate int  // 已决定要渲染的数量（Rendered 的动态分母）
	finished bool // 管线是否已结束，结束时渲染分母不再增长

	// 按判定结果分类（同时用作运行时状态显示）
	run     int
	lost    int
	error   int
	timeout int
	other   int // 裸 HTTP 码（404/403/526…）
	ignore  int // 不写库（工具链故障 + 站点超时）
}

func (s *StageStats) incFetched()  { s.bump(&s.fetched) }
func (s *StageStats) incJudged()   { s.bump(&s.judged) }
func (s *StageStats) incRendered() { s.bump(&s.rendered) }
func (s *StageStats) incEscalate() { s.bump(&s.escalate) }

// Record 按最终判定归类计数。doneCh 收口前对每个站点调用一次。
func (s *StageStats) Record(v Verdict) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case v.InfraErr != "" || v.NavTimeout != "":
		s.ignore++
	default:
		switch string(v.Status) {
		case "RUN":
			s.run++
		case "LOST":
			s.lost++
		case "ERROR":
			s.error++
		case "TIMEOUT":
			s.timeout++
		default:
			s.other++
		}
	}
}
func (s *StageStats) bump(p *int) {
	s.mu.Lock()
	*p++
	s.mu.Unlock()
}

// Snapshot 返回一份一致的三阶段快照。
// 渲染阶段的分母（escalate）是动态的：判定多少站要升级，分母就涨到多少。
func (s *StageStats) Snapshot() (fetched, judged, rendered, escalate int, finished bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetched, s.judged, s.rendered, s.escalate, s.finished
}

// VerdictCounts 返回按判定分类的实时计数。
func (s *StageStats) VerdictCounts() (run, lost, errc, timeout, other, ignore int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run, s.lost, s.error, s.timeout, s.other, s.ignore
}

// Done 一次性把三阶段计数推进到指定值。
// 供外部（测试、进度组件）构造已完成状态，避免逐项自增。
func (s *StageStats) Done(fetched, judged, rendered, escalate int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetched, s.judged, s.rendered, s.escalate = fetched, judged, rendered, escalate
}

// finish 标记管线结束，之后渲染分母固定（用于算最终百分比）。
func (s *StageStats) finish() {
	s.mu.Lock()
	s.finished = true
	s.mu.Unlock()
}

// Site 是从清单（数据库或 API）读到的一条待巡记录，作为 Fetcher 的输入。
type Site struct {
	ID   int
	Name string
	URL  string
}

// Run 执行单次全量巡查（不含写库）。返回内存 verdict 列表。
// 拓扑：Fetcher 并发抓取 → Judge 单点判定/路由 → Renderer 并发渲染 → Judge 终判 → verdict。
// 反压：Fetcher→Judge 队列 cap=FetchQueue；Judge→Renderer 队列 cap=BrowserWorkers+2（不装 body）。
func Run(ctx context.Context, sites []Site, opts Options) []Verdict {
	if opts.HTTPWorkers <= 0 {
		opts.HTTPWorkers = 20
	}
	if opts.BrowserWorkers <= 0 {
		opts.BrowserWorkers = 4
	}
	if opts.FetchQueue <= 0 {
		opts.FetchQueue = 8
	}
	if opts.TimeoutMS <= 0 {
		opts.TimeoutMS = 15000
	}
	if opts.JSSettleMS <= 0 {
		opts.JSSettleMS = 8000
	}
	if opts.CDPURL == "" {
		opts.CDPURL = "ws://127.0.0.1:9222/devtools/browser"
	}

	if opts.Limit > 0 && len(sites) > opts.Limit {
		sites = sites[:opts.Limit]
	}

	verdicts := make([]Verdict, 0, len(sites))
	var mu sync.Mutex
	add := func(v Verdict) {
		mu.Lock()
		verdicts = append(verdicts, v)
		mu.Unlock()
		if opts.Stats != nil {
			opts.Stats.incJudged()
			opts.Stats.Record(v) // 按判定分类，供运行时状态显示
		}
	}

	// —— Fetcher：无脑抓，发到 fetchCh（不判定）。Judge 单 goroutine 收口。 ——
	fetchCh := make(chan Incoming, opts.FetchQueue)

	var fwg sync.WaitGroup
	var next atomic.Int32
	for i := 0; i < opts.HTTPWorkers; i++ {
		fwg.Add(1)
		go func() {
			defer fwg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				idx := int(next.Add(1) - 1)
				if idx >= len(sites) {
					return
				}
				s := sites[idx]
				t := time.Now()
				p := crawl.Raw(ctx, s.URL, ms(opts.TimeoutMS))
				if opts.Stats != nil {
					opts.Stats.incFetched()
				}
				pkglog.Debug().Int("id", s.ID).Str("url", s.URL).
					Int("status", p.Status).Str("err", p.FetchErr).
					Int64("ms", time.Since(t).Milliseconds()).Msg("抓取")

				// 中断后 Judge 已退出，这里必须能走掉，否则抓取 worker 永久阻塞。
				select {
				case fetchCh <- Incoming{Site: s, Page: p}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() { fwg.Wait(); close(fetchCh) }()

	// —— Judge：单 goroutine，唯一判定 + 升级路由 + 防环 ——
	//
	// 重试策略：渲染失败的条目由 Judge 统一决策（它知道重试预算）。
	// 三条通道的生命周期用「待办计数」收口：
	//   todo = 尚未产出最终结论的站点数
	//   每收到一条 fetch 结果 +1（若需渲染）；渲染完成或直接判定后 -1
	//   归零即可退出。这样 raw/both/js 三种模式都能正确收口。
	renderedCh := make(chan Incoming, opts.RenderQueue())
	retryCh := make(chan Incoming, opts.RenderQueue())
	renderCh := make(chan Incoming, opts.RenderQueue())

	rawOnly := opts.Stage == "raw"
	forceJS := opts.Stage == "js"

	var jwg sync.WaitGroup     // Judge 主循环
	var finalWG sync.WaitGroup // 终判消费循环

	// 收口基准：已产出最终结论的站点数，达到站点总数即结束。
	// 用计数而非"待办归零"，三种 stage 模式都能正确收敛
	// （待办归零在 raw 模式会死锁：渲染池没启动，待办恒为 0）。
	var doneMu sync.Mutex
	finished := 0
	doneCh := make(chan struct{})
	markDone := func() {
		doneMu.Lock()
		finished++
		n := finished
		doneMu.Unlock()
		if n == len(sites) {
			close(doneCh)
		}
	}

	// Judge 主循环
	//
	// 职责只有两个：判定 + 路由。它不保存"待渲染"状态——
	// 判完就把条目投给 Renderer，立刻回到消费 fetchCh。
	//
	// 投递用 pending 这个**有界**通道做缓冲（容量 = 渲染队列 + worker 数），
	// 而不是无界切片：无界会让内存随站点数无限增长，也失去反压。
	//
	// pending 满了怎么办？让 select 的其他分支继续跑（含 fetchCh），
	// 只有真正闲下来时才等 pending 空出位置——这样"等渲染时继续抓取"，
	// 又不会把内存撑爆。
	pending := make(chan Incoming, opts.RenderQueue()+opts.BrowserWorkers)

	// pending → renderCh 的搬运工。单独 goroutine，避免 Judge 卡在两次 send 上。
	var pumpWG sync.WaitGroup
	if !rawOnly {
		pumpWG.Add(1)
		go func() {
			defer pumpWG.Done()
			// 同样是中断可退：Judge 退出后 renderer 可能已不再消费。
			for in := range pending {
				select {
				case renderCh <- in:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// route 把条目交给渲染；raw 模式不该调用它。
	// 用 select 保证 doneCh 也能打断，避免收尾时卡死。
	// pending 是**有界**的：满了就在这里等，不像无界切片那样吃掉内存。
	route := func(in Incoming) bool {
		select {
		case pending <- in:
			return true
		case <-doneCh:
			return false
		case <-ctx.Done():
			return false
		}
	}

	// —— Renderer 池 + 终判循环 ——
	if !rawOnly {
		var rwg sync.WaitGroup
		for i := 0; i < opts.BrowserWorkers; i++ {
			rwg.Add(1)
			go func() {
				defer rwg.Done()
				for in := range renderCh {
					t := time.Now()
					// 传 hit：页面里出现有效入口就立刻返回，不空等满 settle。
					p2, err := crawl.Rendered(ctx, opts.CDPURL, in.Page.FinalURL, ms(opts.JSSettleMS),
						func(pp check.Page) bool { _, ok := check.MatchTravellings(pp); return ok })
					errStr := ""
					if err != nil {
						errStr = err.Error()
					}
					pkglog.Debug().Int("id", in.Site.ID).Str("url", in.Page.FinalURL).
						Int("status", p2.Status).Str("err", errStr).Int("attempt", in.Attempt).
						Int64("ms", time.Since(t).Milliseconds()).Msg("渲染")
					// 这里不计 rendered：重试会让同一站渲染多次，按次数计会超过分母。
					if err != nil {
						// 中断后 Judge 可能已退出：发送必须可放弃，否则 worker 钉死在这里，
						// rwg.Wait 永不返回 → renderedCh 永不关闭 → 收口死锁。
						select {
						case retryCh <- Incoming{Site: in.Site, Attempt: in.Attempt, Err: errStr}:
						case <-ctx.Done():
							return
						}
						continue
					}
					select {
					case renderedCh <- Incoming{Site: in.Site, Page: p2}:
					case <-ctx.Done():
						return
					}
				}
			}()
		}

		finalWG.Add(1)
		go func() {
			defer finalWG.Done()
			for in := range renderedCh {
				add(evalRendered(in))
				if opts.Stats != nil {
					opts.Stats.incRendered() // 每站最终结论只计一次
				}
				markDone()
			}
		}()

		go func() {
			rwg.Wait()
			close(renderedCh)
		}()
	}

	jwg.Add(1)
	go func() {
		defer jwg.Done()
		for {
			select {
			case in, ok := <-fetchCh:
				if !ok {
					fetchCh = nil
					continue
				}
				if rawOnly {
					v := check.Evaluate(in.Page, check.Raw)
					add(Verdict{SiteID: in.Site.ID, Name: in.Site.Name, URL: in.Site.URL, Status: v.Verdict,
						Stage: check.Raw, Evidence: v.Evidence})
					markDone()
					continue
				}
				if forceJS {
					if opts.Stats != nil {
						opts.Stats.incEscalate()
					}
					if !route(Incoming{Site: in.Site, Page: check.Page{FinalURL: in.Site.URL}}) {
						return
					}
					continue
				}
				v := check.Evaluate(in.Page, check.Raw)
				pkglog.Debug().Int("id", in.Site.ID).Str("verdict", string(v.Verdict)).
					Bool("escalate", v.Escalate).Str("evidence", v.Evidence).Msg("raw 判定")
				if v.Escalate {
					if opts.Stats != nil {
						opts.Stats.incEscalate()
					}
					// 只传 URL：整页 HTML 已判定完毕，不必再搬一次
					if !route(Incoming{Site: in.Site, Page: check.Page{FinalURL: in.Page.FinalURL}}) {
						return
					}
				} else {
					add(Verdict{SiteID: in.Site.ID, Name: in.Site.Name, URL: in.Site.URL, Status: v.Verdict,
						Stage: check.Raw, Evidence: v.Evidence})
					markDone()
				}

			case r := <-retryCh:
				canRetry := opts.RenderRetries > 0 && r.Attempt < opts.RenderRetries &&
					!check.TerminalRenderErr(r.Err)
				if canRetry {
					pkglog.Debug().Int("id", r.Site.ID).Int("attempt", r.Attempt).
						Str("err", r.Err).Msg("渲染失败，重新入队")
					if !route(r.withNextAttempt()) {
						return
					}
					continue
				}
				if check.IsSiteNavTimeout(r.Err) {
					add(Verdict{SiteID: r.Site.ID, URL: r.Site.URL, Status: check.Timeout,
						Stage: check.JS, Evidence: "nav timeout: " + r.Err, NavTimeout: r.Err})
				} else {
					v := check.Evaluate(check.Page{FetchErr: r.Err}, check.JS)
					add(Verdict{SiteID: r.Site.ID, URL: r.Site.URL, Status: v.Verdict,
						Stage: check.JS, Evidence: "infra js-leg: " + r.Err, InfraErr: r.Err})
				}
				if opts.Stats != nil {
					opts.Stats.incRendered()
				}
				markDone()

			case <-doneCh:
				return

			// 中断：不再等剩余站点出结论，直接收口。
			// 手头已有多少条就返回多少条——中断后的报告要的是已查完的部分。
			case <-ctx.Done():
				return
			}
		}
	}()

	// 收口顺序很关键，顺序错了就会有人永远等在 range 上（实测踩过）：
	//   1. 等 Judge 退出（已收到全部站点的最终结论）
	//   2. 关 pending → pump 退出 → 关 renderCh → Renderer 池退出
	//   3. 关 renderedCh → 终判循环退出
	//   4. 再等所有 goroutine
	jwg.Wait()
	if !rawOnly {
		close(pending)
		pumpWG.Wait()
		close(renderCh) // Renderer 池退出后会自己关 renderedCh（见上）
		finalWG.Wait()
	}
	if opts.Stats != nil {
		opts.Stats.finish()
	}
	return verdicts
}

// Incoming 是一站数据在管线内的载体；Page 可含大 body，只在 fetchCh（FetchQueue 有界）
// 流动；renderCh 流动的是升级集，不装大 body。
type Incoming struct {
	Site Site
	Page check.Page
	// InfraErr 非空表示**我们自己的工具链**故障（CDP 连不上、浏览器崩溃）。
	InfraErr string
	// NavTimeout 非空表示**站点**在导航预算内没加载完（页面太重/服务端太慢）。
	// 与 InfraErr 分开，因为责任方不同，报告措辞也必须不同。
	NavTimeout string
	// Attempt 记录已尝试渲染的次数（0 表示首次）。
	Attempt int
	// Err 是渲染失败的原因，仅在重试通道使用。
	Err string
}

// withNextAttempt 返回一次重试请求：站点不变，尝试次数 +1。
func (in Incoming) withNextAttempt() Incoming {
	in.Attempt++
	in.Page = check.Page{FinalURL: in.Site.URL} // 重试只保留 URL，大 body 不重带
	return in
}

func evalRendered(in Incoming) Verdict {
	// 站点超时：判 TIMEOUT，措辞用 "nav timeout" 而非 "infra"——
	// 责任方是站点（页面太重/服务端太慢），不是我们的工具链。
	if in.NavTimeout != "" {
		return Verdict{
			SiteID: in.Site.ID, Name: in.Site.Name, URL: in.Site.URL,
			Status: check.Timeout, Stage: check.JS,
			Evidence:   "nav timeout: " + in.NavTimeout,
			NavTimeout: in.NavTimeout,
		}
	}
	// 工具链故障：仍交给 Evaluate 判语汇（超时→TIMEOUT、其余→ERROR），
	// 这样状态字符串与线上语言一致；同时打 InfraErr 标记，写库环节会跳过。
	if in.InfraErr != "" {
		v := check.Evaluate(check.Page{FetchErr: in.InfraErr}, check.JS)
		return Verdict{
			SiteID: in.Site.ID, Name: in.Site.Name, URL: in.Site.URL,
			Status: v.Verdict, Stage: check.JS,
			Evidence: "infra js-leg: " + in.InfraErr,
			InfraErr: in.InfraErr,
		}
	}
	v := check.Evaluate(in.Page, check.JS)
	return Verdict{SiteID: in.Site.ID, Name: in.Site.Name, URL: in.Site.URL, Status: v.Verdict, Stage: check.JS, Evidence: v.Evidence}
}

// RenderQueue 返回 Judge→Renderer 队列容量。
//
// 与 fetchCh 的关键区别：这里的 Incoming **只带 URL**，整页 HTML 已在判定后丢弃。
// 单条实测约 130 字节；而 fetchCh 每条最多 2MB HTML。差 15000 倍。
//
// 所以这个队列可以放得很宽：容量 = workers × 20，
// 4 个 worker → 80 条 ≈ 10KB，完全可忽略。
//
// 放宽的收益：Judge 判完一批站后不必为"渲染池还在忙"而停顿，
// 能把待渲染任务一次性交出去，立刻回头抓取——这正是吞吐瓶颈所在。
func (o Options) RenderQueue() int {
	q := o.BrowserWorkers * 20
	if q < 16 {
		q = 16 // 小并发时给个下限，避免队列过窄导致频繁停顿
	}
	return q
}

func ms(x int) time.Duration { return time.Duration(x) * time.Millisecond }

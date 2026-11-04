package crawl

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"travellings-bot-go/check"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
	"github.com/rs/zerolog"
)

// setupTimeout 限制「建页 + 初始设置」这几步 CDP 往返的总耗时。
//
// 取 20s 而非更短：AttachToTarget 之后有 8 个 enable 往返（log/network/
// inspector/page/dom/css/SetDiscoverTargets/SetAutoAttach），
// 慢机器上每个几十毫秒，叠加起来仍需要数秒余量。
// 一旦超过这个值，说明对端已经不回包，继续等只会把整轮巡查钉死。
const setupTimeout = 20 * time.Second

// BrowserTaskBudget 返回 crawl.Rendered 处理**单个任务**的最坏内部预算：
//
//	建页 10s + 初始设置 20s + 导航 settle+35s + 关页/收尾 ~10s = settle + 75s
//
// master 的 Browser 租约必须以此为依据——旧公式把单机「worker 本地重试预算」
// （×(1+render-retries)）错误继承进来，且未覆盖建页/关页段，默认参数下
// （settle 8s）租约只有 79s < 最坏预算 83s，慢站必被误回收（树莓派实测
// reclaimed=1 每 30/60s 一次即此）。D14 铁律后 worker 从不本地重试，乘数本就无主。
// 改参数时请同步 Rendered 内各 WithTimeout 行（createCtx/setupCtx/navCtx）。
func BrowserTaskBudget(settle time.Duration) time.Duration { return settle + 75*time.Second }

// forceCloseTab 兜底关闭标签页，补 chromedp 关闭路径的两处静默失败。
//
// chromedp 的 Cancel 只做两件事，且两处都可能静默放弃：
//  1. `if tgt := c.Target; tgt == nil { return }` —— 建了 target 但 attach
//     中途失败时 c.Target 仍为 nil（chromedp.go:513 先赋 c.targetID、
//     514 才 attach），Cancel 直接返回，标签页永久残留。
//  2. CloseTarget 之后只用 500ms 等它从列表中消失，超时被
//     `errors.Is(err, context.DeadlineExceeded)` 显式忽略（chromedp.go:289）。
//     页面里有永不结束的请求时，标签页会留下来。
//
// 所以调用方在 Cancel **之前**把 target id 读出来传进来，这里直接补一发
// CloseTarget。兜底逻辑不该把主流程搞崩：取不到 id 就静默跳过。
func forceCloseTab(ctx context.Context, tid target.ID) {
	c := chromedp.FromContext(ctx)
	if c == nil || c.Browser == nil {
		return
	}
	// 这个函数在 Cancel **之后**调用，此时 c.Target 可能已被清空，
	// 所以必须由调用方在 Cancel 之前把 TargetID 取出来传进来。
	if tid == "" {
		return
	}
	qctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cdp.Call(qctx, c.Browser, target.CloseTarget,
		target.CloseTargetParams{TargetID: tid}); err != nil {
		log.Debug().Str("target", string(tid)).Err(err).Msg("兜底关闭标签页失败")
	}
}

// newTabToken 生成建页标记里的唯一串。
// 用时间戳 + 原子自增即可：它只需要在**进程内**唯一，
// 目的是把「我这条 CreateTarget 建出来的页」从别的页里认出来，
// 不需要密码学强度，也就不必引入额外依赖。
var tabSeq atomic.Uint64

func newTabToken() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" +
		strconv.FormatUint(tabSeq.Add(1), 36)
}

// reapOrphanTab 在 CreateTarget 失败后，按初始 URL 标记反查并关掉孤儿页。
//
// 场景：命令发出去、对端建了页，但回包在超时前没回来。我们没有 id，
// 若不处理，这个页会一直挂着（正是 tabs 超过 workers+1 的又一条来路）。
// 拿不到清单或没命中就静默返回——兜底逻辑不该把主流程搞崩。
func reapOrphanTab(br *chromedp.Browser, marker string) {
	qctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	list, err := cdp.Call(qctx, br, target.GetTargets, target.GetTargetsParams{})
	if err != nil {
		log.Debug().Err(err).Msg("兜底清理：列举标签页失败")
		return
	}
	for _, ti := range list.TargetInfos {
		if ti.URL != marker {
			continue
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, cerr := cdp.Call(cctx, br, target.CloseTarget,
			target.CloseTargetParams{TargetID: ti.TargetID})
		ccancel()
		if cerr != nil {
			log.Debug().Str("target", string(ti.TargetID)).Err(cerr).Msg("兜底清理孤儿页失败")
		}
	}
}

// Rendered 执行渲染阶段：attach 已在运行的 CDP 实例（Obscura，ws URL 含
// /devtools/browser 时不再做 /json/version 发现），取 JS 执行后的 DOM。
// links 用 a.href IDL 属性——浏览器替我们绝对化（2026-10-31 实测），
// 这修正了抓取阶段正则提取够不到 JS 挂载内容的问题。
// hit 非 nil 时命中即返，不空等 settle——SPA 站的主要提速手段
// （实测 Navigate 返回远早于内容挂载：27ms vs 700ms）。
func Rendered(ctx context.Context, cdpURL, urlstr string, settle time.Duration, hit func(check.Page) bool) (check.Page, error) {
	// 同样补全 scheme：API 里有裸域名记录（如 blog.joker2yue.com），
	// 直接喂给浏览器会报 "Invalid URL: relative URL without a base"。
	urlstr = normalizeURL(urlstr)
	p := check.Page{FinalURL: urlstr}
	// 从连接池租一条 CDP 连接，用完归还。
	//
	// 不能每站新建连接：反复建立-拆除 WebSocket 会让浏览器侧内存只涨不降
	// （实测 Obscura 涨到 1.9GB）。这点没变，变的只是「一条」成了「N 条」——
	// 为什么必须多于一条，见 connPool 的注释。
	cx, attachErr := acquireConn(ctx, cdpURL)
	if attachErr != nil {
		return p, fmt.Errorf("open browser: %w", attachErr)
	}
	defer releaseConn(cx)
	activeCtx := cx.ctx
	// 每个站点在共享 Browser 上开一个标签页。
	//
	// 清理必须**等标签页真正关闭**再返回：chromedp 的 defer cancel() 只发取消信号，
	// 不等待清理完成；紧接着的下一站会撞上正在拆除的 target，
	// 表现为 "executor doesn't exist" 的伪错误（2026-10-31 实测过的噪声源）。
	// 显式 Cancel 会等待资源释放，代价是每条渲染多几毫秒，换来标签页不堆积。
	// 父 context 已有 Browser，此时不能再传 BrowserOption（WithErrorf 就是），
	// 否则 chromedp 会 panic："WithBrowserOption can only be used when allocating a new browser"。
	// 拆除时的 executor 噪声改由日志层面过滤，不在这里压制。
	//
	// 父级是 activeCtx（已持有 Browser）→ 本调用只开新标签页、复用同一条连接。
	// 标签页由**我们自己**建，id 从这一刻起就在手里。
	//
	// 不能用 chromedp 的默认路径（NewContext 后由 Do 内部 CreateTarget）：
	// 那条路上 id 存在 c.targetID 这个未导出字段里，attach 成功前 c.Target
	// 还是 nil。一旦 attach 失败或超时，chromedp 的 Cancel 走
	// `if tgt == nil { return }` 直接返回（chromedp.go:250），
	// 标签页就此永久残留——这正是 tabs 能超过 workers+1 的路径。
	// 自己建页后，无论后面哪一步失败，defer 都能拿 id 精确关闭。
	base := chromedp.FromContext(activeCtx)
	if base == nil || base.Browser == nil {
		return p, fmt.Errorf("open browser: no browser in cdp conn")
	}
	// 初始 URL 里塞一个唯一标记，用来兜住一个很实在的坑：
	// CreateTarget 超时的时候**服务端可能已经建好了页**，只是回包没赶上，
	// 我们手里没有 id，就成了谁也认不出的孤儿页。32 并发实测里这个错误
	// 出现过 91 次，不处理就等于换个地方漏。带上标记后可以按 url 反查并关掉。
	marker := "data:text/html,<title>travellings-tab-" + newTabToken() + "</title>"
	newWin := true // 与 chromedp 默认一致：非首 context 每页独立窗口
	createCtx, cancelCreate := context.WithTimeout(ctx, 10*time.Second)
	created, createErr := cdp.Call(createCtx, base.Browser, target.CreateTarget,
		target.CreateTargetParams{URL: marker, NewWindow: &newWin})
	cancelCreate()
	if createErr != nil {
		// 超时也要扫一遍：命中的就是这条命令建出来的孤儿页。
		reapOrphanTab(base.Browser, marker)
		return p, fmt.Errorf("create tab: %w", createErr)
	}
	tabID := created.TargetID

	tctx, cancelT := chromedp.NewContext(activeCtx, chromedp.WithTargetID(tabID))
	defer func() {
		// 顺序很关键：先精确关闭，再让 chromedp 收尾。
		//
		// chromedp 的 Cancel 把 DetachFromTarget 与 CloseTarget 放在**同一个**
		// 5s 预算里（chromedp.go:261 建 ctx，264 用在 detach，275 又用在 close）。
		// 32 并发下 DetachFromTarget 一旦超时就把预算吃光，紧接着的 CloseTarget
		// 拿到已过期的 context 必然失败（chromedp.go:275-278），标签页就留下了。
		// Obscura 侧同时打出 `INTERCEPTION: deferred queue full (256)`，正是拥塞证据。
		// 先关掉之后，Cancel 面对的已是不存在的 target，detach 会立刻报错
		// 而不是耗满 5s，worker 也不会被钉住。
		forceCloseTab(activeCtx, tabID)
		_ = chromedp.Cancel(tctx) // 再等 chromedp 内部 goroutine 收尾
		cancelT()
	}()

	// 建页那几步必须自带超时，否则会**永久**阻塞。
	//
	// chromedp.Do 内部要串行做 CreateTarget → AttachToTarget → 8 个 enable 往返，
	// 每一步都是同步等对端回包。Obscura 在 32 并发下偶发拥塞不回包时，
	// 整条链路就永远停在这里：既不返回也不触发 defer 的 Cancel，
	// 于是标签页永不关闭（tabs 涨到 43、48 这类超过 workers+1 的值），
	// 32 个 worker 全被钉死后进程 CPU 归零、整轮巡查挂死不结束。
	// 2026-11-01 实测：进程跑了 20 分钟 CPU 只累积 12.6s 且不再增长，
	// netstat 显示 CDP 连接仍在 ESTABLISHED。
	//
	// 只对**建页与初始设置**限时；navCtx 另有一套（settle+35s），
	// 两者预算独立，互不挤占。
	setupCtx, cancelSetup := context.WithTimeout(tctx, setupTimeout)
	defer cancelSetup()
	// 中断传播：tctx 挂在进程级 sharedCtx 上，不含 pipeline 的取消信号。
	// 不接这一根线，用户按下 ^C 之后在途渲染还要跑满 20s/35s 才回来。
	stopSetup := context.AfterFunc(ctx, cancelSetup)
	defer stopSetup()

	if err := chromedp.Do(setupCtx); err != nil {
		return p, fmt.Errorf("start browser: %w", err)
	}
	if _, err := chromedp.Call(setupCtx, emulation.SetUserAgentOverride,
		emulation.SetUserAgentOverrideParams{UserAgent: BotUA}); err != nil {
		return p, fmt.Errorf("set ua: %w", err)
	}
	if _, err := chromedp.Call(setupCtx, network.SetExtraHTTPHeaders,
		network.SetExtraHTTPHeadersParams{Headers: network.Headers{"Referer": Referer}}); err != nil {
		return p, fmt.Errorf("set referer: %w", err)
	}

	navCtx, cancelNav := context.WithTimeout(tctx, settle+35*time.Second)
	defer cancelNav()
	// 同上：中断时立刻结束轮询，不再空等 settle。
	stopNav := context.AfterFunc(ctx, cancelNav)
	defer stopNav()

	// 等页面 load 完成再判定：这是**刻意**的。
	// 若不等待，SPA 站会在内容挂载前就被判成 LOST——把好站误判成坏站，
	// 这个代价远高于多花几秒（误判会写进线上 status，影响 API 与其他成员站）。
	// 状态码语义保留：403 挑战页必须记 403，WAIT 过滤与 API 展示依赖它。
	resp, err := chromedp.Run(navCtx, chromedp.NavigateResponse(urlstr))
	if err != nil {
		return p, fmt.Errorf("navigate: %w", err)
	}
	p.Status = 200
	if resp != nil {
		p.Status = int(resp.Status)
	}

	// 轮询只取 links + 当前 URL：这两者便宜，且能覆盖绝大多数判定。
	// 整页 HTML 最贵（可能几百 KB），只有 links 全部落空、需要走 specialCases
	// 签名匹配时才取一次——而 MatchTravellings 本身也是先 Links 后 HTML。
	deadline := time.After(settle)
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()

	// oneShot 一次往返同时取 links 与 location，避免两次独立 CDP 往返。
	type snapshot struct {
		Links []string `json:"links"`
		Loc   string   `json:"loc"`
	}
	const snapJS = `({links: [...document.querySelectorAll('a[href]')].map(a=>a.href), loc: location.href})`

	for {
		snap, errS := chromedp.Run(navCtx, chromedp.Evaluate[snapshot](snapJS))
		if errS == nil {
			p.Links = snap.Links
			if snap.Loc != "" {
				p.FinalURL = snap.Loc
			}
			// 命中即返；此处不需要 HTML，省掉最贵的那次往返。
			if hit != nil && hit(p) {
				return p, nil
			}
		}
		select {
		case <-deadline:
			// 轮询结束仍未命中：补取一次 HTML，让 specialCases 签名匹配能生效，
			// 同时把页面内容留作证据（failedReason）。
			if html, errH := chromedp.Run(navCtx,
				chromedp.Evaluate[string]("document.documentElement.outerHTML")); errH == nil {
				p.HTML = html
			}
			return p, nil
		case <-tick.C:
		case <-navCtx.Done():
			return p, navCtx.Err()
		}
	}
}

// log 由 SetLogger 注入；crawl 包自身不配置日志输出。
var log = zerolog.Nop()

// SetLogger 允许上层把配置好的 logger 传进来。
func SetLogger(l zerolog.Logger) { log = l }

// 远程 CDP 的可用性只需判定一次。
//
// 首站渲染时探活：连得上就一直用远程，连不上就整轮切本地。
// 不能每站都探一次——那等于每条渲染多一次完整 CDP 握手，
// 全量上千站会白花几分钟。用 sync.Once 保证只判定一次。
var (
	probeOnce sync.Once
	useRemote bool // 探测结论：远程是否可用
)

// openBrowser 优先连 cdpURL 指定的外部浏览器；连不上则回退到本地自启的无头 Chrome。
//
// 加回退的理由：CI/临时机器上没起 Obscura 时，此前是整轮巡查全判 infra 失败，
// 一条可信数据都产不出来。有了回退，至少还能跑出结果。
func openBrowser(ctx context.Context, cdpURL string) (context.Context, context.CancelFunc, error) {
	if cdpURL != "" {
		probeOnce.Do(func() {
			err := probeRemote(ctx, cdpURL)
			useRemote = err == nil
			if err != nil {
				log.Warn().Str("cdp", cdpURL).Err(err).
					Msg("CDP 连接失败，回退到本地无头 Chrome")
			}
		})
		if useRemote {
			a, c := remote.NewAllocator(ctx, cdpURL, remote.NoModifyURL)
			return a, c, nil
		}
	}
	return localAllocator(ctx)
}

// probeRemote 试连一次，仅用于判定远程是否可用。
func probeRemote(ctx context.Context, cdpURL string) (err error) {
	// 探活本身要有超时：连不上的机器可能一直 hang 住，不能拖死首站。
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	allocCtx, cancelAlloc := remote.NewAllocator(pctx, cdpURL, remote.NoModifyURL)
	c, cancelC := chromedp.NewContext(allocCtx,
		chromedp.WithErrorf(func(string, ...any) {}))
	defer func() {
		// 必须显式 Cancel 并等待：defer cancelC() 只发信号，
		// 探活建出来的 Browser/标签页可能来不及回收就进入主流程，
		// 这些残留会一直挂在浏览器上（内存只涨不降的元凶之一）。
		_ = chromedp.Cancel(c)
		cancelC()
		cancelAlloc()
	}()
	return chromedp.Do(c)
}

// localAllocator 用 chromedp 默认的 ExecAllocator 启动本地无头浏览器。
// 需要在 PATH 或标准安装路径上能找到 Chrome/Chromium。
func localAllocator(ctx context.Context) (context.Context, context.CancelFunc, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("headless", true),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	return allocCtx, cancel, nil
}

// 主连接的调试注入点。
//
// sharedCtx 指向连接池里第一条建立的连接，供 TabCount / SweepTabs 这类
// 「问一下浏览器现在什么状态」的调用方使用；池关闭后置 nil。
// 测试会直接写它来模拟 CDP 不可用（sharedCtx 非 nil 但里面没有 Browser），
// 所以名字和锁都保留——换成别的形式那些测试就编译不过了。
var (
	sharedMu  sync.Mutex
	sharedCtx context.Context
)

// setSharedConn 记录主连接（仅第一条）。
func setSharedConn(ctx context.Context) {
	sharedMu.Lock()
	if sharedCtx == nil {
		sharedCtx = ctx
	}
	sharedMu.Unlock()
}

// queryConns 返回本次要查询/对账的连接集合。
//
// 优先用池里的真实连接；池还没建起来时退回 sharedCtx——测试靠这个
// 注入一个没有 Browser 的上下文来模拟 CDP 不可用。
func queryConns() []context.Context {
	ents := poolConns()
	if len(ents) > 0 {
		out := make([]context.Context, 0, len(ents))
		for _, e := range ents {
			out = append(out, e.ctx)
		}
		return out
	}
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedCtx != nil {
		return []context.Context{sharedCtx}
	}
	return nil
}

// CloseBrowser 释放池里全部连接（进程退出或每轮巡查结束时调用）。
//
// 用 Cancel 而非直接 cancelCtx：Cancel 会等待标签页与连接真正拆除，
// 直接取消只是发信号，进程退出时可能留下未回收的浏览器侧资源。
func CloseBrowser() {
	poolMu.Lock()
	p := pool
	pool = nil
	poolMu.Unlock()
	if p != nil {
		p.close()
	}
	sharedMu.Lock()
	sharedCtx = nil
	sharedMu.Unlock()
}

// SweepTabs 关闭每条连接上除首标签页外的所有 page 标签页。
//
// 这是一轮巡查结束后的**对账**，不是主清理路径：正常情况下每个 Rendered
// 的 defer 已经关掉了自己的页。但在拥塞时关闭命令本身也会失败——
// chromedp 把 DetachFromTarget 与 CloseTarget 放在同一个 5s 预算里
// （chromedp.go:261/264/275），detach 一旦超时，close 拿到过期 context
// 必然失败；Obscura 过载时还会直接回 INTERCEPTION: deferred queue full。
// 这些失败都被静默吞掉，只有把「一轮下来还剩几个页」摆出来才看得见。
//
// 放在 pipeline.Run 返回之后调用：此时所有 worker 已退出，不会再有人建页。
// 逐条连接串行处理——对账是收尾动作，不值得为它再开一堆并发。
func SweepTabs() int {
	closed := 0
	for _, ctx := range queryConns() {
		closed += sweepConn(ctx)
	}
	if closed > 0 {
		log.Warn().Int("closed", closed).Msg("收尾对账：上一轮有标签页未正常关闭")
	}
	return closed
}

func sweepConn(ctx context.Context) int {
	base := chromedp.FromContext(ctx)
	if base == nil || base.Browser == nil {
		return 0
	}
	// 每条连接自己的那页是浏览器的第一页，关掉会连带毁掉整条连接。
	var keep target.ID
	if base.Target != nil {
		keep = base.Target.TargetID
	}

	qctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	list, err := cdp.Call(qctx, base.Browser, target.GetTargets, target.GetTargetsParams{})
	cancel()
	if err != nil {
		log.Debug().Err(err).Msg("收尾对账：列举标签页失败")
		return 0
	}

	closed := 0
	for _, ti := range list.TargetInfos {
		if ti.Type != "page" || ti.TargetID == keep {
			continue
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, cerr := cdp.Call(cctx, base.Browser, target.CloseTarget,
			target.CloseTargetParams{TargetID: ti.TargetID})
		ccancel()
		if cerr != nil {
			log.Debug().Str("target", string(ti.TargetID)).Err(cerr).Msg("收尾对账：关闭残留标签页失败")
			continue
		}
		closed++
	}
	return closed
}

// TabCount 返回所有连接上「正在渲染」的标签页数（用于运行时健康显示）。
//
// 已排除每条连接自带的那个常驻首页：它是浏览器的第一页，不是我们为某个站点
// 开的页。不减掉它，32 条连接空闲时就显示 tabs 32，一开渲染就冲破
// --browser-workers——那不是泄漏，是计数口径把地板当成了天花板。
//
// 我们主动管理标签页：每个站点开一个、用完 Cancel 关掉。
// 若这个数字持续上涨，说明清理有漏，浏览器内存会越堆越高——
// 这就是之前踩过的坑，摆到状态行上就能盯着。
//
// 走已建立的 CDP 会话列举 target，而不是按 ws 地址拼 HTTP 端点查 /json/list：
// 后者只对外部 CDP（Obscura）成立，回退到本地自启的无头 Chrome 时端口是
// chromedp 随机分派的、拿不到可拼的地址，数字会恒为 -1（状态行整块消失）。
//
// 多条连接时并行问、共享 2s 总预算：进度条每 120ms 就要一次答案，
// 串行问 32 条连接在拥塞时能卡到分钟级，等于把状态行变成卡顿源。
//
// lastTabCount 缓存最近一次成功的标签页数（未建立浏览器时为 0）。
//
// 为什么需要：TabCount 是一次真实 CDP 往返，并发渲染时连接被导航/轮询占满，
// Target.getTargets 会 2 秒超时（实测复现：58 次采样 16 次 deadline exceeded，占 27%）。
// 若查询失败就返回 -1，状态行的 tabs 段会在高负载时反复消失又出现——
// 看起来像泄漏排查本身坏了。浏览器已建立过，标签页数就不会突然变成"未知"，
// 沿用上次值即可。
var lastTabCount atomic.Int64

// 查询失败沿用上次计数，不阻塞主流程。
func TabCount() int {
	conns := queryConns()
	if len(conns) == 0 {
		return int(lastTabCount.Load())
	}
	qctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
		anyOK bool
	)
	for _, ctx := range conns {
		c := chromedp.FromContext(ctx)
		if c == nil || c.Browser == nil {
			continue // 测试注入的假上下文走这里 → 沿用缓存
		}
		// 每条连接自己那页是浏览器的第一页，不算「正在渲染的标签页」。
		// 不减掉它，状态行就会恒等于连接数：开 32 条连接时空闲也显示 tabs 32，
		// 一开始渲染就冲破 --browser-workers，看起来像泄漏，其实一直是满的。
		var keep target.ID
		if c.Target != nil {
			keep = c.Target.TargetID
		}
		wg.Add(1)
		go func(br *chromedp.Browser, keep target.ID) {
			defer wg.Done()
			res, err := cdp.Call(qctx, br, target.GetTargets, target.GetTargetsParams{})
			if err != nil {
				return
			}
			// 只数页面型 target，过滤 devtools 自身等
			n := 0
			for _, t := range res.TargetInfos {
				if t.Type == "page" && t.TargetID != keep {
					n++
				}
			}
			mu.Lock()
			total += n
			anyOK = true
			mu.Unlock()
		}(c.Browser, keep)
	}
	wg.Wait()
	if !anyOK {
		return int(lastTabCount.Load())
	}
	lastTabCount.Store(int64(total))
	return total
}

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"travellings-bot-go/crawl"
	"travellings-bot-go/internal/ui"
	"travellings-bot-go/pipeline"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/lipgloss"
)

// progress 在 stderr 上画多行分段进度：抓取 / 判定 / 渲染 / 总计。
// 进度条本体用 bubbles/progress 渲染（ViewAs 直接出字符串，不跑 bubbletea 事件循环）。
//
// 设计约束：
//   - 只在 TTY 启用：非 TTY 输出控制字符会污染 CI 与管道
//   - -v 时禁用：debug 日志与多行进度同写 stderr 会互相冲乱
//   - 每帧整体重绘：上移 N-1 行回到起点再逐行清写，避免多行残影
//
// 渲染阶段的分母是**动态**的：判定出多少站要升级，分母就涨到多少。
// 因此它的百分比在跑到一半时可能回落，这是真实语义，不做平滑。
type progressView struct {
	mu        sync.Mutex
	w         io.Writer
	enabled   bool
	total     int
	stats     *pipeline.StageStats
	start     time.Time
	lastDraw  time.Time
	drawn     int
	lastFrame []string                     // 最近一帧，Suspend 后原样重画
	suspended bool                         // 正在让位给其他输出，期间禁止绘制
	bar       progress.Model               // bubbles 进度条模型
	stopped   bool                         // Finish 后禁止再画，防止 ticker 收尾竞态
	done      chan struct{}                // Start 的 goroutine 退出信号
	frameFn   func(now time.Time) []string // 帧覆盖钩子：非 nil 时 render 直接用它（master 侧复用同一组件）
}

// activeProg 指向当前正在画的进度条，供日志 writer 让位使用。
// 每轮 runOnce 注册一次，Finish 后置空。
var activeProg atomic.Pointer[progressView]

// progWriter 在写终端前擦掉进度块、写完重画。
//
// 进度块靠「上移上一帧行数」相对定位，任何一行插入都会把基准永久推低一行，
// 之后整轮都是两块进度条。让日志与进度条互斥，这个前提才成立。
type progWriter struct{ w io.Writer }

func (s progWriter) Write(b []byte) (int, error) {
	p := activeProg.Load()
	if p == nil {
		return s.w.Write(b)
	}
	p.Suspend()
	n, err := s.w.Write(b)
	p.Resume()
	return n, err
}

func newProgress(total int, stats *pipeline.StageStats, verbose bool) *progressView {
	p := &progressView{
		w:       os.Stderr,
		enabled: ui.UseColorStderr() && !verbose,
		total:   total,
		stats:   stats,
		start:   time.Now(),
	}
	// 宽度用组件默认值（defaultWidth=40），不自己定。
	p.bar = progress.New(
		progress.WithDefaultGradient(),
	)
	return p
}

// newMasterProgress 给 bot master 画三条队列进度（爬取队列/渲染队列/所有任务）。
// snap = master.Round.Snapshot：两腿各自的「累计派发/累计任务数(已派+待派)」+ 累计回传。
// 分母会随重试回灌增长——「所有任务 400/1600」这种分母远大于首轮清单是设计口径。
func newMasterProgress(snap func() (hd, ht, bd, bt, rep int), verbose bool) *progressView {
	p := &progressView{
		w:       os.Stderr,
		enabled: ui.UseColorStderr() && !verbose, // 只在 TTY 且非 -v：与 run 同一开关口径
		start:   time.Now(),
	}
	p.bar = progress.New(progress.WithDefaultGradient())
	p.frameFn = func(now time.Time) []string {
		hd, ht, bd, bt, rep := snap()
		return []string{
			ui.ColorizeTo(os.Stderr, ui.Bold, "开往巡查 · 主从") + "  " + now.Sub(p.start).Round(time.Second).String(),
			p.barLine("爬取队列", hd, ht, 8),
			p.barLine("渲染队列", bd, bt, 8),
			p.barLine("所有任务", rep, ht+bt, 8),
		}
	}
	return p
}

// Start 启动后台刷新：按固定间隔重绘，直到 stop 关闭。
// done 在 goroutine 真正退出时关闭——Finish 靠它确认「不会再有人画了」。
func (p *progressView) Start(stop <-chan struct{}) {
	p.done = make(chan struct{})
	if !p.enabled {
		close(p.done)
		return
	}
	go func() {
		defer close(p.done)
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				// 先检查是否已收尾：stop 关闭与 tick 同时就绪时 select 随机选，
				// 若选中 tick 就会在最终帧下方再画一整块——这正是「两行重复」的来源。
				if p.isStopped() {
					return
				}
				p.Tick(false)
			}
		}
	}()
}

func (p *progressView) isStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

// Tick 重绘一帧。force=true 时跳过节流（用于收尾帧）。
func (p *progressView) Tick(force bool) {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	if !force && now.Sub(p.lastDraw) < 120*time.Millisecond {
		p.mu.Unlock()
		return
	}
	p.lastDraw = now
	frame := p.render(now)
	p.mu.Unlock()
	p.paint(frame)
}

// Finish 画一帧最终值并**保留**在终端上，随后把光标移到进度区下方，
// 让后续输出（报表、日志）自然接在后面。
//
// 保留而非擦除的理由：完成瞬间的满格进度条是这一轮的可视凭证，
// 擦掉后用户只看到凭空出现的报表，反而像"没跑完就没了"。
func (p *progressView) Finish() {
	if !p.enabled {
		return
	}
	// 先立停牌：此后任何 Tick 都是 no-op。
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	p.mu.Unlock()

	// 等后台 goroutine 退出，确保它手里没有正在进行的绘制。
	if p.done != nil {
		<-p.done
	}

	// 再亲手画最终帧。render 会查标签页数（HTTP），必须在锁外做，
	// 否则网络慢时会把整帧绘制卡住、日志与之交错。
	frame := p.render(time.Now())

	p.mu.Lock()
	defer p.mu.Unlock()
	// 收尾帧必须落到终端：即使刚被 Suspend 让位过，也要补画回来。
	p.suspended = false
	p.paintLocked(frame)
	if p.drawn == 0 {
		return
	}
	// 已是最后一帧内容，光标停在最后一行行首；换行把后续输出推到下方。
	_, _ = fmt.Fprint(p.w, "\n")
	p.drawn = 0
}

// paintLocked 在已持锁的前提下刷整帧。
func (p *progressView) paintLocked(frame []string) {
	if p.suspended {
		return
	}
	p.lastFrame = frame
	prev := p.drawn
	p.drawn = len(frame)
	if prev > 0 {
		p.moveToFirst(prev)
	}
	width := ui.TerminalWidth()
	for i, line := range frame {
		// 必须保证一行不超过终端宽度：一旦自动换行，物理行数就不等于逻辑行数，
		// 上移行数随之失效，画面会错位堆叠。
		_, _ = fmt.Fprint(p.w, "\r\x1b[2K"+ui.ClipDisplay(line, width))
		if i < len(frame)-1 {
			_, _ = fmt.Fprint(p.w, "\n")
		}
	}
	_, _ = fmt.Fprint(p.w, "\r")
}

// Suspend 擦掉进度块，把这块屏幕让给其他输出（日志）。
//
// 只擦不画：随后写什么都不会落在进度块的相对定位基准上，
// 写完由 Resume 在当前位置重新锚定。
func (p *progressView) Suspend() {
	if p == nil || !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.suspended || p.drawn == 0 {
		return
	}
	p.suspended = true
	p.moveToFirst(p.drawn)
	p.drawn = 0
	// 回到块首后清到屏幕底部，整块一起消失，不留半截旧帧。
	_, _ = fmt.Fprint(p.w, "\x1b[J")
}

// Resume 在光标当前位置重画最近一帧，作为新的定位基准。
func (p *progressView) Resume() {
	if p == nil || !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || !p.suspended {
		return
	}
	p.suspended = false
	if len(p.lastFrame) == 0 {
		return
	}
	p.paintLocked(p.lastFrame)
}

// moveToFirst 把光标从「最后一行的行首」移回「第一行行首」。
// 画完 N 行后光标停在最后一行行首，所以只需上移 N-1 行。
func (p *progressView) moveToFirst(n int) {
	if n > 1 {
		_, _ = fmt.Fprintf(p.w, "\x1b[%dA", n-1)
	}
	_, _ = fmt.Fprint(p.w, "\r")
}

// paint 把整帧刷到终端：回到首行起点，逐行覆盖。
func (p *progressView) paint(frame []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paintLocked(frame)
}

func (p *progressView) render(now time.Time) []string {
	// master 侧走帧覆盖：三条队列进度复用 barLine，不重写组件（D1 复用）。
	if p.frameFn != nil {
		return p.frameFn(now)
	}
	fetched, judged, rendered, escalate, _ := p.stats.Snapshot()
	elapsed := now.Sub(p.start)

	// 结构：标题 / 总进度 / 抓取 / 判定 /（渲染）/ 状态计数
	// 总进度以"已判定数"为准——它才是真正完成的站点数。
	lines := []string{
		ui.ColorizeTo(os.Stderr, ui.Bold, "开往巡查") + "  " + elapsed.Round(time.Second).String(),
		p.stageLine("总进度", judged, p.total),
		p.stageLine("抓取", fetched, p.total),
		p.stageLine("判定", judged, p.total),
	}
	// 渲染行只在真的有升级任务时出现，空跑不占屏。
	if escalate > 0 {
		lines = append(lines, p.stageLine("渲染", rendered, escalate))
	}
	// 状态计数行：这轮有几个坏站 + 标签页数（涨了说明清理有漏）。
	lines = append(lines, p.statusLine())
	return lines
}

// statusLine 拼一行 "RUN 12 · LOST 3 · ERROR 2 · IGNORE 1   tabs 2"。
// 只显示非零项，保持简洁；全部为零时显示抓取/判定进度。
func (p *progressView) statusLine() string {
	run, lost, errc, timeout, other, ignore := p.stats.VerdictCounts()

	type kv struct {
		label string
		n     int
		style lipgloss.Style
	}
	items := []kv{
		{"RUN", run, lipgloss.NewStyle().Foreground(lipgloss.Color("10"))},
		{"LOST", lost, lipgloss.NewStyle().Foreground(lipgloss.Color("11"))},
		{"ERROR", errc, lipgloss.NewStyle().Foreground(lipgloss.Color("9"))},
		{"TIMEOUT", timeout, lipgloss.NewStyle().Foreground(lipgloss.Color("9"))},
		{"HTTP", other, lipgloss.NewStyle().Foreground(lipgloss.Color("11"))},
		{"IGNORE", ignore, lipgloss.NewStyle().Foreground(lipgloss.Color("5"))},
	}

	var parts []string
	for _, it := range items {
		if it.n == 0 {
			continue // 零项不显示，避免噪声
		}
		parts = append(parts, it.style.Render(fmt.Sprintf("%s %d", it.label, it.n)))
	}
	if len(parts) == 0 {
		parts = append(parts, ui.DimStyle.Render("等待首批判定…"))
	}

	left := "  " + strings.Join(parts, ui.DimStyle.Render(" · "))

	// 标签页数：右对齐，涨了就说明有标签页没关掉
	tabs := crawl.TabCount()
	if tabs >= 0 {
		left += "   " + ui.DimStyle.Render(fmt.Sprintf("tabs %d", tabs))
	}
	return left
}

// stageLine 画一行 "标签 [条形] 12/34 (35%)"。
// 排版用 lipgloss 的 Width()——它按显示宽度计算、正确跳过 ANSI，
// 比手写 padDisplay 更可靠，也和报表的单元格排版保持一致。
// 分母为 0 时条形留空并显示 "—"：渲染阶段在还没有升级任务前就是这个状态。
func (p *progressView) stageLine(label string, done, total int) string {
	// run 路径：标签定宽 6，与改造前逐字节一致（bot run 行为不变）。
	return p.barLine(label, done, total, 6)
}

// barLine 画一行「标签 计数 条形」。width 是标签单元格宽度：
// run 用 6（「总进度」「抓取」都是 2-3 字），master 用 8 容纳「爬取队列」这类 4 字标签。
// 抽出来只为让 master 复用同一套排版，不给 run 增加任何行为差异。
func (p *progressView) barLine(label string, done, total, width int) string {
	pct := 0.0
	if total > 0 {
		pct = float64(done) / float64(total)
	}
	if pct > 1 {
		pct = 1
	}

	// 缩进 → 标签定宽 → 计数 → 组件渲染的「条形+百分比」。
	// 百分比交给 progress 组件（PercentFormat 控制），不再自己拼。
	labelCell := stageStyle(label).Width(width).Render(label)
	counts := "—"
	if total > 0 {
		counts = fmt.Sprintf("%d/%d", done, total)
	}
	return "  " + ui.JoinRow(labelCell, countStyle.Width(9).Render(counts), p.bar.ViewAs(pct))
}

// 进度条专用样式。阶段标签用青色区分于报表正文。
var (
	stageLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	countStyle      = lipgloss.NewStyle().Faint(true)
)

// stageStyle 让「总计」行更醒目，其余阶段统一青色。
func stageStyle(label string) lipgloss.Style {
	if label == "总计" {
		return lipgloss.NewStyle().Bold(true)
	}
	return stageLabelStyle
}

// 注：原先的 padDisplay 已移除——排版统一交给 lipgloss 的 Width()，
// 手写宽度补白是此前表格错位的根源，不该留第二条路径。

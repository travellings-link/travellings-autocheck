package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	stdlog "log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"travellings-bot-go/check"
	"travellings-bot-go/crawl"
	"travellings-bot-go/db"
	"travellings-bot-go/pipeline"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"gorm.io/gorm"
)

// verbose 由 -v 持久标志设置，供各命令判断是否输出 debug 细节。
var verbose bool

func main() {
	root := &cobra.Command{
		Use:   "bot",
		Short: "开往成员站巡查工具",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			setupLog(verbose)
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "输出 debug 级日志")
	root.PersistentFlags().StringVar(&cfgLogFile, "log-file", "", "日志写入指定文件（JSON 行格式）；留空则输出到 stderr")
	root.AddCommand(checkCmd())
	root.AddCommand(runCmd())

	// ^C 不直接退出：先把取消信号传进管线，让它停下来、把已判定的站点整理成报告。
	// 第一次 ^C 触发取消；若管线在收尾阶段再次按下，signal.NotifyContext 的行为
	// 是第二次直接按默认处理退出——这正是需要的「先礼后兵」。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := root.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func checkCmd() *cobra.Command {
	var (
		cdpURL   string
		timeout  time.Duration
		jsSettle time.Duration
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "check <url>",
		Short: "判定单个站点是否仍挂有开往入口（不写库）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			urlstr := args[0]
			t0 := time.Now()
			rawCtx, cancelRaw := context.WithTimeout(ctx, timeout)
			p := crawl.Raw(rawCtx, urlstr, timeout)
			cancelRaw()
			msRaw := time.Since(t0)
			res := check.Evaluate(p, check.Raw)
			stage := check.Raw
			var msJS time.Duration
			if res.Escalate {
				t1 := time.Now()
				p2, err := crawl.Rendered(ctx, cdpURL, urlstr, jsSettle,
					func(pp check.Page) bool { _, ok := check.MatchTravellings(pp); return ok })
				msJS = time.Since(t1)
				// 无论渲染成功与否，都已进入 js 阶段——stage 必须如实反映，
				// 否则会出现 stage=raw 但 evidence 写着 "infra js-leg" 的自相矛盾。
				stage = check.JS
				if err != nil {
					// 工具链故障：用 Evaluate 判语汇（超时→TIMEOUT），与管线口径一致。
					v := check.Evaluate(check.Page{FetchErr: err.Error()}, check.JS)
					res = check.Result{
						Verdict:  v.Verdict,
						Evidence: "infra js-leg: " + err.Error(),
					}
				} else {
					res = check.Evaluate(p2, check.JS)
				}
			}
			out := map[string]any{
				"url": urlstr, "verdict": string(res.Verdict), "stage": string(stage),
				"evidence": res.Evidence, "raw_ms": msRaw.Milliseconds(),
			}
			if msJS > 0 {
				out["js_ms"] = msJS.Milliseconds()
			}
			if asJSON {
				_ = json.NewEncoder(os.Stdout).Encode(out)
				return nil
			}
			// 短字段用表格对齐；evidence 可能很长（含完整错误链），
			// 塞进定宽单元格会被截断——而诊断恰恰需要完整原文。
			// 所以 evidence 单独占一块，按终端宽度自动换行。
			r := newReporter(os.Stdout)
			short := []string{"url", "verdict", "stage", "raw_ms", "js_ms"}
			rows := make([]table.Row, 0, len(short))
			for _, k := range short {
				v, ok := out[k]
				if !ok {
					continue
				}
				rows = append(rows, table.Row{k, fmt.Sprint(v)})
			}
			r.block(newTable([]table.Column{
				{Title: "字段", Width: 10},
				{Title: "值", Width: 70},
			}, rows).View())

			if ev, ok := out["evidence"]; ok {
				r.blank()
				r.text(labelStyle.Render("evidence"))
				for _, ln := range wrapText(fmt.Sprint(ev), terminalWidth()-2) {
					r.text("  " + ln)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cdpURL, "cdp-url", "ws://127.0.0.1:9222/devtools/browser", "已运行的浏览器调试端点")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Second, "单请求超时时间（抓取阶段）")
	cmd.Flags().DurationVar(&jsSettle, "js-settle", 8*time.Second, "等待 SPA 挂载上限（渲染阶段）")
	cmd.Flags().BoolVar(&asJSON, "json", false, "单行 JSON 输出，便于脚本消费")
	return cmd
}

func runCmd() *cobra.Command {
	var cfg runConfig
	cmd := &cobra.Command{
		Use:   "run",
		Short: "全量巡查（默认 dry-run；--write 才写库）",
		Long: "全量巡查。默认 dry-run 只产判定、不写库。\n" +
			"配 --from-api 时站点清单取自 https://api.travellings.cn/all，完全不读数据库。",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.Verbose = verbose
			if cfg.DryRun {
				cfg.Write = false // --dry-run 是显式开关，压过 --write
			}
			return runOnce(cmd.Context(), cfg)
		},
	}
	// run 的参数集中定义：--help 展示与实际读到的配置必须一致。
	addRunFlags(cmd, &cfg)
	return cmd
}

// —— ANSI 颜色（零依赖）。非 TTY 或 NO_COLOR 时自动降级为纯文本，
// 保证管道/重定向/CI 日志里不出现控制字符。 ——
type color int

const (
	_ color = iota // 占位，保证后续色值从 1 开始
	cDim
	cBold
	cRed
	cGreen
	cYellow
	cCyan
)

// 报表写 stdout、日志写 stderr，两条流要各自判断是否终端：
// 只重定向 stdout 时（如 ... > report.txt），终端里的日志仍应上色。
// 颜色能力惰性求值：包级变量在某些加载顺序下可能早于环境/句柄就绪，
// 且测试需要能重新探测。用 OnceValue 保证只探测一次、且发生在真正要输出时。
var (
	useColorOnce       = sync.OnceValue(func() bool { return detectColor(os.Stdout) })
	useColorStderrOnce = sync.OnceValue(func() bool { return detectColor(os.Stderr) })
)

func useColor() bool       { return useColorOnce() }
func useColorStderr() bool { return useColorStderrOnce() }

// detectColor 判定该流是否支持颜色，遵循业界通行的环境变量约定：
// NO_COLOR / TERM=dumb 强制关闭；CLICOLOR_FORCE / CLICOLOR 可覆盖终端探测。
// Windows 上同时尝试开启 VT 解析（旧 conhost 默认不认 ANSI）。
func detectColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if v, ok := os.LookupEnv("CLICOLOR_FORCE"); ok && v != "0" {
		return true
	}
	if os.Getenv("CLICOLOR") == "0" {
		return false
	}
	return enableVT(f)
}

func colorize(c color, s string) string {
	return colorizeTo(os.Stdout, c, s)
}

// colorizeTo 按目标流的终端能力决定是否上色。
func colorizeTo(f *os.File, c color, s string) string {
	on := useColor()
	if f == os.Stderr {
		on = useColorStderr()
	}
	if !on {
		return s
	}
	var code string
	switch c {
	case cDim:
		code = "\x1b[2m"
	case cBold:
		code = "\x1b[1m"
	case cRed:
		code = "\x1b[31m"
	case cGreen:
		code = "\x1b[32m"
	case cYellow:
		code = "\x1b[33m"
	case cCyan:
		code = "\x1b[36m"
	default:
		return s
	}
	return code + s + "\x1b[0m"
}

// isIgnore 判断该条判定是否属于"不写库"。
// 这类结果的结论可信度不足（我方工具链问题或站点超时），
// 既不写库也不该混进正常的 ERROR/TIMEOUT 统计。
func isIgnore(v pipeline.Verdict) bool {
	return v.InfraErr != "" || v.NavTimeout != ""
}

// statusColor 按判定语义词返回样式：RUN 绿、LOST 黄、ERROR/TIMEOUT 红、IGNORE 紫。
// 返回 Style 而非字符串，方便调用方决定宽度/加粗等附加属性。
func statusColor(s string) lipgloss.Style {
	switch s {
	case "RUN":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // 亮绿
	case "LOST":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // 亮黄
	case "ERROR", "TIMEOUT":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // 亮红
	case "IGNORE":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("5")) // 紫：结论不可信
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // 裸 HTTP 码等
	}
}

// log 是全局 logger，由 setupLog 在启动时初始化（默认 Info 级、无颜色）。
var log = zerolog.Nop()

// setupLog 配置全局日志。verbose 时开 Debug 级，否则 Info。
// 日志一律走 stderr——stdout 留给 --format json 的判定流（baseline diff 要能直接管道）。
func setupLog(verbose bool) *zerolog.Logger {
	zerolog.TimeFieldFormat = "15:04:05.000"
	// 级别名缩短，便于扫读
	zerolog.LevelFieldName = "level"
	zerolog.MessageFieldName = "msg"

	lvl := zerolog.InfoLevel
	if verbose {
		lvl = zerolog.DebugLevel
	}

	// 分工：控制台交给进度条与报表（bubbles + lipgloss 排版着色），日志走文件。
	//
	// 理由：
	//  1. 两者同写 stderr 会互相冲乱——实测踩过；
	//  2. 日志价值在事后可查，追加写文件比刷屏合适；
	//  3. zerolog 的结构化输出是给机器解析的，不是给人盯屏幕的。
	//
	// 未配置 --log-file 时退回 stderr（人读格式），保证无文件时仍可用。
	l := zerolog.New(logWriter(cfgLogFile)).With().Timestamp().Logger().Level(lvl)
	log = l
	pipeline.SetLogger(l) // 管线内部（抓取/渲染/判定）的 debug 也走同一出口
	crawl.SetLogger(l)    // crawl 包（CDP 回退等告警）也走同一出口

	// 接管标准库 log：net/http 等库在内部直接 log.Printf 写 os.Stderr
	// （如 transport.go 的 "Unsolicited response received on idle HTTP channel"），
	// 完全绕过我们的出口，会把进度块的相对定位基准顶偏，整轮错位成两块。
	// 去掉默认的日期时间前缀（zerolog 自己会加），再转成一条正常日志。
	stdlog.SetFlags(0)
	stdlog.SetOutput(logBridge{l: &l})
	return &l
}

// logBridge 把标准库 log 的输出转成一条 zerolog 日志。
//
// 为什么要桥接而不是直接改 SetOutput 到 stderr：日志出口统一后，
// 进度条的 Suspend/Resume 才有机会让位；否则任何一次标准库 log
// 都会在进度块中间硬插一行。
type logBridge struct{ l *zerolog.Logger }

func (b logBridge) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		b.l.Warn().Str("src", "stdlog").Msg(msg)
	}
	return len(p), nil
}

// cfgLogFile 由 --log-file 设置；为空表示不写文件、退回 stderr。
var cfgLogFile string

// logWriter 决定日志落点：
//   - 配了文件：写 JSON 到文件（便于 grep/jq 与事后分析）
//   - 未配文件：写人读格式到 stderr
func logWriter(path string) io.Writer {
	if path != "" {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			return f
		} else {
			fmt.Fprintf(os.Stderr, "无法打开日志文件 %s: %v（改用 stderr）\n", path, err)
		}
	}
	// 包一层 progWriter：写终端前先擦掉进度块，写完再重画，
	// 否则日志会插进进度块的相对定位基准里，整轮错位成两块进度条。
	return progWriter{w: zerolog.ConsoleWriter{
		Out:        os.Stderr,
		NoColor:    !useColorStderr(),
		TimeFormat: "15:04:05.000",
		PartsOrder: []string{zerolog.TimestampFieldName, zerolog.LevelFieldName,
			zerolog.MessageFieldName},
		FormatLevel: func(i interface{}) string {
			ls, _ := i.(string)
			return levelTag(ls)
		},
	}}
}

// levelTag 把 zerolog 的级别名缩成 3 字母，与报表风格一致。
func levelTag(s string) string {
	switch s {
	case "debug":
		return "DBG"
	case "info":
		return "INF"
	case "warn":
		return "WRN"
	case "error":
		return "ERR"
	case "fatal":
		return "FTL"
	default:
		return strings.ToUpper(s)
	}
}

// reporter 是报表的唯一输出出口。
// 分工：
//   - 结构化部分（表格、进度条）交给 bubbles 组件；
//   - 文本部分（标题、空行、状态提示）交给 lipgloss 渲染——
//     bubbles 没有文本/日志组件，它的组件清单是：table/progress/list/
//     textinput/textarea/viewport/spinner/paginator/help/key/filepicker/
//     cursor/stopwatch/timer/runeutil，都是交互控件。
//
// reporter 自身只负责把已渲染的字符串写出去，其余代码不再直接调 fmt。
type reporter struct{ w io.Writer }

func newReporter(w io.Writer) *reporter { return &reporter{w: w} }

// write 输出一行；空字符串即空行。
// lipgloss 的 Render 对空串返回空串，所以标题/空行/正文可以走同一条路径。
func (r *reporter) write(s string) {
	_, _ = fmt.Fprintln(r.w, s)
}

// title 输出报告主标题：加粗，片段用 · 分隔。
func (r *reporter) title(parts ...string) {
	r.write(headingStyle.Render(strings.Join(parts, " · ")))
}

// section 输出小节标题：暗色，作为内容分组。
func (r *reporter) section(s string) {
	r.write(sectionStyle.Render(s))
}

// blank 输出一个空行。
func (r *reporter) blank() { r.write("") }

// text 输出一段已渲染的文本。
func (r *reporter) text(s string) { r.write(s) }

// block 输出组件渲染出的多行文本（如 table.View()）。
// table 会把每行右侧补齐到表格全宽，这里逐行裁掉行尾空格，
// 避免终端里出现大片空白，也便于拷进聊天窗口。
func (r *reporter) block(s string) {
	for _, ln := range strings.Split(s, "\n") {
		r.write(strings.TrimRight(ln, " "))
	}
}

// printReport 输出人类可读的巡查报告：状态分布 + 问题站点明细。
// 排版与着色一律交给 bubbles（table 负责表格，lipgloss 样式由 table.Styles 承载），
// 不再手工补空格或自己算 CJK 宽度。
func printReport(w io.Writer, vs []pipeline.Verdict, elapsed time.Duration, interrupted bool) {
	r := newReporter(w)
	stats := countStats(vs)
	total := len(vs)
	if total == 0 {
		r.text("本次没有巡查任何站点")
		return
	}

	runN := stats["RUN"]
	pct := 100 * float64(runN) / float64(total)

	// 两类"未写库"分开计数，因为它们指向完全不同的排查方向：
	//   infra   → 查 CDP/浏览器（我们的问题）
	//   navTimeout → 站点太慢或 js-settle 太小（站点/配置问题）
	infraN, navTimeoutN, ignoreN := 0, 0, 0
	for _, v := range vs {
		switch {
		case v.InfraErr != "":
			infraN++
		case v.NavTimeout != "":
			navTimeoutN++
		}
		if isIgnore(v) {
			ignoreN++
		}
	}

	r.blank()
	// 中断与正常跑完的标题必须一眼可分：中断时这 N 站不代表全量清单，
	// 误当成「巡查完成」会让人以为剩下的站都还没开始就已经有结论了。
	headText := "巡查完成 " + strconv.Itoa(total) + " 站"
	if interrupted {
		headText = "巡查中断 · 已判定 " + strconv.Itoa(total) + " 站"
	}
	head := headingStyle.Render(headText) + " · " +
		runPctTag(pct) + " · " +
		dimStyle.Render("用时 "+elapsed.Round(time.Millisecond).String())
	// IGNORE 用紫色：既不是错误（红会让人以为站点坏了），
	// 也不是正常（淡色会被忽略），而是"本轮结论不可信、不写库"。
	// 括号里给出细分原因，方便一眼判断该查 CDP 还是该调 js-settle。
	if ignoreN > 0 {
		detail := ""
		switch {
		case infraN > 0 && navTimeoutN > 0:
			detail = fmt.Sprintf("，工具链故障 %d / 站点超时 %d", infraN, navTimeoutN)
		case infraN > 0:
			detail = fmt.Sprintf("，工具链故障 %d", infraN)
		case navTimeoutN > 0:
			detail = fmt.Sprintf("，站点超时 %d", navTimeoutN)
		}
		head += " · " + unwrittenStyle.Render(fmt.Sprintf("IGNORE %d%s", ignoreN, detail))
	}
	r.text(head)
	r.blank()

	// —— 状态分布：bubbles/table ——
	type row struct {
		name string
		n    int
	}
	// 状态分布按"最终归类"统计：判了 IGNORE 的站点从原始 status 里扣掉，
	// 避免同一站在 TIMEOUT 和 IGNORE 里各算一次、占比相加超过 100%。
	rows := make([]row, 0, len(stats)+1)
	counts := map[string]int{}
	for _, v := range vs {
		if isIgnore(v) {
			continue // IGNORE 单独成行
		}
		counts[string(v.Status)]++
	}
	for k, n := range counts {
		rows = append(rows, row{k, n})
	}
	if ignoreN > 0 {
		rows = append(rows, row{"IGNORE", ignoreN})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].name < rows[j].name
	})

	r.section("状态分布")
	r.text(joinRow(
		cell("判定", 10, dimStyle),
		cell("数量", 8, dimStyle),
		cell("占比", 8, dimStyle),
	))
	for _, x := range rows {
		r.text(joinRow(
			cell(x.name, 10, statusColor(x.name)),
			cell(strconv.Itoa(x.n), 8, boldStyle),
			cell(fmt.Sprintf("%.1f%%", 100*float64(x.n)/float64(total)), 8, dimStyle),
		))
	}

	// —— 问题站点明细 ——
	bad := make([]pipeline.Verdict, 0, total-runN)
	for _, v := range vs {
		if v.Status != check.Run {
			bad = append(bad, v)
		}
	}
	if len(bad) == 0 {
		r.blank()
		r.text(okStyle.Render("无问题站点"))
		return
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].SiteID < bad[j].SiteID })

	r.blank()
	r.text(headingStyle.Render(fmt.Sprintf("问题站点（%d）", len(bad))))
	// 「原因」列按终端剩余宽度自适应：错误原文是诊断的关键，
	// 固定 60 列会把 TLS 错误链、CDP 超时详情都切掉，让人看不懂为什么失败。
	const (
		idW, stW, stageW, urlW = 6, 8, 6, 38
	)
	reasonW := terminalWidth() - (idW + stW + stageW + urlW) - 6
	if reasonW < 30 {
		reasonW = 30 // 窄终端下保底，宁可折行也别丢信息
	}

	r.text(joinRow(
		cell("ID", idW, dimStyle),
		cell("判定", stW, dimStyle),
		cell("阶段", stageW, dimStyle),
		cell("站点", urlW, dimStyle),
		cell("原因", reasonW, dimStyle),
	))
	for _, v := range bad {
		// 不写库的判定在明细里统一显示 IGNORE，与状态分布对得上；
		// 原始 status（TIMEOUT/ERROR）已含在 evidence 里，不丢信息。
		st := string(v.Status)
		if isIgnore(v) {
			st = "IGNORE"
		}
		r.text(joinRow(
			cell(strconv.Itoa(v.SiteID), idW, dimStyle),
			cell(st, stW, statusColor(st)),
			cell(string(v.Stage), stageW, dimStyle),
			cell(truncate(v.URL, urlW-1), urlW, lipgloss.NewStyle()),
			cell(truncate(v.Evidence, reasonW-1), reasonW, dimStyle),
		))
	}
}

// newTable 构造一张静态表（不含 ANSI 的纯文本用它，边框与对齐由 bubbles 负责）。
// 注意：bubbles 的 table 用 len() 计算单元格宽度，含 ANSI 的字符串会被
// 当成可见字符而截断（实测出现 "ERR…"），所以带颜色的内容不要喂给它。
func newTable(cols []table.Column, rows []table.Row) table.Model {
	sum := 0
	for _, c := range cols {
		sum += c.Width
	}
	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithWidth(sum+len(cols)*3),
		table.WithHeight(len(rows)+1),
		table.WithFocused(false),
	)
	styles := table.DefaultStyles()
	// Header 与 Cell 必须同样的内边距，否则表头和数据列会错开一格。
	styles.Header = styles.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("8")).
		BorderBottom(true).
		Bold(true).
		Padding(0, 1)
	styles.Cell = styles.Cell.Padding(0, 1)
	styles.Selected = lipgloss.NewStyle()
	t.SetStyles(styles)
	return t
}

// cell 构造一个定宽单元格。
// lipgloss 的 Width() 按**显示宽度**填充，正确跳过 ANSI 转义，
// 所以彩色内容用它排版不会被算错——这正是 table 组件做不到的。
func cell(s string, width int, style lipgloss.Style) string {
	return style.Width(width).Render(s)
}

// row 把若干已定宽的单元格拼成一行。
func joinRow(cells ...string) string { return strings.Join(cells, " ") }

// 样式统一由 bubbles/lipgloss 提供，这里只做语义命名，不自己算排版。
var (
	headingStyle = lipgloss.NewStyle().Bold(true)
	sectionStyle = lipgloss.NewStyle().Faint(true)
	boldStyle    = lipgloss.NewStyle().Bold(true)
	dimStyle     = lipgloss.NewStyle().Faint(true)
	okStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	warnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	// unwrittenStyle 用于「未写库」标记：紫色（洋红），区别于错误的红、警告的黄。
	unwrittenStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	labelStyle     = lipgloss.NewStyle().Faint(true)
	errStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

// wrapText 按显示宽度折行，不切断单词也不吞字符。
// evidence 里常见长 URL 与错误链，定宽截断会丢失诊断信息。
func wrapText(s string, width int) []string {
	if width < 20 {
		width = 20
	}
	var out []string
	for _, paragraph := range strings.Split(s, "\n") {
		line := ""
		for _, word := range strings.Fields(paragraph) {
			if line == "" {
				line = word
				continue
			}
			if displayWidth(line)+1+displayWidth(word) <= width {
				line += " " + word
				continue
			}
			out = append(out, line)
			line = word
		}
		if line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		out = []string{s}
	}
	return out
}

// runPctTag 给 RUN 占比上色：>=90 绿、>=70 黄、其余红（与闸门直觉一致）。
func runPctTag(pct float64) string {
	s := strconv.FormatFloat(pct, 'f', 1, 64) + "% RUN"
	switch {
	case pct >= 90:
		return colorize(cGreen, s)
	case pct >= 70:
		return colorize(cYellow, s)
	default:
		return colorize(cRed, s)
	}
}

// displayWidth 估算终端显示宽度：CJK/全角算 2 列，其余算 1。
// 表格含中文表头，用 len() 排版会错位。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115f, // 谚文字母
			r >= 0x2e80 && r <= 0xa4cf && r != 0x303f, // CJK 部首~彝文
			r >= 0xac00 && r <= 0xd7a3,                // 谚文音节
			r >= 0xf900 && r <= 0xfaff,                // CJK 兼容
			r >= 0xfe30 && r <= 0xfe6f,                // CJK 兼容标点
			r >= 0xff00 && r <= 0xff60,                // 全角
			r >= 0xffe0 && r <= 0xffe6:                // 全角符号
			w += 2
		default:
			w++
		}
	}
	return w
}

// pad 右补空格到指定显示宽度。
func pad(s string, width int) string {
	if d := displayWidth(s); d < width {
		return s + strings.Repeat(" ", width-d)
	}
	return s
}

// truncate 按 rune 截断，避免把多字节字符切坏。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func countStats(vs []pipeline.Verdict) map[string]int {
	m := map[string]int{}
	for _, v := range vs {
		m[string(v.Status)]++
	}
	return m
}

func writeJSON(vs []pipeline.Verdict) error {
	enc := json.NewEncoder(os.Stdout)
	for _, v := range vs {
		if err := enc.Encode(map[string]any{"id": v.SiteID, "url": v.URL, "verdict": string(v.Status), "stage": string(v.Stage), "evidence": v.Evidence}); err != nil {
			return err
		}
	}
	return nil
}

// verdicts2updates 把判定转成待写库更新。
// 关键：InfraErr 非空的条目**跳过**——那是我们自己的工具链故障，
// 写进去等于用自己的失败去改站点的线上状态（PLAN §5 约定）。
func verdicts2updates(vs []pipeline.Verdict) []db.VerdictUpdate {
	out := make([]db.VerdictUpdate, 0, len(vs))
	for _, v := range vs {
		// 工具链故障与站点超时都不写库：
		// 前者是我们的 CDP 出了问题，后者是预算不足以证明站点真的坏了。
		if v.InfraErr != "" || v.NavTimeout != "" {
			continue
		}
		out = append(out, db.VerdictUpdate{SiteID: v.SiteID, Status: string(v.Status), Reason: v.Evidence})
	}
	return out
}

// runConfig 汇总一次巡查的全部参数。
type runConfig struct {
	Driver, SQLitePath, DSN      string
	HTTPWorkers, BrowserWorkers  int
	FetchQueue                   int
	TimeoutMS, JSSettleMS, Limit int
	Stage, CDPURL                string
	Verbose                      bool
	RenderRetries                int
	CDPConns                     int
	DryRun                       bool
	Write                        bool
	Format                       string
	MinPct                       float64
	FromAPI                      bool
}

// runOnce 执行一次完整巡查。
// FromAPI 时不打开数据库，清单取自 api.travellings.cn；该模式禁止写库。
func runOnce(ctx context.Context, cfg runConfig) error {
	if cfg.FromAPI && cfg.Write {
		return fmt.Errorf("--from-api 与 --write 互斥：API 清单无行级上下文，不能写回")
	}
	// 连接数先落地再建池：池是懒建的（第一次 Rendered 时），这里只是写下配置，
	// CloseBrowser 之后下一轮按新值重建。
	//
	// 默认跟随 --browser-workers：并行度来自**连接数**而不是 --workers。
	// Obscura 的 serve 起的是进程池 + 按连接 round-robin，同一连接上的命令
	// 由单个 worker 串行处理；32 路渲染挤 1 条连接会把建页/关页命令排到超时，
	// 客户端放弃而服务端照建，标签页就开始堆。每个渲染 worker 一条连接，
	// 才能让并发真正并行（实测 8 页：1 条连接 13.9s vs 8 条 4.0s）。
	conns := cfg.CDPConns
	if conns <= 0 {
		conns = cfg.BrowserWorkers
	}
	crawl.SetConnCount(conns)

	// 每轮结束后释放共享浏览器：否则 Browser/标签页/连接都留在进程里，
	// 下一轮复用会叠加新的实例，表现为浏览器内存只涨不降。
	defer crawl.CloseBrowser()
	var sites []pipeline.Site
	var d *gorm.DB
	if cfg.FromAPI {
		var err error
		sites, err = pipeline.FetchSitesFromAPI(ctx)
		if err != nil {
			return fmt.Errorf("fetch api sites: %w", err)
		}
		// 生产口径会跳过「30 天内人工检查过」的站点（依据 lastManualCheck 时间戳）。
		// 但 API 只返回布尔 isManualChecked、没有时间戳，无法判断是否在 30 天内，
		// 所以这里不跳过任何站点，全量巡查——dry-run 要的是完整 baseline。
		log.Debug().Str("source", "api.travellings.cn").Int("sites", len(sites)).
			Bool("skip_recent_30d", false).
			Str("note", "API 无人工检查时间，未跳过任何站点").
			Msg("清单来源")
	} else {
		var err error
		d, err = db.Open(cfg.Driver, cfg.SQLitePath, cfg.DSN)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		// 显式关闭底层连接池：runOnce 结束即释放句柄，不留到进程退出兜底。
		// 否则同进程内多次调用会累积占用；测试用 t.TempDir 时，未关的句柄
		// 会让临时库删不掉而误报失败（Windows 文件锁）。
		if sqlDB, derr := d.DB(); derr == nil {
			defer func() { _ = sqlDB.Close() }()
		}
		var ws []db.Web
		ws, err = db.ListSites(d, time.Now())
		if err != nil {
			return fmt.Errorf("list sites: %w", err)
		}
		sites = make([]pipeline.Site, 0, len(ws))
		for _, w := range ws {
			sites = append(sites, pipeline.Site{ID: w.ID, URL: w.Link})
		}
		log.Debug().Str("source", "db").Int("sites", len(sites)).
			Bool("skip_recent_30d", true).
			Msg("清单来源")
	}

	// 进度条只在终端且非 -v 时显示（-v 的 debug 日志会冲乱它）
	total := len(sites)
	if cfg.Limit > 0 && cfg.Limit < total {
		total = cfg.Limit
	}
	stats := &pipeline.StageStats{}
	prog := newProgress(total, stats, cfg.Verbose)
	progStop := make(chan struct{})
	prog.Start(progStop)
	// 注册给日志 writer：运行中写 stderr 的日志会先让进度条让位。
	activeProg.Store(prog)
	// 兜底：错误路径提前返回时也要停刷新、擦掉进度条。
	// stopProg 用 sync.Once 包一层，正常路径已调用过也不会重复 close。
	var stopOnce sync.Once
	stopProg := func() {
		stopOnce.Do(func() {
			close(progStop)
			prog.Finish()
			activeProg.Store(nil)
		})
	}
	defer stopProg()

	opts := pipeline.Options{
		HTTPWorkers: cfg.HTTPWorkers, BrowserWorkers: cfg.BrowserWorkers, FetchQueue: cfg.FetchQueue,
		TimeoutMS: cfg.TimeoutMS, JSSettleMS: cfg.JSSettleMS, CDPURL: cfg.CDPURL,
		Limit: cfg.Limit, Stage: cfg.Stage,
		RenderRetries: cfg.RenderRetries,
		Stats:         stats,
	}
	log.Debug().
		Int("http_workers", opts.HTTPWorkers).Int("browser_workers", opts.BrowserWorkers).
		Int("fetch_queue", opts.FetchQueue).Int("timeout_ms", opts.TimeoutMS).
		Int("js_settle_ms", opts.JSSettleMS).Str("stage", opts.Stage).Int("limit", opts.Limit).
		Int("cdp_conns", conns).
		Msg("管线参数")
	t0 := time.Now()
	verdicts := pipeline.Run(ctx, sites, opts)
	elapsed := time.Since(t0)

	// 中断判定放在管线返回之后：ctx 被取消只说明用户按了 ^C，
	// 具体停在哪一刻由 pipeline.Run 自己收口，这里只读结果。
	// runOnce 的 ctx 唯一取消来源就是信号（main 里 NotifyContext），
	// 故 Err() != nil 即等价于「被中断」。
	interrupted := ctx.Err() != nil

	// 收尾对账：正常情况下每个站点的标签页在 Rendered 里已经关掉，
	// 这里只是把「没关掉的」统计出来并补关。浏览器拥塞（Obscura 的
	// INTERCEPTION: deferred queue full）时关闭命令本身会静默失败，
	// 不做对账就只能等标签页累积到把浏览器拖死。
	if interrupted {
		// 中断时先收进度条：对账要扫一遍浏览器，不能让用户盯着冻结的帧等。
		stopProg()
	}
	if n := crawl.SweepTabs(); n > 0 {
		log.Warn().Int("closed", n).Msg("本轮有标签页未正常关闭，已补关")
	}

	// 管线已结束，先把进度条收尾，之后报表才写到干净的行上。
	// 若留到 defer，报表会盖在旧帧下面，看起来就像没跑到 100%。
	stopProg()

	if cfg.Format == "json" {
		if err := writeJSON(verdicts); err != nil {
			return err
		}
	} else {
		// json 模式已在上一分支返回，这里必然是给人看的表格——写 stdout。
		printReport(os.Stdout, verdicts, elapsed, interrupted)
	}
	if interrupted {
		// 中断的那一轮无论如何不写库：已判定的只是一部分，写进去等于把
		// 「没查到」当成「站点坏了」落盘，会污染线上 status。
		log.Warn().Int("judged", len(verdicts)).Int("total", total).
			Msg("巡查被中断，本轮结果不写库")
		return nil
	}
	if !cfg.Write {
		log.Info().Msg("dry-run：未写库")
		return nil
	}
	// 写库必须有连接。FromAPI 与 Write 在入口已互斥，
	// 这里仍显式断言：不让"某天有人放宽互斥"变成 nil 解引用崩溃。
	if d == nil {
		return fmt.Errorf("内部错误：写库路径无数据库连接（FromAPI=%v）", cfg.FromAPI)
	}
	// 占比闸门(--limit 时禁用)
	if cfg.Limit > 0 {
		log.Info().Int("limit", cfg.Limit).Msg("--limit 生效，跳过占比闸门")
	} else {
		pct, count := db.RunStats(verdicts2updates(verdicts))
		log.Info().Float64("run_pct", pct).Int("sites", count).Msg("RUN 占比")
		if pct < cfg.MinPct {
			log.Warn().Float64("min_pct", cfg.MinPct).Msg("低于阈值，本轮结果丢弃不写库")
			return nil
		}
	}
	ups := verdicts2updates(verdicts)
	if err := db.WriteVerdicts(d, ups); err != nil {
		return fmt.Errorf("write verdicts: %w", err)
	}
	log.Info().Int("sites", len(ups)).Msg("已写回")
	return nil
}

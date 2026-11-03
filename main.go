package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	stdlog "log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"travellings-bot-go/check"
	"travellings-bot-go/crawl"
	"travellings-bot-go/db"
	"travellings-bot-go/internal/feishu"
	"travellings-bot-go/internal/report"
	"travellings-bot-go/internal/ui"
	"travellings-bot-go/pipeline"

	"github.com/charmbracelet/bubbles/table"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"gorm.io/gorm"
)

// verbose 由 -v 持久标志设置，供各命令判断是否输出 debug 细节。
var verbose bool

// botVersion 由发版 CI 注入（-ldflags "-X main.botVersion=..."），本地构建为 dev。
var botVersion = "dev"

func main() {
	root := &cobra.Command{
		Use:   "bot",
		Short: "开往成员站巡查工具",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			setupLog(verbose)
		},
	}
	root.Version = botVersion
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "输出 debug 级日志")
	root.PersistentFlags().StringVar(&cfgLogFile, "log-file", "", "日志写入指定文件（JSON 行格式）；留空则输出到 stderr")
	root.AddCommand(checkCmd())
	root.AddCommand(runCmd())
	root.AddCommand(masterCmd())
	root.AddCommand(workerCmd())

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
			r := ui.NewReporter(os.Stdout)
			short := []string{"url", "verdict", "stage", "raw_ms", "js_ms"}
			rows := make([]table.Row, 0, len(short))
			for _, k := range short {
				v, ok := out[k]
				if !ok {
					continue
				}
				rows = append(rows, table.Row{k, fmt.Sprint(v)})
			}
			r.Block(ui.NewTable([]table.Column{
				{Title: "字段", Width: 10},
				{Title: "值", Width: 70},
			}, rows).View())

			if ev, ok := out["evidence"]; ok {
				r.Blank()
				r.Text(ui.LabelStyle.Render("evidence"))
				for _, ln := range ui.WrapText(fmt.Sprint(ev), ui.TerminalWidth()-2) {
					r.Text("  " + ln)
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
	feishu.SetLogger(l)   // 跳过通知的 Debug 也走同一出口
	report.SetLogger(l)   // 轮末闸门/写回的日志（RUN 占比、已写回）也走同一出口

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
		NoColor:    !ui.UseColorStderr(),
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
			sites = append(sites, pipeline.Site{ID: w.ID, Name: w.Name, URL: w.Link})
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

	// 通知：跑完就发（webhook 未配置则内部静默跳过），标题色 = 本轮是否实际写库。
	notify := func(written bool) {
		if err := feishu.Notify(ctx, feishu.BuildCard(verdicts, elapsed, written)); err != nil {
			log.Error().Err(err).Msg("飞书通知发送失败")
		} else {
			log.Info().Bool("written", written).Msg("飞书通知已发送")
		}
	}
	if cfg.Format == "json" {
		if err := report.WriteJSON(verdicts); err != nil {
			return err
		}
	} else {
		// json 模式已在上一分支返回，这里必然是给人看的表格——写 stdout。
		report.Print(os.Stdout, verdicts, elapsed, interrupted)
	}
	if interrupted {
		// 中断的那一轮无论如何不写库：已判定的只是一部分，写进去等于把
		// 「没查到」当成「站点坏了」落盘，会污染线上 status。
		log.Warn().Int("judged", len(verdicts)).Int("total", total).
			Msg("巡查被中断，本轮结果不写库")
		notify(false)
		return nil
	}
	if !cfg.Write {
		log.Info().Msg("dry-run：未写库")
		notify(false)
		return nil
	}
	// 写库必须有连接。FromAPI 与 Write 在入口已互斥，
	// 这里仍显式断言：不让"某天有人放宽互斥"变成 nil 解引用崩溃。
	if d == nil {
		return fmt.Errorf("内部错误：写库路径无数据库连接（FromAPI=%v）", cfg.FromAPI)
	}
	// 轮末写库：limit 旁路/占比闸门/写回全在 report.FinishRound——与 bot master 共用
	// 同一实现（D1，旁路逻辑不再两份），调用方只做通知与错误上抛。
	written, ferr := report.FinishRound(d, verdicts, cfg.MinPct, cfg.Limit)
	if ferr != nil {
		notify(false)
		return fmt.Errorf("write verdicts: %w", ferr)
	}
	notify(written)
	return nil
}

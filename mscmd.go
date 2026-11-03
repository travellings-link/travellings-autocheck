package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"gorm.io/gorm"

	"travellings-bot-go/db"
	"travellings-bot-go/internal/feishu"
	"travellings-bot-go/internal/report"
	"travellings-bot-go/master"
	"travellings-bot-go/pipeline"
	mspb "travellings-bot-go/proto/mspb"
	"travellings-bot-go/worker"

	"github.com/spf13/cobra"
)

// masterConfig 是 bot master 的参数集（D6/D11/D13）。
type masterConfig struct {
	Listen, Token                               string
	Driver, SQLitePath, DSN                     string
	ReclaimHTTP, ReclaimBrowser                 time.Duration
	RenderRetries, TimeoutMS, JSSettleMS, Limit int
	Write, DryRun, FromAPI                      bool
	MinPct                                      float64
}

// workerConfig 是 bot worker 的参数集（D5/D9/D10）。
type workerConfig struct {
	Master, Token                          string
	HTTPWorkers, BrowserWorkers, RefillPct int
	TimeoutMS, JSSettleMS, CDPConns        int
	CDPURL                                 string
}

// loadMasterTasks 取本轮清单：--from-api 走 api.travellings.cn（完全不碰 DB，
// 便于本地测试），否则走 db.ListSites（含 30 天豁免，线上口径）。
// 与 bot run 共用同一批函数，互斥契约也照搬：--from-api 与 --write 互斥。
//
// 返回 db 连接供轮末写库（--from-api 时为 nil）：master 全程持有，取清单和写回用同一条。
// 连接关闭由调用方（masterCmd）负责，避免取完清单即弃、轮末无库可写。
func loadMasterTasks(ctx context.Context, c masterConfig) (*gorm.DB, []*mspb.Task, error) {
	if c.FromAPI && c.Write {
		return nil, nil, fmt.Errorf("--from-api 与 --write 互斥：API 清单无行级上下文，不能写回")
	}
	var (
		sites []pipeline.Site
		d     *gorm.DB
	)
	if c.FromAPI {
		got, err := pipeline.FetchSitesFromAPI(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch api sites: %w", err)
		}
		sites = got
		log.Info().Str("source", "api.travellings.cn").Msg("清单来源")
	} else {
		var err error
		d, err = db.Open(c.Driver, c.SQLitePath, c.DSN)
		if err != nil {
			return nil, nil, fmt.Errorf("open db: %w", err)
		}
		ws, err := db.ListSites(d, time.Now())
		if err != nil {
			if sql, e := d.DB(); e == nil {
				_ = sql.Close()
			}
			return nil, nil, fmt.Errorf("list sites: %w", err)
		}
		sites = make([]pipeline.Site, 0, len(ws))
		for _, w := range ws {
			sites = append(sites, pipeline.Site{ID: w.ID, Name: w.Name, URL: w.Link})
		}
		log.Info().Str("source", "db").Msg("清单来源（30 天豁免已生效）")
	}
	if c.Limit > 0 && len(sites) > c.Limit {
		sites = sites[:c.Limit]
	}
	tasks := make([]*mspb.Task, 0, len(sites))
	for _, s := range sites {
		tasks = append(tasks, &mspb.Task{Id: int32(s.ID), Url: s.URL, Name: s.Name})
	}
	return d, tasks, nil
}

// masterCmd：开 gRPC 服务、灌本轮清单、等 worker 拉取，收口后走轮末闸门写库+通知+哨兵。
// 清单来源：默认 DB（30 天豁免）/ --from-api（本地测试）；写库复用 report.FinishRound（与 bot run 同一实现）。
func masterCmd() *cobra.Command {
	var c masterConfig
	cmd := &cobra.Command{
		Use:   "master",
		Short: "主从模式 master：灌任务队列、等 worker 拉取、收口后闸门写库+通知+哨兵",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if c.Token == "" {
				return fmt.Errorf("--token 必填：worker 凭共享 token 接入（v1 无 TLS，仅限内网/隧道）")
			}
			d, tasks, err := loadMasterTasks(ctx, c)
			if err != nil {
				return err
			}
			if len(tasks) == 0 {
				return fmt.Errorf("本轮清单为空（30 天豁免把站点全跳过了？还是 --limit 0 站）")
			}
			// DB 连接 master 全程持有（取清单与轮末写回同一条），结束即关。
			if d != nil {
				if sqlDB, derr := d.DB(); derr == nil {
					defer func() { _ = sqlDB.Close() }()
				}
			}
			round := master.NewRound(c.Token, tasks, 0)
			t0 := time.Now()

			// 三行进度（爬取/渲染/所有任务）复用 run 的同一组件；仅 TTY 且非 -v 生效。
			prog := newMasterProgress(round.Snapshot, verbose)
			progStop := make(chan struct{})
			prog.Start(progStop)
			activeProg.Store(prog)
			var stopOnce sync.Once
			stopProg := func() {
				stopOnce.Do(func() {
					close(progStop)
					prog.Finish()
					activeProg.Store(nil)
				})
			}
			defer stopProg()

			lis, err := net.Listen("tcp", c.Listen)
			if err != nil {
				return fmt.Errorf("listen %s: %w", c.Listen, err)
			}
			srv := grpc.NewServer()
			// 收口即优雅退出：OnClose→GracefulStop→Serve 返回→停进度→master 退（D7 闭环）。
			round.OnClose(srv.GracefulStop)
			master.Register(srv, round)
			go func() {
				<-ctx.Done()
				srv.GracefulStop()
			}()
			log.Info().Str("listen", c.Listen).Int("tasks", len(tasks)).
				Bool("from_api", c.FromAPI).Bool("dry_run", c.DryRun || !c.Write).
				Msg("master 已就绪")
			err = srv.Serve(lis)
			stopProg()
			elapsed := time.Since(t0)

			// 收口判定：Serve 返回后仍齐判 = 正常轮末；不齐只可能是 ^C 中断，
			// 按既有契约「中断轮不写库」（D12：master 无内部 deadline，进程级交 systemd）。
			// 未写库时不发 ROUND_DONE——在线 worker 继续重试，等下次 timer 开新轮。
			if !round.Completed() {
				log.Warn().Int("total", len(tasks)).Msg("巡查被中断，本轮结果不写库")
				notifyFeishuMaster(ctx, round.ToVerdicts(), elapsed, false)
				return err
			}
			vs := round.ToVerdicts()
			report.Print(os.Stdout, vs, elapsed, false)

			dry := c.DryRun || !c.Write
			if dry {
				log.Info().Msg("dry-run：未写库")
				notifyFeishuMaster(ctx, vs, elapsed, false)
				return err
			}
			if d == nil {
				// 理论不可达：--from-api 与 --write 在取清单前已互斥。显式断言防放宽。
				notifyFeishuMaster(ctx, vs, elapsed, false)
				return fmt.Errorf("内部错误：写库路径无数据库连接（FromAPI=%v）", c.FromAPI)
			}
			written, ferr := report.FinishRound(d, vs, c.MinPct, c.Limit)
			if ferr != nil {
				notifyFeishuMaster(ctx, vs, elapsed, false)
				return fmt.Errorf("write verdicts: %w", ferr)
			}
			notifyFeishuMaster(ctx, vs, elapsed, written)
			return err
		},
	}
	addMasterFlags(cmd, &c)
	return cmd
}

// workerCmd：连 master 跑拉流循环；连不上按 D8 无限重试（指数退避封顶 60s）。
// HTTP 初审腿走真实 crawl.Raw+check.Evaluate；Browser 渲染腿 crawl.Rendered 留 M3。
func workerCmd() *cobra.Command {
	var c workerConfig
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "主从模式 worker：从 master 拉任务并逐条回传（HTTP 腿真实抓取）",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if c.Master == "" || c.Token == "" {
				return fmt.Errorf("--master 与 --token 必填")
			}
			if c.HTTPWorkers <= 0 && c.BrowserWorkers <= 0 {
				return fmt.Errorf("无能力：--http-workers 与 --browser-workers 至少一个 >0（D5）")
			}
			backoff := time.Second
			for {
				n, err := worker.RunOnce(ctx, worker.Options{
					Master: c.Master, Token: c.Token,
					HTTPWorkers: c.HTTPWorkers, BrowserWorkers: c.BrowserWorkers,
					TimeoutMS: c.TimeoutMS,
				})
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err == nil {
					log.Info().Int("reported", n).Msg("本轮结束（收到 ROUND_DONE 哨兵），worker 退出")
					return nil // D7：exit 0，由 systemd timer 进入下一轮等待
				}
				log.Warn().Err(err).Dur("retry_in", backoff).Msg("连不上 master，按 D8 重试")
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return ctx.Err()
				}
				if backoff *= 2; backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
			}
		},
	}
	addWorkerFlags(cmd, &c)
	return cmd
}

// notifyFeishuMaster 与 bot run 的 notify 闭包同语义：webhook 未配置静默跳过，
// 标题色 = 本轮是否实际写库。卡片构建/发送复用 feishu 包，不复制。
func notifyFeishuMaster(ctx context.Context, vs []pipeline.Verdict, elapsed time.Duration, written bool) {
	if err := feishu.Notify(ctx, feishu.BuildCard(vs, elapsed, written)); err != nil {
		log.Error().Err(err).Msg("飞书通知发送失败")
	} else {
		log.Info().Bool("written", written).Msg("飞书通知已发送")
	}
}

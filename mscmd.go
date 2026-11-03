package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"

	"travellings-bot-go/db"
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
func loadMasterTasks(ctx context.Context, c masterConfig) ([]*mspb.Task, error) {
	if c.FromAPI && c.Write {
		return nil, fmt.Errorf("--from-api 与 --write 互斥：API 清单无行级上下文，不能写回")
	}
	var sites []pipeline.Site
	if c.FromAPI {
		got, err := pipeline.FetchSitesFromAPI(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetch api sites: %w", err)
		}
		sites = got
		log.Info().Str("source", "api.travellings.cn").Msg("清单来源")
	} else {
		d, err := db.Open(c.Driver, c.SQLitePath, c.DSN)
		if err != nil {
			return nil, fmt.Errorf("open db: %w", err)
		}
		ws, err := db.ListSites(d, time.Now())
		if err != nil {
			return nil, fmt.Errorf("list sites: %w", err)
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
		tasks = append(tasks, &mspb.Task{Id: int32(s.ID), Url: s.URL})
	}
	return tasks, nil
}

// masterCmd：开 gRPC 服务 + token 握手（M1 骨架）。
// M2 起在此接入：连 DB 取清单灌队列 → 路由/计数 → 轮末闸门写库 → 下发哨兵退出。
func masterCmd() *cobra.Command {
	var c masterConfig
	cmd := &cobra.Command{
		Use:   "master",
		Short: "主从模式 master：灌任务队列并等 worker 拉取（M1 为握手骨架）",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if c.Token == "" {
				return fmt.Errorf("--token 必填：worker 凭共享 token 接入（v1 无 TLS，仅限内网/隧道）")
			}
			tasks, err := loadMasterTasks(ctx, c)
			if err != nil {
				return err
			}
			if len(tasks) == 0 {
				return fmt.Errorf("本轮清单为空（30 天豁免把站点全跳过了？还是 --limit 0 站）")
			}
			// M1 骨架：worker 回传的 verdict 只回哨兵状态，不做路由/计数/写库（M2–M4 接入）。
			// 有人误给 --write 时必须说清楚，否则以为已经落盘。
			if c.Write && !c.DryRun {
				log.Warn().Msg("master M1 骨架：--write 尚未接入轮末写库（M2 生效），本轮不会落盘")
			}
			round := master.NewRound(c.Token, tasks, 0)

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
				Msg("master 已就绪（M1 握手骨架）")
			err = srv.Serve(lis)
			// 收口：本轮结束（或中断）时把进度推进到满格再收尾帧。
			stopProg()
			return err
		},
	}
	addMasterFlags(cmd, &c)
	return cmd
}

// workerCmd：连 master 跑拉流循环；连不上按 D8 无限重试（指数退避封顶 60s）。
// M1 干的是假活（回传占位 verdict）；M2 起接 crawl.Raw/check.Evaluate 真抓取。
func workerCmd() *cobra.Command {
	var c workerConfig
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "主从模式 worker：从 master 拉任务并逐条回传（M1 为握手骨架）",
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

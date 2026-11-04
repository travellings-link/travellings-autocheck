package main

import (
	"github.com/spf13/cobra"
)

// addRunFlags 把「一轮巡查」的全部参数注册到 run 命令上。
//
// 默认值集中在这里，--help 展示与实际读到的配置必须一致。
func addRunFlags(cmd *cobra.Command, c *runConfig) {
	f := cmd.Flags()
	f.StringVar(&c.Driver, "driver", "", "数据库类型：sqlite 或 mysql")
	f.StringVar(&c.SQLitePath, "sqlite", "", "sqlite 文件")
	f.StringVar(&c.DSN, "dsn", "", "mysql DSN")
	f.IntVar(&c.HTTPWorkers, "http-workers", 20, "抓取阶段并发数")
	f.IntVar(&c.BrowserWorkers, "browser-workers", 4, "渲染阶段并发数")
	f.IntVar(&c.FetchQueue, "fetch-queue", 8, "抓取到判定的队列深度，用于反压")
	f.IntVar(&c.TimeoutMS, "timeout", 15000, "单请求超时毫秒数（抓取阶段）")
	f.IntVar(&c.JSSettleMS, "js-settle", 8000, "等待 SPA 挂载上限毫秒数（渲染阶段）")
	f.IntVar(&c.RenderRetries, "render-retries", 2, "渲染失败后重试次数（排队尾重试；确定性失败不重试）")
	f.IntVar(&c.Limit, "limit", 0, "本次最多巡查站数，0 表示全量")
	f.StringVar(&c.Stage, "stage", "both", "both=抓取+渲染，raw=只抓取不渲染，js=全部交浏览器渲染")
	f.StringVar(&c.CDPURL, "cdp-url", "ws://127.0.0.1:9222/devtools/browser", "浏览器调试端点")
	f.IntVar(&c.CDPConns, "cdp-conns", 0, "CDP 连接数，0 表示跟随 --browser-workers；每条连接是一条独立的并行通道")
	f.BoolVar(&c.Write, "write", false, "把判定写回数据库（默认只读不写）")
	f.BoolVar(&c.FromAPI, "from-api", false, "站点清单取自 api.travellings.cn，完全不碰数据库")
	f.BoolVar(&c.DryRun, "dry-run", false, "强制不写库（与 --write 同时给则以 --dry-run 为准）")
	f.StringVar(&c.Format, "format", "table", "输出格式：table 人类可读，json 逐站一行")
	f.Float64Var(&c.MinPct, "min-run-percentage", 50, "低于该 RUN 占比则丢弃本轮结果，不写库")
}

// addMasterFlags 注册 master 参数（D6/D11/D13：token 握手、租约、只走 DB）。
func addMasterFlags(cmd *cobra.Command, c *masterConfig) {
	f := cmd.Flags()
	f.StringVar(&c.Listen, "listen", ":9500", "gRPC 监听地址（仅内网/隧道，v1 无 TLS）")
	f.StringVar(&c.Token, "token", "", "pre-shared token，worker 必须携带一致（必填）")
	f.StringVar(&c.Driver, "driver", "", "数据库类型：sqlite 或 mysql")
	f.StringVar(&c.SQLitePath, "sqlite", "", "sqlite 文件")
	f.StringVar(&c.DSN, "dsn", "", "mysql DSN")
	f.DurationVar(&c.ReclaimHTTP, "reclaim-http", 0, "HTTP 租约超时回收；0=--timeout×3（D11）")
	f.DurationVar(&c.ReclaimBrowser, "reclaim-browser", 0, "Browser 租约超时回收；0=(js-settle+75s)×(1+render-retries)+15s（D11）")

	f.IntVar(&c.RenderRetries, "render-retries", 2, "渲染重试预算（决策权在 master）")
	f.IntVar(&c.HTTPRetries, "http-retries", 1, "HTTP 腿 ERROR/TIMEOUT 重派预算（D15）；0=完全对齐单机（基准测试用）")
	f.IntVar(&c.TimeoutMS, "timeout", 15000, "单请求超时毫秒数（租约推算用）")
	f.IntVar(&c.JSSettleMS, "js-settle", 8000, "等待 SPA 挂载上限毫秒数（租约推算用）")
	f.BoolVar(&c.Write, "write", false, "把判定写回数据库（默认只读不写）")
	f.BoolVar(&c.DryRun, "dry-run", false, "强制不写库")
	f.BoolVar(&c.FromAPI, "from-api", false, "站点清单取自 api.travellings.cn，完全不碰数据库（本地测试；与 --write 互斥）")
	f.IntVar(&c.Limit, "limit", 0, "本次最多巡查站数，0 表示全量（调试/灰度用）")
	f.Float64Var(&c.MinPct, "min-run-percentage", 50, "低于该 RUN 占比则丢弃本轮结果，不写库")
	f.StringVar(&c.Format, "format", "table", "输出格式：table 人类可读，json 逐站一行（与 bot run 同 schema，对照回归用）")
}

// addWorkerFlags 注册 worker 参数（D5/D9/D10：能力即批次、背压阈值参数化）。
func addWorkerFlags(cmd *cobra.Command, c *workerConfig) {
	f := cmd.Flags()
	f.StringVar(&c.Master, "master", "", "master 地址 host:port（必填）")
	f.StringVar(&c.Token, "token", "", "pre-shared token，与 master 一致（必填）")
	f.IntVar(&c.HTTPWorkers, "http-workers", 20, "HTTP 并发数；>0 拉 HTTP 流，同时 = 批次量（workers×1）")
	f.IntVar(&c.BrowserWorkers, "browser-workers", 0, "渲染并发数；>0 拉 Browser 流，同时 = 批次量；0=纯 HTTP 节点")
	f.IntVar(&c.RefillPct, "refill-pct", 25, "在手剩余 < 批次×该百分比时再拉（D9）")
	f.IntVar(&c.TimeoutMS, "timeout", 15000, "单请求超时毫秒数（抓取阶段）")
	f.IntVar(&c.JSSettleMS, "js-settle", 8000, "等待 SPA 挂载上限毫秒数（渲染阶段）")
	f.StringVar(&c.CDPURL, "cdp-url", "ws://127.0.0.1:9222/devtools/browser", "浏览器调试端点")
	f.IntVar(&c.CDPConns, "cdp-conns", 0, "CDP 连接数，0 跟随 --browser-workers")
}

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

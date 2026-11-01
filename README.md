# 🚆Travellings Autocheck
_**WIP | 自动化批量巡查工具**_

部分替代原先的 [travellings-link/travellings-bot](https://github.com/travellings-link/travellings-bot)，高性能的自动化巡查工具

## 环境
 - 自身使用 Go 开发，无需额外 Runtime
   - 开发环境：go version go1.26.0 windows/amd64
 - 外部依赖三选一：
   - [Obscura](https://github.com/h4ckf0r0day/obscura)（建议）
   - Chrome
   - 任意兼容 CDP 协议的东西

## 用法

```text
开往成员站巡查工具

Usage:
  bot [command]

Available Commands:
  check       判定单个站点是否仍挂有开往入口（不写库）
  completion  Generate the autocompletion script for the specified shell
  help        Help about any command
  run         全量巡查（默认 dry-run；--write 才写库）

Flags:
  -h, --help              help for bot
      --log-file string   日志写入指定文件（JSON 行格式）；留空则输出到 stderr
  -v, --verbose           输出 debug 级日志

Use "bot [command] --help" for more information about a command.
```

### run

```
全量巡查。默认 dry-run 只产判定、不写库。
配 --from-api 时站点清单取自 https://api.travellings.cn/all，完全不读数据库。

Usage:
  bot run [flags]

Flags:
      --browser-workers int        渲染阶段并发数 (default 4)
      --cdp-conns int              CDP 连接数，0 表示跟随 --browser-workers；每条连接是一条独立的并行通道
      --cdp-url string             浏览器调试端点 (default "ws://127.0.0.1:9222/devtools/browser")
      --driver string              数据库类型：sqlite 或 mysql
      --dry-run                    强制不写库（与 --write 同时给则以 --dry-run 为准）
      --dsn string                 mysql DSN
      --fetch-queue int            抓取到判定的队列深度，用于反压 (default 8)
      --format string              输出格式：table 人类可读，json 逐站一行 (default "table")
      --from-api                   站点清单取自 api.travellings.cn，完全不碰数据库
  -h, --help                       help for run
      --http-workers int           抓取阶段并发数 (default 20)
      --js-settle int              等待 SPA 挂载上限毫秒数（渲染阶段） (default 8000)
      --limit int                  本次最多巡查站数，0 表示全量
      --min-run-percentage float   低于该 RUN 占比则丢弃本轮结果，不写库 (default 50)
      --render-retries int         渲染失败后重试次数（排队尾重试；确定性失败不重试） (default 2)
      --sqlite string              sqlite 文件
      --stage string               both=抓取+渲染，raw=只抓取不渲染，js=全部交浏览器渲染 (default "both")
      --timeout int                单请求超时毫秒数（抓取阶段） (default 15000)
      --write                      把判定写回数据库（默认只读不写）

Global Flags:
      --log-file string   日志写入指定文件（JSON 行格式）；留空则输出到 stderr
  -v, --verbose           输出 debug 级日志
```

速查表：

| 场景示例                             | 执行命令                             |
| ------------------------------------ | ------------------------------------ |
| 完全脱离数据库干运行                 | `bot run --from-api`                 |
| 使用多线程渲染**（注意内存占用！）** | `bot run --browser-workers [线程数]` |
| 连接 MySQL 并**实际写入**数据库      | `bot run --dsn [DSN]`                |

#### --browser-workers

指定启动几个渲染进程，如果使用 obscura 作为渲染器，那么这个参数的值应当小于 `obscura --workers` 的参数值。如果是 Chrome 作为渲染器，该参数决定了最多同时打开几个 Tabs。

#### --cdp-conns

该参数与 `--browser-workers` 伴生，意思是最多打开几个 CDP 连接。obscura 对同一 CDP 连接中的页面请求是串行的，这会由于性能原因导致大量的假阳性（超时）数据，需要通过连接池的方式分散在 obscura 的 workers 中。如果以 Chrome 作为渲染后端，Chrome 会自动处理多线程，此值应为 1，防止意外启动多个 Chrome 进程。

通常无需设置此参数，当程序连接到外部 CDP 服务时，此值默认等于 `--browser-workers`；当程序没有连上外部 CDP 而回退到 Chrome 时，会强制设置此值为 1。

### check

判定单个站点是否仍挂有开往入口，**不读也不写数据库**——手边有网址想快速确认时用这个，不用起一整轮巡查。

```text
判定单个站点是否仍挂有开往入口（不写库）

Usage:
  bot check <url> [flags]

Flags:
      --cdp-url string       已运行的浏览器调试端点 (default "ws://127.0.0.1:9222/devtools/browser")
  -h, --help                 help for check
      --js-settle duration   等待 SPA 挂载上限（渲染阶段） (default 8s)
      --json                 单行 JSON 输出，便于脚本消费
      --timeout duration     单请求超时时间（抓取阶段） (default 15s)
```

```sh
bot check --json https://example.com
```

## 输出格式

`bot check --json` 与 `bot run --format json` 输出单行 JSON（stdout 只有判定流，人读日志一律走 stderr 或 --log-file，可直接接 `jq`）：

```json
{"url":"https://example.com","verdict":"LOST","stage":"js","evidence":"no entry after render","raw_ms":886,"js_ms":9685}
```

```json
{"id":1,"url":"https://blog.oalo.cn","verdict":"RUN","stage":"raw","evidence":"https://www.travellings.cn/go.html"}
```

`evidence`：通过时是命中的开往入口 URL，失败时是判定依据原文（HTTP 状态、错误链等）——诊断就看它，不会被截断。
`raw_ms` / `js_ms`：抓取与渲染两阶段各自耗时（未进入渲染阶段则无 js_ms）。

`bot run` 默认的 `--format table` 是人类可读报告：状态分布计数 + 问题站点明细（ID / 判定 / 阶段 / 站点 / 原因）。

## 判定结果

判定语汇沿用线上 status 词汇（API 直接消费，新增取值须先与上游对齐）：

| 判定                 | 含义                                             | 写库     |
| -------------------- | ------------------------------------------------ | -------- |
| `RUN`              | 页面上找到开往入口                               | ✅       |
| `LOST`             | 页面正常但找不到入口                             | ✅       |
| `ERROR`            | 证书 / 传输层失败（如实反映，不回避）            | ✅       |
| `TIMEOUT`          | 预算内没握完（`--timeout` / `--js-settle`）     | ✅       |
| HTTP 码（`403` 等） | 非 2xx 直接以状态码记录，渲染复核同一响应无意义  | ✅       |
| `IGNORE`           | 本轮结论不可信：工具链故障或站点导航超时         | ❌       |
| `WAIT`             | 人工预置状态                                       | 仅可变 `RUN` |

巡查分两个阶段（对应 `--stage`）：**抓取**（raw，并发 HTTP，便宜快）先判；未命中或失败才升级到**渲染**（js，CDP 浏览器，贵），渲染结论为终判不再升级——防环。IGNORE 的站点既不写库、也不混进正常错误统计，报告里单独计数并给出细分原因（工具链故障 / 站点超时）。

## 渲染后端

推荐 [Obscura](https://github.com/h4ckf0r0day/obscura)：巡查前先起它的 CDP 服务（默认监听 `ws://127.0.0.1:9222`）：

```sh
obscura serve --workers 8
```

`--workers` 是 Obscura 侧的渲染进程数；bot 侧的 `--browser-workers` 应当小于它（见上文速查表说明）。

**Chrome 或任何 CDP 兼容后端同理**，用 `--cdp-url` 指过去即可。不想起外部服务也能跑：连不上 `--cdp-url` 时自动回退到本地自启的无头 Chrome（CI / 临时机器可用），此时 `--cdp-conns` 强制压回 1，防止意外拉起多个 Chrome 进程。

## 数据库

`--write` 写回线上 `webs` 表，列：`id` / `status` / `name` / `link` / `tag` / `failedReason` / `lastManualCheck`。

连接方式三选一（也可直接吃环境变量：`DB_DRIVER` / `DB_DSN` / `SQLITE_PATH`）：

```sh
bot run --write --driver mysql --dsn "user:pass@tcp(host:3306)/dbname"
bot run --write --driver sqlite --sqlite ./webs.db
bot run --from-api --dry-run          # 完全不碰数据库
```

写库契约（与开往 API 共用这张库，所以动得克制）：

- 只动三列：`status`、`failedReason`、`lastManualCheck`；`name`/`link`/`tag` 等成员元数据只读，不 INSERT / 不 DELETE
- `WAIT` 不变量写进 SQL：`WAIT` 只会被改成 `RUN`，其余判定保持 `WAIT` 不动
- 30 天豁免：`lastManualCheck` 在 30 天内的站点不进本轮清单（人工刚查过就别机器再折腾）；判定写库时该字段置 `NULL`，即重新起算豁免窗口
- 占比闸门：本轮 RUN 占比低于 `--min-run-percentage`（默认 50）说明大概率是我们自己出了问题，整轮丢弃不写库；`--limit` 时闸门自动禁用
- `^C` 中断的轮次无论如何不写库——已判定的一小部分不能代表全量清单

`--from-api` 从 `https://api.travellings.cn/all` 取站点清单，与 `--write` 互斥（API 清单没有行级上下文，写不回库），适合零依赖跑 baseline。

## 定时与通知

本工具只干一件事：**跑一轮巡查，出报告，退出**（成功 0，失败非 0）。常驻调度与结果通知刻意解耦在外层：

```crontab
0 4 * * * /usr/local/bin/bot run --write >> /var/log/autocheck.log 2>&1
```

cron / systemd timer / serverless 拉起均可；DSN 含凭据，建议走环境变量 `DB_DSN` 而不是写进命令行。想把结果发进群（Telegram / 飞书 / 任意 webhook），拿 `--format json` 的判定流自己接一层即可，比如 `jq` + `curl`。

## 开发

```sh
go build -o bot .      # Windows: go build -o bot.exe .
go vet ./...
go test ./...          # 测试自包含，clone 后即可跑
```

涉及真实 CDP 的用例需要显式给出探活地址，否则自动跳过（不会误报失败）：

```sh
TRAVELLINGS_CDP_URL="ws://127.0.0.1:9222/devtools/browser" go test ./crawl
```

项目结构：

```text
check/      判定核：入口链接匹配、归一化、状态机裁决
crawl/      抓取（HTTP）与渲染（CDP 连接池）
pipeline/   巡查管线：Fetcher → Judge → Renderer，判定收集
db/         webs 表读写与写库契约
main.go     cobra 命令入口与报表输出
flags.go    run 的全部参数定义
```

## 关于贡献

欢迎为项目提出 [Issue](https://github.com/travellings-link/travellings-autocheck/issues)、[Pull Request](https://github.com/travellings-link/travellings-autocheck/pulls)。
贡献时请参考本项目已有的 Commit History，规范 Commit message。

## 版权

Under [GPL v3](https://www.gnu.org/licenses/gpl-3.0.html)
2025 © Travellings-link Project. All rights reserved.

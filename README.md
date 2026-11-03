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
| 使用多线程渲染 **（注意内存占用！）** | `bot run --browser-workers [线程数]` |
| 连接 MySQL 并 **实际写入** 数据库      | `bot run --dsn [DSN]`                |

#### --browser-workers

指定启动几个渲染进程，如果使用 obscura 作为渲染器，那么这个参数的值应当小于 `obscura --workers` 的参数值。如果是 Chrome 作为渲染器，该参数决定了最多同时打开几个 Tabs。

#### --cdp-conns

该参数与 `--browser-workers` 伴生，意思是最多打开几个 CDP 连接。obscura 对同一 CDP 连接中的页面请求是串行的，这会由于性能原因导致大量的假阳性（超时）数据，需要通过连接池的方式分散在 obscura 的 workers 中。如果以 Chrome 作为渲染后端，Chrome 会自动处理多线程，此值应为 1，防止意外启动多个 Chrome 进程。

通常无需设置此参数，当程序连接到外部 CDP 服务时，此值默认等于 `--browser-workers`；当程序没有连上外部 CDP 而回退到 Chrome 时，会强制设置此值为 1。

#### --cdp-url

浏览器调试端点，默认 `ws://127.0.0.1:9222/devtools/browser`——正是 Obscura serve 的默认监听地址。程序先用它探活一次：连上了就用这个外部浏览器；连不上**自动回退**为自启一个本地无头 Chrome（CI / 临时机器不会空跑，日志会提示回退）。

#### --driver

数据库类型，`sqlite` 或 `mysql`。只要碰库就要给：不配 `--from-api` 时清单从库读（dry-run 也算读库），配 `--write` 时判定写回。留空则读环境变量 `DB_DRIVER`。

#### --dry-run

强制不写库，即使同时给了 `--write` 也以它为准。演练、出 baseline、CI 校验都用这个。

#### --dsn

MySQL 连接串，形如 `user:pass@tcp(host:3306)/dbname`；留空读 `DB_DSN`。带凭据，建议走 `EnvironmentFile` 而不是命令行。

#### --fetch-queue

抓取到判定的队列深度，默认 8。抓取阶段快、判定是单点收口，中间用这条队列缓冲「已抓取待判定」的页面——这就是反压机制。为什么不调大：队列每条携带整页 HTML（上限 2MB），调大只会多吃内存，判定吞吐不变（收口的 Judge 只有一个）。

#### --format

`table`（默认）输出人类可读报告：状态分布 + 问题站点明细；`json` 每站点一行判定，供脚本消费。日志一律走 stderr 或 `--log-file`，所以 stdout 是干净的数据流，可以直接 `bot run --format json --from-api | jq`。

#### --from-api

站点清单改从 `https://api.travellings.cn/all` 拉取，全程不碰数据库，且与 `--write` 互斥（API 清单没有行级上下文，写不回库）。注意口径：API 只有布尔 `isManualChecked`、没有 `lastManualCheck` 时间戳，因此不做 30 天豁免、全量扫——**与生产待巡清单不等价**，适合打 baseline / 对比，不适合当正式巡查。

#### --http-workers

抓取阶段并发数，默认 20。HTTP 抓取便宜，约束主要在本地带宽与对方站点的容忍度；跑在判定前面的部分由 `--fetch-queue` 反压兜住，不会无限堆内存。

#### --js-settle

渲染阶段等待 SPA 挂载的上限（毫秒），默认 8000。是上限不是死等：渲染带回执判定，页面里一出现有效入口立刻返回，快站几百毫秒就交卷；只有确实慢的站才会等满。调太小会误伤慢 SPA 站（判成站点超时、本轮不写库）；调太大纯粹烧渲染预算。

#### --limit

本轮最多巡查前 N 个站点，0 为全量。快速验证用的。给 `--limit` 时**占比闸门自动禁用**——部分结果代表不了全量，拿它判 RUN 占比只会误判。

#### --min-run-percentage

占比闸门阈值，默认 50（%）。本轮 RUN 占比低于它就整轮丢弃不写库。防的是我们自己：断网、CDP 挂掉这类故障会让大面积站点被误判 TIMEOUT/ERROR，写库等于用自己的故障判大面积站点死刑（配合 IGNORE 计数看报告，能立刻分辨是站点坏了还是工具链坏了）。

#### --render-retries

渲染失败后的重试次数，默认 2。失败站点丢回渲染池队尾重新排队（不堵正在排队的站点），偶发的超时、CDP 抖动往往重跑就过——直接判死会误伤好站。确定性失败不重试：域名解析不了、证书过期、协议不支持，重试多少次结果都一样，白白占着浏览器池。0 为关闭重试。

#### --sqlite

sqlite 文件路径（配 `--driver sqlite` 使用），留空读 `SQLITE_PATH`。

#### --stage

巡查深度。`both`（默认）= 抓取先判，未命中的升级浏览器渲染终判，正常生产口径；`raw` = 只抓取不渲染，最快最省，但内容在 JS 里的 SPA 站会被判 LOST（误杀面见 `--from-api` 同款说明）；`js` = 全部站点交浏览器渲染（抓取阶段仍会跑，但其结果不做判定），终判最全、成本最高，适合拿一轮数据校准两种口径的翻转名单。

#### --timeout

抓取阶段单请求超时（毫秒），默认 15000。用满预算仍无响应直接终判 TIMEOUT、**不升级渲染**——抓取等不到的，浏览器进去也只是把等待重演一遍，最后仍超时收场（实测一个站白烧 15s+30s=45s），结论不会变。

#### --write

把判定写回线上 webs 表；不给就是 dry-run，只出报告不写库。写库范围与契约（只动三列、WAIT 不变量、双闸门、中断轮不写）见「数据库」一节。

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

生产部署见「定时」：Obscura 做成独立 unit，随巡查单元起停。上面这条命令用于本地调试与临时手跑。

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

## 定时

三个文件都在 `/etc/systemd/system/`。

`obscura.service`：

```ini
[Unit]
Description=Obscura CDP server
PartOf=autocheck.service

[Service]
# --workers 与 --worker-max-rss 按需调整
ExecStart=/usr/local/bin/obscura serve --workers 8 --worker-max-rss 1024 --quiet
MemoryMax=1G  # 按需调整
```

`autocheck.service`：

```ini
[Unit]
Description=Travellings autocheck
Requires=obscura.service
After=obscura.service

[Service]
Type=oneshot
EnvironmentFile=/etc/autocheck.env
# --log-file 按需调整
ExecStart=/usr/local/bin/bot run --write --driver mysql --log-file "/root/travellings-autocheck.log"
MemoryMax=1G  # 按需调整

[Install]
WantedBy=multi-user.target
```

`autocheck.timer`：

```ini
[Unit]
Description=Daily Travellings autocheck

[Timer]
OnCalendar=*-*-* 04:00:00  # 按需调整
Persistent=true

[Install]
WantedBy=timers.target
```

`/etc/autocheck.env`：

```ini
DB_DRIVER=mysql
DB_DSN=user:pass@tcp(127.0.0.1:3306)/dbname  # 按需调整
FEISHU_WEBHOOK=https://open.feishu.cn/open-apis/bot/v2/hook/xxxx  # 可选，留空则跳过通知
```

```sh
systemctl daemon-reload
systemctl enable --now autocheck.timer
systemctl start autocheck.service
systemctl list-timers autocheck.timer
journalctl -u autocheck.service -u obscura.service
```

每轮跑完自动发一张飞书交互卡片（`FEISHU_WEBHOOK` 未配置则跳过；文本含「巡查」满足关键词鉴权）。标题颜色 = 本轮是否实际写库：green 已写入，red 未写入（dry-run / 中断 / 闸门丢弃 / 写库失败）——群里扫一眼就知道要不要补跑。

巡查连不上 `--cdp-url` 会静默回退本地 Chrome，所以 Obscura 必须先于巡查就绪：`Requires=` 拉起它、`After=` 排好顺序、`PartOf=` 让它在巡查单元停止时一并停掉（`Type=oneshot` 跑完即停，不会跨轮驻留）。内存相关的参数需依环境配置针对调整。

## 主从部署（master / worker 拉模式）

单机 \`bot run\` 之外，可把巡查算力拆到多台机器：\`bot master\` 只在轮末开一个短进程
（timer 调起，D2），维护 HTTP/Browser 两条待派发队列 + 已派发哈希表 + 已响应队列（纯内存、轮末即焚）；
\`bot worker\` 主动连 master **拉**任务、逐条回传——master 不追 worker 死活，NAT 后无妨，
天然背压与负载均衡。worker 绝不原地重试/升级：一切结果回 master 重分配。

三个必踩的坑，先说死：

1. **集群里必须至少有一台带 \`--browser-workers\` 的 worker。** SPA 站 HTTP 初审判 LOST 后会
   escalate 进 Browser 队列等渲染；纯 HTTP 集群遇到一个 SPA 站就永远凑不齐收口条件，
   本轮挂到 systemd \`TimeoutStartSec\` 才作废。
2. **worker unit 千万别设 \`TimeoutStartSec\`。** worker 起来先无限重试等 master 上线（D8），
   被 systemd 掐死就永远连不上（deploy/ms-worker.service 里有注释）。
3. **master 端口对 worker 可达，v1 只有 token 没有 TLS。** \`--token\` 两侧必须一致，
   master 只应在内网/隧道监听；token 泄露 = 可领任务也可灌假判定，按 DB 凭据同级保管。

\`/etc/ms-master.env\`（DB 凭据只在 master 侧）：

\`\`\`ini
DB_DRIVER=mysql
DB_DSN=user:pass@tcp(127.0.0.1:3306)/dbname
MS_TOKEN=换成长随机串            # master --token 与所有 worker --token 用同一个
FEISHU_WEBHOOK=https://open.feishu.cn/open-apis/bot/v2/hook/xxxx  # 可选，通知仍只在 master 轮末发
\`\`\`

unit 文件在 \`deploy/\`：\`ms-cdp.service\`（worker 机的 CDP 后端）+\`ms-master.service/.timer\`
（timer 调起、跑完即退，\`TimeoutStartSec\` 兜底 = D12「进程级时限交 systemd」）+
\`ms-worker.service/.timer\`。master 机：

\`\`\`sh
systemctl daemon-reload
systemctl enable --now ms-master.timer
\`\`\`

每台 worker 机（有 Chrome/Obscura 的那台给 --browser-workers）：

\`\`\`sh
bot worker --master ms.internal:9500 --token "$MS_TOKEN" \
     --http-workers 20 --browser-workers 4 --cdp-url ws://127.0.0.1:9222/devtools/browser
\`\`\`

参数口径：拉取批次 = 对应 \`--http-workers\`/\`--browser-workers\`（workers×1，无需在 master 指定）；
在手剩余 < 批次×\`--refill-pct\`%（默认 25）自动补拉；worker 炸掉未回传的任务按租约超时
自动回队重派（HTTP \`--timeout\`×3、Browser 按 js-settle+timeout 推算，\`--reclaim-http/--reclaim-browser\`
可覆盖），反复炸超预算则按不写库终结，轮照样收口。判定语汇、写库契约（WAIT 不变量、
RUN 占比闸门、\`InfraErr/NavTimeout\` 不写库）与单机逐条一致。

**对照回归**（上线前必跑一次）：主从与单机对同一份清单应产出逐站一致的结果——

\`\`\`sh
bot master --from-api --limit 50 --format json > ms.json &      # 配一台 both worker 跑完退出
bot run    --from-api --limit 50 --format json > run.json       # 同一清单单机再跑
diff <(jq -S 'del(.evidence, .raw_ms)' -c run.json | sort) \
     <(jq -S 'del(.evidence, .raw_ms)' -c ms.json  | sort)      # 期望只差渲染竞态导致的个别站
\`\`\`

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
pipeline/   巡查管线（单机 bot run）：Fetcher → Judge → Renderer，判定收集
db/         webs 表读写与写库契约
master/     主从模式 master：四结构 + 路由核 + 租约回收 + gRPC server
worker/     主从模式 worker：两腿并发拉流循环 + gRPC client
proto/      gRPC 接口定义与 protoc 生成代码（接口契约以 .proto 为准）
internal/ui 终端渲染原语：ANSI 颜色、显示宽度、排版助手
internal/report 巡查结果多出口：人读报表 / 逐站 JSON / 写库更新 + 轮末闸门
internal/feishu 飞书交互卡片构建与 webhook 发送
main.go     cobra 命令入口（run/check/master/worker）
flags.go    各子命令的全部参数定义
deploy/     主从 systemd unit 样板
```

## 关于贡献

欢迎为项目提出 [Issue](https://github.com/travellings-link/travellings-autocheck/issues)、[Pull Request](https://github.com/travellings-link/travellings-autocheck/pulls)。
贡献时请参考本项目已有的 Commit History，规范 Commit message。

## 版权

Under [GPL v3](https://www.gnu.org/licenses/gpl-3.0.html)
2026 © Travellings-link Project. All rights reserved.

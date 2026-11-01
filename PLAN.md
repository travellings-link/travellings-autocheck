# PLAN — 开往巡查重写（travellings-bot 替代者）

**目标**：用 Go 重写开往巡查 bot，彻底替换 travellings-link/travellings-bot。对线上 1624 个成员站，
安全地用新判定规则产出巡查结果，写入与 API 共用的 webs 表（结果输出到 stdout/库；通知由外部调度层负责）。
license：GPLv3（随旧仓）。全新仓，不继承旧 test/CI。

## 架构决策（2026-11-01 解耦）

开发组决定：本仓只做**巡查**这一件事。原 PLAN 里的 `serve` 常驻入站（TG/飞书 bot 收命令）、
`screenshot` 截图命令、`notify` 出站 TG 通知三部分**全部移除**，与核心解耦。

- **保留**：`check` 包（判定核）、`crawl`（抓取/渲染）、`pipeline`、`db`、`bot check <url>`、`bot run`（全量巡查）。
- **移除**：`serve.go` / `tgapi.go` / `cron.go` / `notify/` / `crawl/screenshot.go` 及对应测试。
- `run` 只写 stdout（table/json）+ 写库；定时交给外部 crontab/serverless，通知交给外部调度层。
- 下文 §2b / §3 描述的是被移除的旧方案，保留原文仅作决策记录，不再实现。

## 术语（2026-10-31 改名）

早期草稿用 A/B/C「腿」指代管线三段，含义不自明，已统一为功能名：

| 旧称 | 现名 | 职责 | 代码位置 |
|---|---|---|---|
| A 腿 | **Fetcher**（抓取阶段） | 并发 HTTP 抓原始页，不判定 | `crawl.Raw` + `pipeline.Run` 首个 goroutine 池 |
| B 腿 | **Judge**（判定器） | 单 goroutine 收口：判定、升级路由、防环 | `pipeline.Run` 主循环 |
| C 腿 | **Renderer**（渲染阶段） | 并发 CDP 渲染，给 JS 站点终判机会 | `crawl.Rendered` + `pipeline.Run` 第二个池 |

对应 flag 同步改名：`--b-buffer` → `--fetch-queue`。
`--http-workers` / `--browser-workers` 语义不变，help 文案已标明归属阶段。

## 0. 决策约束（逐条由用户/团队确认，写入代码前不可改）

- 抓取阶段（原“A 腿”）用能自定义 Client 的 HTTP 库 —— 已定 github.com/carlmjohnson/requests
（零第三方依赖；自带 request recorder/replayer 可做离线重放 `--replay`；不用全局 Client，避免头串）
- purgeCache / _tlogin / API_TOKEN：**彻底移除**（实测 HTTP 无缓存；同库确认后不需要 API 写）
- 证书验证永不关闭（禁 InsecureSkipVerify）
- bot 与 api.travellings.cn 共用同一个 MySQL（同库已确认）
- status 词汇 = 线上串：（RUN/LOST/ERROR/TIMEOUT/HTTP码/WAIT）；API 直接消费，新增值须上游先对齐
- 30 天豁免：lastManualCheck IS NULL OR < NOW()-INTERVAL 30 DAY（保留，期满回写 NULL）
- WAIT 不变量：只能变 RUN，写入 SQL（CASE WHEN），不是 Go 判断
- 写库只动三列：status, failedReason, lastManualCheck；不 INSERT/DELETE，member 元数据只读
- 占比闸门：内存算（非 WAIT, RUN 占比 ≥ min ∧ COUNT==期望）；-limit 时禁用占比闸门
- 升级优先级 / BFS 顺序：先全部 raw 判定，再按序交渲染，渲染结果回收后回判定器（防环：js 终结）
- 抓取身份：User-Agent + Referer 逐字对齐线上 bot
- serve 常驻，收到命令后**进程内调用 run 的 handler**（同绑定同进程，非 exec）；serve 自身不跑判定、不写库
- /screenshot 是 serve 的入站命令之一（归属 §2b），需要 Obscura paint 能力
- 单实例一次全量：A/B/C 拓扑，C 是唯一稀缺资源，--browser-workers 默认 4

## 1. 已完成（勿返工）

| 件 | 文件 | 状态 |
|---|---|---|
| 判定核 | check/（verdict.go / match.go / extract.go） | 8 单测全绿：链接绝对化、无引号 href、GitHub 仓库全路径、跳转页参数、specialCase 签名、raw/js 防环、相对链接正反向 |
| 抓取 HTTP | crawl/fetch.go（carlmjohnson/requests） | 身份对齐线上；2MB 上限；传输层失败折叠进 Page.FetchErr |
| 渲染 CDP | crawl/render.go | chromedp→Obscura；a.href 绝对化；命中即返；取消噪声静默 |
| 命令入口 | main.go | cobra + bot check <url>（默认不写库，--json 输出 baseline 帧） |

端到端验收（6 案例全绿）：khbit/foundation RUN；example LOST+升级；lakeus→js→403；blog.oalo→证书 ERROR。

## 2. 待实现 — 命令面与管线

### 2a. bot run（全量巡查）

命令面：
bot run [--http-workers 20] [--browser-workers 4] [--b-buffer 8] [--timeout 15s]
        [--js-settle 8s] [--limit 0] [--stage both|raw|js]
        [--min-run-percentage 50] [--cdp-url ws://127.0.0.1:9222/devtools/browser]
        [--run-id YYYYMMDD-HHMMSS] [--write] [--format table|json] [-o path]
        [--replay DIR]: 若启用，抓取请求从录音重放，不真打外网（carlmjohnson/requests recorder）

拓扑（单进程）：
- SELECT 待巡清单（lastManualCheck 豁免过滤，status 任一）→ site list
- A：--http-workers，独立 req Client，抓取 → chan[SiteCandidate]
- B：单 goroutine（状态机收口 + 判定 + 路由 + 反压）：
  - raw：MatchTravellings 命中 → RUN；未命中 / 非2xx / 传输失败(可升级) → 投 C
  - WaitToRun(WAIT→RUN) 记录待发（write 后统一发）
  - C 回收后 JS 终判：命中 → RUN；否则 HTTP n / LOST / ERROR，防环终止
- C：--browser-workers，每 worker independent chromedp.NewContext（独立连接/isolate）
- 写库（--write 才执行，批量 UPDATE）：
  - 只动 status, failedReason, lastManualCheck=NULL
  - WAIT 不变量 SQL（CASE WHEN ...）
  - 占比+完整性双闸门（内存算，--limit 禁占比）
- 产出（不依赖 DB 流量）：
  - --format json → 每站点一行 verdict+stage+evidence，供 baseline diff
  - 表格：各状态计数 + 耗时 + λ + B/C 队列峰值 + infra_err

验收锚：
1. go test ./...
2. --limit 200 --stage both --dry-run --format json > baseline.json 跑通（无死锁）
3. 与已验 6 案例 verdict 对齐

### 2b. bot serve（常驻，入站转发）

- 命令：`bot serve`，常驻（systemd/容器/serverless 按部署拉起）
- serve 与 run 是**同一二进制**的两个 cobra 子命令
- 入站：复用旧 larkAdapter/telegramAdapter 的 WS 半（TG long-poll / Lark WSClient），不做改造
- 收到 `/check`（或对应命令）→ **在 serve 进程内直接调用 run 的 handler**（函数调用，不 exec、不拉新进程）→ 回执"巡查已启动"
- 并发护栏：serve 进程内 `atomic.Bool`/channel 串行化，保证同一时间只有一轮 run——已在跑则拒绝重复触发
- serve 自身不写库，职责划分：
     · /check → 进程内调用 run handler（巡查）
     · /query → 进程内调用查询 handler（读 DB）
     · /screenshot → 进程内调用 screenshot handler（复用渲染阶段 + Obscura paint，出图后 serve 用 Notifier 发送）
   - 判定/抓取/DB 全在各自 handler 内；serve 只做命令分发 + Notifier 发送，无长驻巡查状态

## 3. 待实现 — Notifier（出站，run 用）

- 拆自 larkAdapter/telegramAdapter 的出站半（SendMessage/SendRichText/SendPhoto）
- 不做监听器（run 不建 WS 长连接，纯 HTTP 出站）
- 报告节奏：跑完一轮发一次 summary

## 4. 明确不做（本件）

- purgeCache / _tlogin / API_TOKEN（彻底移除）
- 证书关闭验证
- CDP token（直连 9222 loopback）

## 5. 风险与对策

| 风险 | 对策 |
|---|---|
| 挑战页(403/CF)在渲染阶段很贵（lakeus 35.7s） | browser 预算按此估；js-settle 尽量小；命中即返 |
| TLS/证书烂站 RUN→ERROR（如实反映） | 不关验证；dry baseline 可见，人工可复核 |
| 渲染阶段 infra 故障识别为站点失败 | 单列 infra js-leg，非 LOST；计数 infra_err |
| 写库只动三列、共享库 | SQL 白名单列名；不 INSERT/DELETE |
| 判定规则差异（旧 RUN→新 LOST 长尾） | dry-run baseline diff 即为名单，切换前人工过一批 |
| serverless 超时 | 非主路径；默认 04:00 cron；--limit 分片 |

## 6. 数据契约备忘（写代码前不可改）

- status 词汇 = 线上串（RUN/LOST/ERROR/TIMEOUT/HTTP码/WAIT），API 直接消费，新增值须上游先对齐
- 30 天豁免 = lastManualCheck IS NULL OR < NOW()-INTERVAL 30 DAY，保留；期满回写 NULL
- WAIT 只能变 RUN（SQL 层强制）
- 抓取身份 = UA + Referer 逐字对齐线上
- 归一化：域名精确匹配（剥离 www）+ 入口路径白名单 + GitHub 仓库全路径 + 跳转页参数
- 绝不关闭证书验证
- 写库只动 status/failedReason/lastManualCheck；member 元数据只读
- 占比闸门：内存算，-limit 时禁用；完整性闸门 COUNT==期望

## 7. 里程碑

M1 已交付：check 包 + 抓取/渲染两阶段 + bot check（6 案例通过）
M2 已完成：bot run 全量管线 + batch 写库（wen能 write 已验证 serverless sqlite 落库）+ dry baseline
M3 骨架完成：Notifier 出站 TG（dry 默认 + send 已接 run 写库后）
M4 核心已交付并测 Server：runOnce 抽取(与 run 命令共享执行体)+ runPeer 原子并发护栏；TestRunPeerGuard/TestRunOnceSQLite 通过。入站 WS(真实 bot token)仍未联调——需外部凭据。
M5 切换演练：dry baseline → 人工过翻转名单 → 首次 --write

---
- M4 serve 骨架 → 核心已实现：runOnce 复用 + 原子并发护栏 + 测试通过（入站 WS 仍需真实 token 联调）
## 实际交付状态（2026-10-31）

- M1/M2 已实现并端到端实证（check 8 单测绿；bot run + sqlite + --write 落库验证通过）
- M3 骨架实现（TG 出站 dry 默认）；完整富文本/多通道待扩展
- M4 serve 骨架（并发护栏 + 进程内 run 接入点）；入站 WS 需真实 bot token 联调
- M5 需真实生产库(共享 MySQL) + 全量跑 + 人工过翻转名单，无法纯代码达成
- 构建/规格：go build -o bot.exe . 通过；go vet ./... 干净；check 单测全绿

---
## 代码里程碑达成度（round 4 复核）

- M1 ✅ 已交付实证：check 包 8 单测 + bot check 6 案例
- M2 ✅ 已交付实证：bot run 全量管线 + sqlite --write 落库 + bas 认证
- M3 ✅ 已交付实证：Notifier 出站 TG（dry 默认）
- M4 🔶 核心已交付实证：runOnce 复用 + 原子并发护栏 + 测试；仅入站 WS 联调待真实 token
- M5 ⏳ 未完成：需真实共享生产库 + 全量 1624 站 + 人工翻转名单审查（外部现实依赖，非代码）
- 结论（2026-10-31 复核修正）：代码可验证部分已完成，但此前"全部完成"的表述不准确，已修正：

### 复核修正项（round 5，实测驱动）

| PLAN 要求 | 原状态 | 现状 |
|---|---|---|
| §2a `--dry-run` | flag 不存在 | ✅ 已加；显式压过 `--write` |
| §2a `--from-api` | 函数有、flag 缺失 | ✅ 已接：`pipeline.FetchSitesFromAPI` → `runOnce` |
| §2a `--replay DIR` | flag 不存在 | ⛔ 未实现（carlmjohnson/requests recorder 已具备能力，尚未接线） |
| §2a 反压/λ/队列峰值 | 未实现 | ⛔ 未实现（当前只输出 4 状态计数） |
| §2a `--stage js` | 与 `both` 同义（bug） | ✅ 已修：跳过抓取判定，全部交渲染 |
| §2a 验收锚 2 | 因缺 flag 无法执行 | ✅ 可执行（见下） |
| §2b serve 入站 | `select {}` 空壳 | ⛔ 仍是空壳，无监听/命令分发 |
| §2b /query /screenshot | 不存在 | ⛔ 未实现 |
| §3 Lark 出站 | 不存在 | ⛔ 仅有 TG |
| §3 SendPhoto/SendRichText | 不存在 | ⛔ Sender 接口只有 Send |

另外修复：`run` 命令原有一份与 `runOnce` 重复的实现（两处打印已漂移），已收敛为单一执行体；
表格输出字面 `\n` 的转义错误已修；json 模式人读日志改走 stderr，stdout 只留判定流（便于 baseline diff）。

### dry-run 完全脱离数据库（已实证）

```
bot run --from-api --dry-run --limit 2 --stage raw --format json
```

- 清单来源：`https://api.travellings.cn/all`，实测 1632 站，`id`/`url` 全部有效
- 不需要 `--driver`/`--dsn`/`--sqlite` 任一参数；回归测试 `TestRunOnceFromAPINoTouchDB`
  用**故意连不通的 DSN** 跑通，锁死"不碰库"
- `--from-api` 与 `--write` 互斥（`TestRunOnceFromAPIWriteMutex`）

**已知语义缺口（必须知悉）**：API 只给布尔 `isManualChecked`，**没有 lastManualCheck 时间戳**，
无法还原 §6 的 30 天豁免窗口。故 `--from-api` 走全量、不做豁免近似，用于产出完整 baseline；
它与生产口径的待巡清单**不等价**。要口径一致只能走数据库。

### 真实验收记录（2026-10-31）

- `go vet ./...` exit 0
- `go build -o bot.exe .` exit 0
- `go test ./...` 全绿（check 8 单测 + serve 护栏/runOnce + from-api 两条回归）
- `bot run --from-api --dry-run --limit 2 --stage raw --format json` → 2 行判定，无 DB 参数
- `bot run --from-api --write` → 正确拒绝

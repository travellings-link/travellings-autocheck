#!/usr/bin/env bash
# 主从对照回归：同一份清单，bot master(+一台 both worker) 与单机 bot run 逐站比对判定。
# 期望只差渲染竞态导致的个别站；大面积不一致 = 主从判定与单机分叉，是回归信号。
#
# 依赖：能访问 api.travellings.cn（用 --from-api 免配 DB）、本机有可回退的无头 Chrome、jq。
# 用法：MS_TOKEN=<口令> BOT=./bot ./deploy/regress.sh [站点数，默认 50]
set -euo pipefail

N="${1:-50}"
: "${MS_TOKEN:?设 MS_TOKEN，master 与 worker 共用}"
: "${BOT:?设 BOT 指向编译好的 bot 二进制}"
PORT="${PORT:-9500}"
OUT="$(mktemp -d)"
trap 'kill 0 2>/dev/null || true; rm -rf "$OUT"' EXIT

# 1) master 后台起（--from-api 只读、不写库），json 逐站输出落盘
"$BOT" master --from-api --limit "$N" --format json --token "$MS_TOKEN" \
    --listen "127.0.0.1:$PORT" >"$OUT/ms.json" 2>"$OUT/master.log" &
M=$!
# 等 master 就绪（清单灌完、gRPC 起来）后才放 worker，避免 worker 空连
until grep -q 'master 已就绪' "$OUT/master.log" 2>/dev/null; do sleep 0.3; done

# 2) 一台 both worker：既初审也渲染。缺 --browser-workers 会卡住收口（README 主从部署坑①）。
"$BOT" worker --master "127.0.0.1:$PORT" --token "$MS_TOKEN" \
    --http-workers 8 --browser-workers 4 >"$OUT/worker.log" 2>&1
wait "$M"   # master 收口后自己退出（OnClose→GracefulStop）

# 3) 同一清单单机再跑（--from-api 同取 api.travellings.cn、同 --limit）
"$BOT" run --from-api --limit "$N" --format json >"$OUT/run.json" 2>"$OUT/run.log"

# 4) 逐站 diff：按 id 排序、剔除天然会抖的 evidence 文本
norm() { jq -S 'del(.evidence)' -c "$1" | sort; }
echo "=== 判定差异（期望为空，或仅个别渲染竞态站）==="
diff <(norm "$OUT/run.json") <(norm "$OUT/ms.json") && echo "主从与单机判定一致"

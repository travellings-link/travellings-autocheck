package crawl

import (
	"context"
	"testing"
	"time"
)

// 连不上的 CDP 地址，probeRemote 必须返回错误（且不 hang）。
func TestProbeRemoteFailsFast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	err := probeRemote(ctx, "ws://127.0.0.1:9/devtools/browser")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("连不通的端口应返回错误")
	}
	// 探活要有自己的超时，不能依赖外层——否则首站会被拖死
	if elapsed > 15*time.Second {
		t.Fatalf("探测耗时 %v，超时保护可能失效", elapsed)
	}
}

// 可用的 CDP 地址应探活成功（本机 9222 在跑时）。
func TestProbeRemoteSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := probeRemote(ctx, "ws://127.0.0.1:9222/devtools/browser"); err != nil {
		t.Skipf("本机 9222 未运行，跳过：%v", err)
	}
}

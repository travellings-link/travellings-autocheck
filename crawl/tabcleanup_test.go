package crawl

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

// 兜底关闭必须在 Cancel 之后按 id 精确关一次，且拿到 id 后不能因
// 上下文已取消而放弃——forceCloseTab 用的是 background 派生超时。
func TestForceCloseTabIgnoresEmptyID(t *testing.T) {
	// 空 id 应静默返回：不能 panic，也不能把 nil Browser 的路径走到 cdp.Call。
	forceCloseTab(context.Background(), "")

	// 无 chromedp 上下文的 ctx 也不能炸。
	forceCloseTab(context.Background(), target.ID("page-1"))
}

// 自建标签页是这次修复的核心：id 必须在 attach 成功之前就握在手里。
// 这里固化「页面数量最终回到基线」这一可观测结论——泄漏修复的可执行断言。
func TestRenderedLeavesNoExtraTabs(t *testing.T) {
	CloseBrowser()
	defer CloseBrowser()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 先跑一次建立共享浏览器，取基线。
	if _, err := Rendered(ctx, "", "https://www.travellings.cn/go.html", 500*time.Millisecond, nil); err != nil {
		t.Skipf("无法建立浏览器，跳过: %v", err)
	}
	// 空闲时的 TabCount 是 0（常驻首页不计入），基线即「没有在渲染的页」。
	base := TabCount()
	if base != 0 {
		t.Fatalf("渲染刚结束时 Baseline 应为 0，得到 %d：上一站的页没关干净", base)
	}

	// 连跑 3 站，每站都应自己关干净。
	for i := 0; i < 3; i++ {
		if _, err := Rendered(ctx, "", "https://www.travellings.cn/go.html", 500*time.Millisecond, nil); err != nil {
			t.Fatalf("第 %d 次渲染失败: %v", i+1, err)
		}
	}

	// 标签页从列表消失有异步延迟，留一点余量再断言。
	deadline := time.Now().Add(5 * time.Second)
	var got int
	for {
		got = TabCount()
		if got <= base || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got > base {
		t.Fatalf("渲染后标签页泄漏: 基线 %d, 现状 %d", base, got)
	}
}

// forceCloseTab 的错误分支不该影响主流程：这里只确保它不 panic、不阻塞。
func TestForceCloseTabBounded(t *testing.T) {
	done := make(chan struct{})
	go func() {
		forceCloseTab(context.Background(), target.ID("not-a-real-target"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("forceCloseTab 未在 10s 内返回")
	}
}

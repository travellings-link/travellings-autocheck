package crawl

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// 收尾对账必须保住共享首标签页，只关多余的。
//
// 为什么这个测试重要：SweepTabs 是唯一一处「我们主动关别人的页」的代码。
// 一旦把 base.Target.TargetID 算错，整条 CDP 连接会被连带毁掉，
// 表现为下一轮所有站点直接失败——比标签页泄漏严重得多。
func TestSweepTabsKeepsSharedTarget(t *testing.T) {
	CloseBrowser()
	defer CloseBrowser()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if _, err := Rendered(ctx, "", "https://www.travellings.cn/go.html", 500*time.Millisecond, nil); err != nil {
		t.Skipf("无法建立浏览器，跳过: %v", err)
	}

	// 干净状态下不该有可关的页：共享页必须被保住。
	if n := SweepTabs(); n != 0 {
		t.Fatalf("干净状态下 SweepTabs 关了 %d 个页，共享页可能被误关", n)
	}
	// 共享首页本身不计数（TabCount 已排除各连接自带的常驻首页），
	// 所以这里断言的是「没有残留的在渲染页」——被误关的证据是下一句：浏览器还能继续干活。
	if got := TabCount(); got != 0 {
		t.Fatalf("收尾对账后仍残留 %d 个在渲染页", got)
	}

	// 对账之后浏览器还能继续干活——这是共享页没被毁掉的实证。
	if _, err := Rendered(ctx, "", "https://www.travellings.cn/go.html", 500*time.Millisecond, nil); err != nil {
		t.Fatalf("收尾对账后浏览器不可用: %v", err)
	}
}

// 对账必须真的关掉多余页，而不只是「不误关」。
//
// 这里手工造两个孤儿页，模拟 Rendered 在拥塞下关失败的情形，
// 验证 SweepTabs 能把它们清干净、共享页还在、浏览器仍可用。
func TestSweepTabsClosesOrphans(t *testing.T) {
	CloseBrowser()
	defer CloseBrowser()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if _, err := Rendered(ctx, "", "https://www.travellings.cn/go.html", 500*time.Millisecond, nil); err != nil {
		t.Skipf("无法建立浏览器，跳过: %v", err)
	}
	base := TabCount()

	sharedMu.Lock()
	sctx := sharedCtx
	sharedMu.Unlock()
	br := chromedp.FromContext(sctx)
	if br == nil || br.Browser == nil {
		t.Fatal("共享上下文里没有 Browser")
	}

	for i := 0; i < 2; i++ {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := cdp.Call(cctx, br.Browser, target.CreateTarget,
			target.CreateTargetParams{URL: "about:blank"})
		ccancel()
		if err != nil {
			t.Fatalf("造孤儿页失败: %v", err)
		}
	}
	if got := TabCount(); got < base+2 {
		t.Fatalf("孤儿页没建出来: base=%d now=%d", base, got)
	}

	if n := SweepTabs(); n != 2 {
		t.Fatalf("SweepTabs 关了 %d 个，期望 2 个", n)
	}
	// 关闭是异步生效的（chromedp 自己也用 waitTargetGone 轮询），
	// 立刻读会读到还没消失的页。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if TabCount() <= base {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := TabCount(); got > base {
		t.Fatalf("对账后仍残留标签页: base=%d now=%d", base, got)
	}

	// 共享页还活着：再跑一站必须成功。
	if _, err := Rendered(ctx, "", "https://www.travellings.cn/go.html", 500*time.Millisecond, nil); err != nil {
		t.Fatalf("对账后浏览器不可用: %v", err)
	}
}

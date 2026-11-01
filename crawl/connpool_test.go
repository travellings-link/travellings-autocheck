package crawl

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// testCDPURL 返回测试要连的 CDP 端点。
//
// 空串 = 本地自启无头 Chrome。连接池的核心行为（多条连接各自独立）只在
// 远程模式下成立——本地回退被强制压回 1 条。不设这个环境变量，那些断言
// 会被跳过，等于修复的核心路径没有测试。
//
//	$env:TRAVELLINGS_CDP_URL="ws://127.0.0.1:9222/devtools/browser"
func testCDPURL(t *testing.T) string {
	t.Helper()
	return os.Getenv("TRAVELLINGS_CDP_URL")
}

// 连接池必须真的建出配置数量的连接，而不是「配置了 N 条、实际只有 1 条」。
//
// 为什么这条断言重要：整个修复的前提就是并发度来自连接数。如果 SetConnCount(4)
// 之后池里只有 1 条，行为与修复前完全一致，而这个 bug 会在长时间压测后
// 才以「标签页又堆起来了」的形式重现——那时已经不在测试现场了。
//
// 只在远程 CDP 模式下有意义（本地回退被强制压回 1 条，见 SetConnCount 注释），
// 所以远程不可用时跳过而不是失败。
func TestConnPoolBuildsConfiguredCount(t *testing.T) {
	CloseBrowser()
	defer CloseBrowser()

	const want = 4
	SetConnCount(want)
	defer SetConnCount(1)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 一次 Rendered 触发建池（池是懒建的）。站点本身失败无所谓，
	// 这里只关心连接建了几条。
	cdpURL := testCDPURL(t)
	if cdpURL == "" {
		t.Skip("未设 TRAVELLINGS_CDP_URL，跳过远程连接池断言")
	}
	_, _ = Rendered(ctx, cdpURL, "https://www.travellings.cn/go.html", 500*time.Millisecond, nil)
	if !useRemote {
		t.Skip("CDP 探活失败，回退到本地单连接模式，不适用")
	}

	got := len(poolConns())
	if got != want {
		t.Fatalf("池里只有 %d 条连接，配置的是 %d 条；并发度会退化成单连接", got, want)
	}
	// 每条都必须是一个能回答 CDP 的活连接。
	for i, c := range poolConns() {
		if chromedp.FromContext(c.ctx) == nil {
			t.Fatalf("第 %d 条连接没有 chromedp 上下文", i)
		}
	}
}

// 租借与归还必须成对：一轮渲染跑完，空闲队列要恢复成和连接数一样多。
//
// 漏归还的后果不是死锁就是连接数被永久吃掉——32 个渲染 worker 抢一个
// 越来越小的池，最后全员阻塞在 acquireConn 上，表现为「巡查卡住不动」。
func TestConnPoolReleasesAll(t *testing.T) {
	CloseBrowser()
	defer CloseBrowser()

	const n = 3
	SetConnCount(n)
	defer SetConnCount(1)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if _, err := Rendered(ctx, "", "https://www.travellings.cn/go.html", 500*time.Millisecond, nil); err != nil {
		t.Skipf("无法建立浏览器，跳过: %v", err)
	}
	ents := poolConns()
	if len(ents) == 0 {
		t.Fatal("池为空")
	}

	// 全部租走再全部归还，归还后必须还能再租到同样多的次数。
	var held []*connEntry
	for i := 0; i < len(ents); i++ {
		e, err := acquireConn(ctx, "")
		if err != nil {
			t.Fatalf("第 %d 次租借失败: %v", i, err)
		}
		held = append(held, e)
	}
	for _, e := range held {
		releaseConn(e)
	}
	for i := 0; i < len(ents); i++ {
		e, err := acquireConn(ctx, "")
		if err != nil {
			t.Fatalf("归还后第 %d 次租借失败，说明有连接没还回去: %v", i, err)
		}
		releaseConn(e)
	}
}

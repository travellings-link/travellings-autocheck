package crawl

import (
	"context"
	"os"
	"testing"
	"time"
)

// TabCount 的数字由什么构成——先量清楚再谈上限。
//
// 假设：每条 CDP 连接都自带一个常驻首页（chromedp.Do 会建出 Target），
// 所以「池里 8 条连接」本身就已经是 8 个标签页，与正在渲染几站无关。
// 若假设成立，TabCount 的语义是「所有连接的首页 + 在渲染的页」，
// 那么把它和 --browser-workers 直接比较就没有意义——必须先减掉基线。
func TestTabCountBaseline(t *testing.T) {
	cdpURL := os.Getenv("TRAVELLINGS_CDP_URL")
	if cdpURL == "" {
		t.Skip("未设 TRAVELLINGS_CDP_URL，跳过真实 CDP 断言")
	}
	CloseBrowser()
	defer CloseBrowser()
	defer SetConnCount(1)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for _, conns := range []int{1, 4, 8} {
		CloseBrowser()
		SetConnCount(conns)
		if _, err := Rendered(ctx, cdpURL, "https://example.com/", 200*time.Millisecond, nil); err != nil {
			t.Skipf("无法渲染，跳过: %v", err)
		}
		if !useRemote {
			t.Skip("本地回退模式，不适用")
		}
		base := TabCount()
		t.Logf("连接数 %d → 空闲时 TabCount = %d（期望 0：只剩各连接自带的首页）", conns, base)
		if base != 0 {
			t.Errorf("空闲时 TabCount = %d，连接数是 %d：常驻首页被算进了渲染页数", base, conns)
		}
	}
}

package crawl

import (
	"context"
	"testing"
	"time"
)

// 失败路径必须和成功路径一样关干净。
//
// 为什么单独一个测试：线上那一轮 tabs 涨到 56（workers=32）时，
// 渲染只完成 5/183——绝大多数站点走的是**失败**分支，而原来的
// TestRenderedLeavesNoExtraTabs 只跑成功路径。失败分支的 defer
// 若漏了关闭，泄漏在成功路径的测试里根本看不见。
//
// 这里用必然失败的目标（保留域名 .invalid 按 RFC 6761 永不解析），
// 连跑多次模拟重试，标签页数必须回到基线。
func TestRenderedFailureStillClosesTab(t *testing.T) {
	CloseBrowser()
	defer CloseBrowser()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// 建共享浏览器并取基线。
	if _, err := Rendered(ctx, "", "https://www.travellings.cn/go.html", 500*time.Millisecond, nil); err != nil {
		t.Skipf("无法建立浏览器，跳过: %v", err)
	}
	base := TabCount()

	const rounds = 5
	for i := 0; i < rounds; i++ {
		// 失败是预期内的：只要不 panic、不泄漏就行。
		if _, err := Rendered(ctx, "", "https://nonexistent-host.invalid/", 300*time.Millisecond, nil); err == nil {
			t.Logf("第 %d 轮意外成功，不影响本测试的泄漏断言", i+1)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	var got int
	for {
		got = TabCount()
		if got <= base || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got > base {
		t.Fatalf("失败路径泄漏标签页: 基线 %d, %d 轮失败后 %d", base, rounds, got)
	}
}

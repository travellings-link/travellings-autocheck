package crawl

import (
	"context"
	"testing"
	"time"
)

// TabCount 在 CDP 拥塞（查询超时）时必须沿用上次成功的计数，
// 而不是返回 -1 让状态行的 tabs 段整块消失。
//
// 实测现场：并发渲染时共享 Browser 上 Target.getTargets 会 2 秒超时
// （context deadline exceeded），58 次采样 16 次 -1（27%）。
// 表现就是进度条上的 tabs 数字时有时无。
func TestTabCountKeepsLastValueOnCallError(t *testing.T) {
	CloseBrowser()
	lastTabCount.Store(0)
	if got := TabCount(); got != 0 {
		t.Fatalf("无浏览器: 期望沿用缓存 0, 得到 %d", got)
	}

	// 模拟"曾经成功数到 3 个标签页"
	lastTabCount.Store(3)

	// 制造 CDP 不可用：sharedCtx 存在但里面没有 Browser → 走失败分支
	sharedMu.Lock()
	saved := sharedCtx
	sharedCtx = context.Background()
	sharedMu.Unlock()
	defer func() {
		sharedMu.Lock()
		sharedCtx = saved
		sharedMu.Unlock()
		lastTabCount.Store(0)
	}()

	if got := TabCount(); got != 3 {
		t.Fatalf("CDP 不可用时应沿用上次计数 3, 得到 %d", got)
	}
}

// 成功查询后必须刷新缓存，否则数字会永远停在旧值。
func TestTabCountCachesLatest(t *testing.T) {
	CloseBrowser()
	lastTabCount.Store(0)
	defer func() {
		CloseBrowser()
		lastTabCount.Store(0)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 建立共享浏览器
	if _, err := Rendered(ctx, "", "https://www.travellings.cn/go.html", 500*time.Millisecond, nil); err != nil {
		t.Skipf("无法建立浏览器，跳过: %v", err)
	}
	got := TabCount()
	if got < 0 {
		t.Fatalf("浏览器已建立, TabCount 不应为 -1, 得到 %d", got)
	}
	if lastTabCount.Load() != int64(got) {
		t.Fatalf("缓存未刷新: TabCount=%d, cached=%d", got, lastTabCount.Load())
	}
}

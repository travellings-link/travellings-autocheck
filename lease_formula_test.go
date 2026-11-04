package main

import (
	"testing"
	"time"

	"travellings-bot-go/crawl"
)

// D11 租约不变量（树莓派 reclaim=1 周期性误回收的回归锁）：
// 默认 Browser 租约必须 > 单次渲染最坏预算 ×(1+重试)。旧公式 settle=8s 时
// 租约 79s < 预算 83s，慢站必被回收——这条测在那个公式下直接红。
func TestBrowserLeaseCoversTaskBudget(t *testing.T) {
	cases := []struct{ settleMS, retries int }{
		{8000, 2}, {8000, 0}, {2000, 2}, {30000, 2},
	}
	for _, c := range cases {
		lease := browserLease(c.settleMS, c.retries)
		worst := crawl.BrowserTaskBudget(time.Duration(c.settleMS)*time.Millisecond) *
			time.Duration(1+c.retries)
		if lease <= worst {
			t.Fatalf("settle=%dms retries=%d: lease=%v 覆盖不了最坏任务预算 %v",
				c.settleMS, c.retries, lease, worst)
		}
	}
	// 默认参数钉一个具体数，改动公式时必须有意识地来更新它：(8+75)s×3+15s = 4m24s
	if got := browserLease(8000, 2); got != 4*time.Minute+24*time.Second {
		t.Fatalf("默认租约应为 4m24s，got %v", got)
	}
}

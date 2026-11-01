package crawl

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 并发渲染时标签页数必须被连接数卡住。
//
// 这是把线上事故原样搬进测试：修复前 --browser-workers 32 全挤在 1 条连接上，
// 建页命令在 worker 里排队，超时后客户端放弃、服务端照建，兜底清理也排队超时，
// 于是 tabs 单调涨到 43、48 这种远超 workers 的值，worker RSS 跟着从 31MB 涨到 300MB+。
//
// 断言用 TabCount() 而不是外部探针：obscura 的 /json/list 只列浏览器自带的初始页，
// 看不到 CDP 建出来的页；Target.getTargets 又只列当前连接的页（每个连接一个独立 CdpContext）。
// TabCount 是唯一能跨所有连接数到真实页面数的地方，也正是状态行显示给用户的那个数。
func TestTabsBoundedByConnCount(t *testing.T) {
	cdpURL := os.Getenv("TRAVELLINGS_CDP_URL")
	if cdpURL == "" {
		t.Skip("未设 TRAVELLINGS_CDP_URL，跳过真实 CDP 并发断言")
	}

	CloseBrowser()
	defer CloseBrowser()

	const conns = 8    // 连接池大小
	const workers = 32 // 并发渲染数，与事故现场一致
	SetConnCount(conns)
	defer SetConnCount(1)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	// 先建池并确认真的走远程路径。
	if _, err := Rendered(ctx, cdpURL, "https://example.com/", 300*time.Millisecond, nil); err != nil {
		t.Skipf("无法渲染，跳过: %v", err)
	}
	if !useRemote {
		t.Skip("CDP 探活失败，回退本地模式，不适用")
	}

	var peak atomic.Int64
	stop := make(chan struct{})
	var sg sync.WaitGroup
	sg.Add(1)
	go func() {
		defer sg.Done()
		tk := time.NewTicker(150 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				n := int64(TabCount())
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = Rendered(ctx, cdpURL, "https://example.com/", 300*time.Millisecond, nil)
		}()
	}
	wg.Wait()
	close(stop)
	sg.Wait()

	got := int(peak.Load())
	// TabCount 已排除各连接自带的常驻首页，所以峰值理论上就是「同时在渲染的页数」，
	// 上限即连接数。租还连接与关页是异步的，采样可能撞上收尾瞬间，
	// 所以再宽 2 页容忍观测误差——但绝不允许「一条连接上挂十来个页」那种排队堆积。
	limit := conns + 2
	t.Logf("并发 %d，连接 %d，标签页峰值 %d（上限 %d = %d 在渲染 + 2 容差）",
		workers, conns, got, limit, conns)
	if got > limit {
		t.Fatalf("并发 %d 时标签页峰值 %d 超过上限 %d：建页速度没有被连接数卡住", workers, got, limit)
	}
}

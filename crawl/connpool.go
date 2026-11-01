package crawl

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/chromedp/chromedp"
)

// 渲染阶段使用的 CDP 连接池。
//
// 为什么需要多于一条连接——这是本文件存在的全部理由：
//
// Obscura 的 `serve --workers N` 起的是 N 个**独立进程**，主进程只按连接做
// round-robin（pool.rs:147 的 pick/next）。每条连接落到一个 worker 进程后，
// 该 worker 用一个单线程 LocalSet 串行处理这条连接上的**所有**命令
// （server.rs:1218 cdp_processor，一次只取一条），而 CreateTarget 又内嵌了
// 整站导航才回包（domains/target.rs）。于是：
//
//	1 条连接 + 32 路并发渲染 → 32 条 CreateTarget 排在同一个串行队列里
//	→ 排在后面的超过客户端 10s 预算 → 客户端放弃，但服务端照建不误
//	→ 孤儿页；连兜底用的 getTargets / closeTarget 也排在同一条队列里超时
//	→ 标签页单调累积，worker RSS 从 31MB 涨到 300MB+。
//
// 32 并发实测：1 条连接建 8 页要 13.9s；8 条连接各 1 并发只要 4.0s。
// 并行度来自**连接数**，不是 --workers。所以连接数必须可配，
// 默认跟随 --browser-workers（每个渲染 worker 一条，互不排队）。

// connEntry 是池里的一条连接：一个已 Dial 完成、持有 Browser 的 chromedp 上下文。
type connEntry struct {
	ctx       context.Context
	cancelCtx context.CancelFunc
	cancelAll context.CancelFunc
	pool      *connPool
}

type connPool struct {
	size   int
	cdpURL string
	root   context.Context // 进程级根 ctx：remote.Allocator 监听它，取消即拆连接
	cancel context.CancelFunc
	free   chan *connEntry
	done   chan struct{}

	mu     sync.Mutex
	all    []*connEntry
	closed bool

	initOnce sync.Once
	initErr  error
}

var (
	poolMu   sync.Mutex
	pool     *connPool
	poolSize = 1
)

// SetConnCount 设置本进程要维持的 CDP 连接数（由 --cdp-conns 决定）。
//
// 只影响下一轮巡查：已经建好的池不会被中途重建，否则在途渲染会拿到已拆的
// 连接。runOnce 在每轮开始时调用，CloseBrowser 之后按新值重新建池。
//
// 回退到本地无头 Chrome 时会被强制压回 1 条——本地模式下每条连接都是一个
// 独立的浏览器进程（ExecAllocator 每个上下文起一个），开 N 条就是 N 个 Chrome。
func SetConnCount(n int) {
	if n <= 0 {
		n = 1
	}
	poolMu.Lock()
	poolSize = n
	poolMu.Unlock()
}

// getPool 取出（或惰性创建）连接池的结构，此时还没有 Dial 任何连接。
func getPool(cdpURL string) *connPool {
	poolMu.Lock()
	defer poolMu.Unlock()
	if pool != nil {
		return pool
	}
	root, cancel := context.WithCancel(context.Background())
	pool = &connPool{
		size:   poolSize,
		cdpURL: cdpURL,
		root:   root,
		cancel: cancel,
		free:   make(chan *connEntry, poolSize),
		done:   make(chan struct{}),
	}
	return pool
}

// acquireConn 租一条连接，池空则阻塞等待归还。ctx 结束即放弃等待。
func acquireConn(ctx context.Context, cdpURL string) (*connEntry, error) {
	p := getPool(cdpURL)
	if err := p.init(); err != nil {
		return nil, err
	}
	select {
	case e := <-p.free:
		return e, nil
	case <-p.done:
		return nil, errors.New("浏览器连接池已关闭")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// releaseConn 归还连接。必须与 acquireConn 成对出现（Rendered 里用 defer）。
func releaseConn(e *connEntry) {
	if e == nil || e.pool == nil {
		return
	}
	e.pool.put(e)
}

func (p *connPool) put(e *connEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return // 池已拆，连接由 CloseBrowser 统一释放
	}
	select {
	case p.free <- e:
	default: // 容量就是 size，正常不可能塞满；塞不下说明状态坏了，丢弃即可
	}
}

// init 惰性建立全部连接，只做一次。
//
// 先串行建第一条：这一条会触发 probeOnce 判定远程是否可用，判定结果决定
// 剩下的建不建——本地回退模式下多建一条就是多一个浏览器进程。远程可用时
// 剩余的并行建（每条一次完整 CDP 握手，串行 32 条要好几秒）。
func (p *connPool) init() error {
	p.initOnce.Do(func() {
		first, err := p.dial()
		if err != nil {
			p.initErr = err
			return
		}
		p.add(first)
		setSharedConn(first.ctx) // TabCount / SweepTabs 的调试注入点与默认查询对象

		want := p.size
		if !useRemote || want <= 1 {
			p.fill()
			return
		}

		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			errs int
		)
		for i := 1; i < want; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				e, err := p.dial()
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs++
					return
				}
				p.add(e)
			}()
		}
		wg.Wait()
		if errs > 0 {
			log.Warn().Int("ok", len(p.all)).Int("want", want).
				Msg("部分 CDP 连接建立失败，按已建立的数量继续")
		}
		p.fill()
	})
	return p.initErr
}

func (p *connPool) add(e *connEntry) {
	p.mu.Lock()
	p.all = append(p.all, e)
	p.mu.Unlock()
}

// fill 把已建立的连接全部放进空闲队列。
func (p *connPool) fill() {
	p.mu.Lock()
	ents := append([]*connEntry(nil), p.all...)
	p.mu.Unlock()
	if len(ents) == 0 {
		p.initErr = errors.New("CDP 连接全部建立失败")
		return
	}
	for _, e := range ents {
		p.free <- e
	}
}

func (p *connPool) dial() (*connEntry, error) {
	allocCtx, cancelAll, err := openBrowser(p.root, p.cdpURL)
	if err != nil {
		return nil, err
	}
	// 每个上下文只建一次 Browser：这就是被复用的那一个，
	// 后续所有站点在它上面开标签页而不是重新 Dial。
	ctx, cancelCtx := chromedp.NewContext(allocCtx,
		chromedp.WithErrorf(func(string, ...any) {}))
	if err := chromedp.Do(ctx); err != nil {
		cancelCtx()
		cancelAll()
		return nil, fmt.Errorf("初始化浏览器: %w", err)
	}
	return &connEntry{ctx: ctx, cancelCtx: cancelCtx, cancelAll: cancelAll, pool: p}, nil
}

// close 释放池里全部连接。用 Cancel 而非直接 cancelCtx：Cancel 会等标签页
// 与连接真正拆除，直接取消只是发信号，进程退出时可能留下未回收的浏览器侧资源。
func (p *connPool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.done)
	ents := append([]*connEntry(nil), p.all...)
	p.mu.Unlock()

	for _, e := range ents {
		_ = chromedp.Cancel(e.ctx)
		e.cancelCtx()
		e.cancelAll()
	}
	p.cancel()
}

// poolConns 返回池里全部连接的快照（已建立的，不含正在建的）。
// 可能为空：池还没初始化，此时调用方应退化为「无可查」。
func poolConns() []*connEntry {
	poolMu.Lock()
	p := pool
	poolMu.Unlock()
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*connEntry(nil), p.all...)
}

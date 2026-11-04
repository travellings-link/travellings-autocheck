package master

import (
	"sync"
	"testing"
	"time"

	mspb "travellings-bot-go/proto/mspb"
)

// 惊群治理验收①：空腿的 Pull 无限挂起不返回，事件唤醒即时应答——
// 「新任务可派发？」由 master 状态变化决定，worker 不再轮询（用户设计：hold 到可确定）。
func TestPullHoldsAndWakesOnEnqueue(t *testing.T) {
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1"}}, 0)
	task := pull1(t, r, mspb.Lane_LANE_HTTP) // 唯一任务已离队 → Browser 腿此刻空

	// 起一个挂起的 Browser Pull。
	var got *mspb.PullResponse
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pr, err := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_BROWSER, Want: 1})
		if err != nil {
			t.Errorf("held pull: %v", err)
			return
		}
		got = pr
	}()
	time.Sleep(120 * time.Millisecond) // 让它确实挂进去
	if got != nil {
		t.Fatal("空腿 Pull 不该提前返回（惊群未消）")
	}
	// escalate 回灌 Browser 队列 → 挂起的 Pull 应被唤醒并领到任务。
	if _, err := rep(t, r, task, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "LOST", Escalate: true}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if got == nil || got.GetAction() != mspb.Action_ASSIGN || len(got.GetTasks()) != 1 {
		t.Fatalf("回灌应唤醒挂起 Pull 并派发, got %v", got)
	}
}

// 惊群治理验收②：收口广播——多个挂起 Pull 全部即时拿到 ROUND_DONE，无人超时陪跑。
func TestRoundDoneBroadcastWakesHolders(t *testing.T) {
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1"}}, 0)
	// 无超时兜底（新契约）：6 个挂起 Pull 只能靠收口广播返回——收不齐即测挂，语义更强。
	const holders = 6
	var wg sync.WaitGroup
	done := make(chan bool, holders)
	for i := 0; i < holders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pr, err := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_BROWSER, Want: 1})
			done <- err == nil && pr.GetAction() == mspb.Action_ROUND_DONE
		}()
	}
	time.Sleep(100 * time.Millisecond) // 全挂进去
	pr0, err := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if err != nil || len(pr0.GetTasks()) != 1 {
		t.Fatalf("http pull: err=%v n=%d", err, len(pr0.GetTasks()))
	}
	first := pr0.GetTasks()[0]
	if _, err := rep(t, r, first, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "RUN"}); err != nil {
		t.Fatal(err) // 唯一站终结 → 收口 → close(closedCh) 广播
	}
	wg.Wait()
	close(done)
	for ok := range done {
		if !ok {
			t.Fatal("有挂起 Pull 没收到哨兵")
		}
	}
}

// 回归锁（树莓派实测「30/31 卡住不派发」根因）：唤醒信号必须按腿隔离。
// 两腿各挂一个 Pull，只回灌 HTTP 腿 → HTTP 腿醒并领到，渲染腿不得被抢走信号（继续挂起）。
// 旧实现共享一颗 wake channel，select 随机挑接收者，回灌 HTTP 的信号可能被渲染腿空拿，
// HTTP 腿再无信号、任务永久滞留（无限挂起下连自愈都没有）——正是现场症状。
func TestWakeSignalIsPerLane(t *testing.T) {
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1"}, {Id: 2, Url: "u2"}}, 0)
	// 领走两站，HTTP 队列清空。
	h1 := pull1(t, r, mspb.Lane_LANE_HTTP)
	pull1(t, r, mspb.Lane_LANE_HTTP)

	type res struct {
		lane  mspb.Lane
		tasks int
	}
	httpCh := make(chan res, 1)
	brCh := make(chan res, 1)
	go func() { // 挂起的 HTTP Pull
		pr, _ := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
		httpCh <- res{mspb.Lane_LANE_HTTP, len(pr.GetTasks())}
	}()
	go func() { // 挂起的渲染 Pull
		pr, _ := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_BROWSER, Want: 1})
		brCh <- res{mspb.Lane_LANE_BROWSER, len(pr.GetTasks())}
	}()
	time.Sleep(150 * time.Millisecond) // 两个都挂进去

	// 只触发 HTTP 腿回灌：h1 报 ERROR → 重试回 queue[0] + signalLocked(0)
	if _, err := rep(t, r, h1, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "ERROR", Stage: "raw"}); err != nil {
		t.Fatal(err)
	}

	select {
	case g := <-httpCh:
		if g.tasks != 1 {
			t.Fatalf("HTTP 腿应被唤醒领到重试任务, got %d", g.tasks)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP 腿 Pull 未被唤醒——信号被别的腿抢走（跨腿串扰回归）")
	}
	// 渲染腿此刻必须还挂着（没被 HTTP 的信号空唤醒后误领/或替 HTTP 吃掉信号）。
	select {
	case g := <-brCh:
		t.Fatalf("渲染腿不该被 HTTP 回灌唤醒（信号未隔离），got tasks=%d", g.tasks)
	case <-time.After(200 * time.Millisecond):
		// 正确：仍挂起
	}
}

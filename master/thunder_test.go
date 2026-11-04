package master

import (
	"sync"
	"testing"
	"time"

	mspb "travellings-bot-go/proto/mspb"
)

// 惊群治理验收①：空腿的 Pull 挂起等待而非返回，事件唤醒即时应答——
// 「新任务可派发？」由 master 状态变化决定，worker 不再轮询。
func TestPullHoldsAndWakesOnEnqueue(t *testing.T) {
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1"}}, 0)
	r.SetPullMaxWait(5 * time.Second)
	task := pull1(t, r, mspb.Lane_LANE_HTTP) // 唯一任务已离队 → Browser 腿此刻空（未用到上限）

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
	r.SetPullMaxWait(30 * time.Second) // 全靠广播返回，超时只是保险
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

package master

import (
	"testing"
	"time"

	mspb "travellings-bot-go/proto/mspb"
)

func fixedClock() (func() time.Time, *time.Time) {
	cur := time.Unix(1700000000, 0)
	p := &cur
	return func() time.Time { return *p }, p
}

// 租约回收：worker 领走不回传 → 超时回灌原腿队列，可被另一 worker 重领。
func TestReclaimReturnsTaskToQueue(t *testing.T) {
	now, cur := fixedClock()
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1", Name: "n1"}}, 1)
	r.SetLease(30*time.Second, 30*time.Second, now)
	if pr, _ := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1}); len(pr.GetTasks()) != 1 {
		t.Fatal("领走 1 条")
	}
	// 未到租约：不回收。
	if got := r.Reclaim(now().Add(10 * time.Second)); got != 0 {
		t.Fatalf("未到期不该回收, got %d", got)
	}
	// 另一腿此刻领不到（HTTP 队列空、未收口→空批）。
	if pr, _ := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1}); len(pr.GetTasks()) != 0 {
		t.Fatal("回收前队列应为空")
	}
	*cur = now().Add(31 * time.Second)
	if got := r.Reclaim(*cur); got != 1 {
		t.Fatalf("到期应回收 1 条, got %d", got)
	}
	pr, _ := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if len(pr.GetTasks()) != 1 || pr.GetTasks()[0].GetName() != "n1" {
		t.Fatalf("回收后应可重领且 name 保留, got %d", len(pr.GetTasks()))
	}
	// 租约状态机保证不重复计数：重派后无论哪个 worker 先回传都消费租约(leased=0)，
	// 之后任何迟到/重复回传（leased=0，不匹配在途腿）一律丢弃、不推进 reports/responded。
	// 先让「当前持有者」正常回传终结这一站：
	if _, err := r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 1, Status: "RUN"}}); err != nil {
		t.Fatalf("holder report: %v", err)
	}
	_, _, _, _, repDone := r.Snapshot()
	// 老 worker 的幽灵回传此刻 leased=0 → 必须丢弃（幂等保证，收口不被重复推高）。
	if _, err := r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 1, Status: "RUN"}}); err != nil {
		t.Fatalf("ghost report: %v", err)
	}
	_, _, _, _, repAfter := r.Snapshot()
	if repDone != repAfter {
		t.Fatalf("终结后的重复/迟到回传不得计入 reports: %d→%d", repDone, repAfter)
	}
}

// 回收预算耗尽：按不写库终结（InfraErr），保证本轮收口不被反复炸的 worker 卡死。
func TestReclaimBudgetTerminates(t *testing.T) {
	now, cur := fixedClock()
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1"}}, 1)
	r.SetLease(10*time.Second, 10*time.Second, now)
	// HTTP 腿预算 retryTransient=1 → 第 2 次回收（recycles=2>1）终结。
	for i := 0; i < 2; i++ {
		r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
		*cur = cur.Add(11 * time.Second)
		r.Reclaim(*cur)
	}
	if !r.Completed() {
		t.Fatal("预算耗尽应终结收口")
	}
	fs := r.Verdicts()
	if len(fs) != 1 || fs[0].GetInfraErr() == "" {
		t.Fatalf("应产生带 InfraErr 的不写库终结, got %d 条", len(fs))
	}
	// 收口后 Pull 发哨兵。
	pr, _ := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if pr.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("收口后应发哨兵, got %v", pr.GetAction())
	}
}

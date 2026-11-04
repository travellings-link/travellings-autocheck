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

// 租约回收：领走不回传 → 超时回灌原腿队列，可被另一 worker 重领；
// 回收即作废 nonce——**同腿重派后老 worker 的迟到回传也被丢弃**（per-dispatch token
// 关死的窄窗，此前只能靠「租约>>预算」概率论证，现在有回归锁）。
func TestReclaimReturnsTaskToQueue(t *testing.T) {
	now, cur := fixedClock()
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1", Name: "n1"}}, 1)
	r.SetLease(30*time.Second, 30*time.Second, now)
	old := pull1(t, r, mspb.Lane_LANE_HTTP) // 第一次派发，记住其 nonce
	if got := r.Reclaim(now().Add(10 * time.Second)); got != 0 {
		t.Fatalf("未到期不该回收, got %d", got)
	}
	// 空腿未收口：Pull 无限挂起——不响应、不回空批，直到状态可确定（用户设计）。
	held := make(chan *mspb.PullResponse, 1)
	go func() {
		pr, err := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
		if err != nil {
			t.Errorf("held pull: %v", err)
		}
		held <- pr
	}()
	select {
	case <-held:
		t.Fatal("空腿 Pull 不该返回（新契约：无限挂起到状态确定）")
	case <-time.After(150 * time.Millisecond):
	}
	// 租约到期回收 → 回灌本腿队列 → 挂起的 Pull 被唤醒并领到重派任务。
	*cur = now().Add(31 * time.Second)
	if got := r.Reclaim(*cur); got != 1 {
		t.Fatalf("到期应回收 1 条, got %d", got)
	}
	var pr *mspb.PullResponse
	select {
	case pr = <-held:
	case <-time.After(2 * time.Second):
		t.Fatal("回收后挂起 Pull 未被唤醒（惊群未消或信号丢失）")
	}
	if len(pr.GetTasks()) != 1 {
		t.Fatalf("挂起 Pull 应被唤醒并领到 1 条, got %d", len(pr.GetTasks()))
	}
	fresh := pr.GetTasks()[0] // 重派：新 nonce
	if fresh.GetName() != "n1" || fresh.GetNonce() == old.GetNonce() {
		t.Fatalf("重派应保留 name 且 nonce 更新, got name=%q nonce=%d/%d", fresh.GetName(), fresh.GetNonce(), old.GetNonce())
	}
	// 窄窗回归锁：持有旧派发的“老 worker”迟到回传（同腿同站！）必须被丢弃。
	_, _ = r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 1, Nonce: old.GetNonce(), Status: "RUN"}})
	if _, _, _, _, rp := snap5(r); rp != 0 {
		t.Fatalf("旧 nonce 的迟到回传必须丢弃, reports=%d", rp)
	}
	// 新持有者正常回传 → 认账终结、收口发哨兵。
	rr, _ := rep(t, r, fresh, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "RUN"})
	if rr.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("新 nonce 应认账并收口, got %v", rr.GetAction())
	}
}

// 回收预算耗尽：按不写库终结（InfraErr），保证收口不被反复炸的 worker 卡死。
func TestReclaimBudgetTerminates(t *testing.T) {
	now, cur := fixedClock()
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1"}}, 1)
	r.SetLease(10*time.Second, 10*time.Second, now)
	// HTTP 腿预算 retryTransient=1 → 第 2 次回收（recycles=2>1）终结。
	for i := 0; i < 2; i++ {
		pull1(t, r, mspb.Lane_LANE_HTTP)
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
	// 收口后 Pull 立即发哨兵（不挂起——closedCh 已关）。
	st := time.Now()
	pr, _ := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	if pr.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("收口后应发哨兵, got %v", pr.GetAction())
	}
	if time.Since(st) > 20*time.Millisecond {
		t.Fatal("收口后的 Pull 不该挂起")
	}
}

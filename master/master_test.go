package master

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	mspb "travellings-bot-go/proto/mspb"
)

func tok(s string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", s))
}

func oneTask() []*mspb.Task { return []*mspb.Task{{Id: 1, Url: "u"}} }

// 非 token 一律拒。
func TestBadToken(t *testing.T) {
	r := NewRound("good", oneTask(), 0)
	if _, err := r.Pull(tok("bad"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1}); err == nil {
		t.Fatal("bad token pull: want error")
	}
}

// escalate 必须转 Browser 队列，不终结（HTTP 初判→渲染复核，worker 不原地升级）。
func TestEscalateGoesBrowser(t *testing.T) {
	r := NewRound("t", oneTask(), 0)
	if _, err := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1}); err != nil {
		t.Fatal(err)
	}
	rr, err := r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 1, Status: "LOST", Stage: "raw", Escalate: true}})
	if err != nil {
		t.Fatal(err)
	}
	if rr.GetAction() != mspb.Action_ASSIGN {
		t.Fatalf("escalate: want ASSIGN(继续), got %v", rr.GetAction())
	}
	// Browser 队列应能领到这条，HTTP 队列空。
	if p, _ := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_BROWSER, Want: 1}); len(p.GetTasks()) != 1 {
		t.Fatalf("escalate: browser queue want 1 task, got %d", len(p.GetTasks()))
	}
	if h, _, _, _, resp := r.Snapshot(); h != 1 || resp != 1 {
		t.Fatalf("escalate 后 HTTP 腿应派发1、累计回传1: http=%d reports=%d", h, resp)
	}
}

// ERROR 回 HTTP 队列重试 1 次，第 2 次仍 ERROR 才终结（预算耗尽）。
func TestErrorRetryOnce(t *testing.T) {
	r := NewRound("t", oneTask(), 0)
	r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1}) // httpDisp=1
	// 第 1 次 ERROR：回灌，未终结。
	rr1, _ := r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 1, Status: "ERROR", Stage: "raw"}})
	if rr1.GetAction() != mspb.Action_ASSIGN {
		t.Fatalf("error retry1: want ASSIGN, got %v", rr1.GetAction())
	}
	if _, _, _, _, rp := r.Snapshot(); rp != 1 {
		t.Fatalf("error retry1: 累计回传应为1, got %d", rp)
	}
	r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1}) // httpDisp=2
	// 第 2 次 ERROR：预算耗尽 → 终结 → 收口 → 哨兵。
	rr2, _ := r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 1, Status: "ERROR", Stage: "raw"}})
	if rr2.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("error retry exhausted: want ROUND_DONE, got %v", rr2.GetAction())
	}
}

// IGNORE（工具链/站点超时）回 Browser 队列重试，不终结。
func TestIgnoreRetriesBrowser(t *testing.T) {
	r := NewRound("t", oneTask(), 0)
	// 先 escalate 进 Browser 并领走（brDisp=1）。
	r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 1, Status: "LOST", Escalate: true}})
	r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_BROWSER, Want: 1}) // brDisp=1
	// Browser 回 IGNORE：回灌 Browser 队列，不终结。
	rr, _ := r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_BROWSER,
		Verdict: &mspb.Verdict{SiteId: 1, Stage: "js", NavTimeout: "nav timeout: x"}})
	if rr.GetAction() != mspb.Action_ASSIGN {
		t.Fatalf("ignore: want ASSIGN(重试), got %v", rr.GetAction())
	}
	if _, _, _, _, rp := r.Snapshot(); rp != 2 {
		t.Fatalf("ignore: 累计回传应为2(升级+ignore各一次), got %d", rp)
	}
	if p, _ := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_BROWSER, Want: 1}); len(p.GetTasks()) != 1 {
		t.Fatalf("ignore: browser 队列应有回灌任务, got %d", len(p.GetTasks()))
	}
}

// RUN 直接终结 → 收口 → 哨兵。
func TestRunTerminates(t *testing.T) {
	r := NewRound("t", oneTask(), 0)
	r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	rr, _ := r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 1, Status: "RUN", Stage: "raw"}})
	if rr.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("run: want ROUND_DONE, got %v", rr.GetAction())
	}
}

// Snapshot 口径：分母=已派+待派；升级转 Browser 会新增派发，把两腿分母各自推高。
func TestSnapshotProgressAccounting(t *testing.T) {
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1"}, {Id: 2, Url: "u2"}}, 0) // batch=0：want 决定批量
	// 全量灌 HTTP 后未派发：hd=0 ht=2，浏览器腿 0/0。
	if hd, ht, bd, bt, rep := r.Snapshot(); hd != 0 || ht != 2 || bd != 0 || bt != 0 || rep != 0 {
		t.Fatalf("初始: got %d/%d %d/%d rep=%d", hd, ht, bd, bt, rep)
	}
	r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 1, Status: "LOST", Escalate: true}}) // 1 号升级转 Browser
	r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 2, Status: "LOST", Escalate: true}}) // 2 号也升级
	// HTTP 腿：已派 2、累计 2（都升级走了）；Browser 腿：2 条待派。
	hdp, htp, bdp, btp, rep := r.Snapshot()
	if hdp != 2 || htp != 2 || rep != 2 {
		t.Fatalf("HTTP 腿后: http=%d/%d rep=%d", hdp, htp, rep)
	}
	if bdp != 0 || btp != 2 {
		t.Fatalf("Browser 待派应为 0/2, got %d/%d", bdp, btp)
	}
	r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_BROWSER, Want: 2})
	r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_BROWSER,
		Verdict: &mspb.Verdict{SiteId: 1, Status: "RUN", Stage: "js"}})
	r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_BROWSER,
		Verdict: &mspb.Verdict{SiteId: 2, Status: "RUN", Stage: "js"}})
	// 收口：Browser 已派 2/累计 2，所有任务回传 4。
	if _, _, bdp, btp, rep := r.Snapshot(); bdp != 2 || btp != 2 || rep != 4 {
		t.Fatalf("收口: browser=%d/%d rep=%d", bdp, btp, rep)
	}
}

// 已响应队列 = 终判集（MS_TASK §3）：只有终结的站进 Verdicts()，
// escalate/重试的中间回传不进，且拷贝返回不受后续回传影响。
func TestVerdictsTerminalOnly(t *testing.T) {
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1"}, {Id: 2, Url: "u2"}}, 0)
	r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_HTTP, Want: 1})
	// 1 号 escalate：不终结，不该进 final。
	r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 1, Status: "LOST", Escalate: true}})
	if got := r.Verdicts(); len(got) != 0 {
		t.Fatalf("escalate 不该终结: final=%d", len(got))
	}
	// 2 号 RUN：终结 → 进 final 一条，且收口发哨兵。
	rr, _ := r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 2, Status: "RUN", Stage: "raw"}})
	fs := r.Verdicts()
	// 1 号 escalate 后浏览器队列还有活，responded 只到 1（2 号），未收口。
	if len(fs) != 1 || fs[0].GetSiteId() != 2 {
		t.Fatalf("final 应只含终结的 2 号: %d 条", len(fs))
	}
	if rr.GetAction() != mspb.Action_ASSIGN {
		t.Fatalf("1 号仍在浏览器腿待判，未收口，want ASSIGN got %v", rr.GetAction())
	}
	// 1 号浏览器腿出 RUN 终判 → 收口，final 两条。
	r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_BROWSER, Want: 1})
	rr2, _ := r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_BROWSER,
		Verdict: &mspb.Verdict{SiteId: 1, Status: "RUN", Stage: "js"}})
	if rr2.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("两站齐了应收口哨兵, got %v", rr2.GetAction())
	}
	if got := r.Verdicts(); len(got) != 2 {
		t.Fatalf("收口 final 应 2 条, got %d", len(got))
	}
}

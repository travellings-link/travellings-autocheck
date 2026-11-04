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

// snap5 兼容旧五值解构写法，测试主体不随 Snapshot 返回结构变更而重写。
func snap5(r *Round) (hd, ht, bd, bt, rep int) {
	p := r.Snapshot()
	return p.HTTPDisp, p.HTTPTotal, p.BrowserDisp, p.BrowserTotal, p.Reports
}

// pull1 从指定腿领 1 条（假定队列非空→立即返回，不会挂起），返回带 nonce 的 task。
// 调用方把返回的 task 交给 rep，nonce 就自动对得上——测试不手工拼凭据。
func pull1(t *testing.T, r *Round, lane mspb.Lane) *mspb.Task {
	t.Helper()
	pr, err := r.Pull(tok("t"), &mspb.PullRequest{Lane: lane, Want: 1})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if pr.GetAction() != mspb.Action_ASSIGN || len(pr.GetTasks()) != 1 {
		t.Fatalf("want 1 ASSIGN task, got action=%v n=%d", pr.GetAction(), len(pr.GetTasks()))
	}
	return pr.GetTasks()[0]
}

// rep 用刚领到的 task 的 nonce 回传一条判定（腿/站点/凭据三者绑定，杜绝漏 nonce）。
func rep(t *testing.T, r *Round, task *mspb.Task, lane mspb.Lane, v *mspb.Verdict) (*mspb.ReportResponse, error) {
	t.Helper()
	// 镜像 judgeHTTP：四要素全部来自领取的 task（测试不手工拼凭据）。
	v.SiteId = task.GetId()
	v.Name = task.GetName()
	v.Url = task.GetUrl()
	v.Nonce = task.GetNonce()
	return r.Report(tok("t"), &mspb.ReportRequest{Lane: lane, Verdict: v})
}

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
	task := pull1(t, r, mspb.Lane_LANE_HTTP)
	rr, err := rep(t, r, task, mspb.Lane_LANE_HTTP,
		&mspb.Verdict{Status: "LOST", Stage: "raw", Escalate: true})
	if err != nil {
		t.Fatal(err)
	}
	if rr.GetAction() != mspb.Action_ASSIGN {
		t.Fatalf("escalate: want ASSIGN(继续), got %v", rr.GetAction())
	}
	// escalate 唤醒挂起/后续 Pull：Browser 队列应能领到这条（有活立即返回，不挂起）。
	if p, _ := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_BROWSER, Want: 1}); len(p.GetTasks()) != 1 {
		t.Fatalf("escalate: browser queue want 1 task, got %d", len(p.GetTasks()))
	}
	if h, _, _, _, resp := snap5(r); h != 1 || resp != 1 {
		t.Fatalf("escalate 后 HTTP 腿应派发1、累计回传1: http=%d reports=%d", h, resp)
	}
}

// ERROR 回 HTTP 队列重试 1 次，第 2 次仍 ERROR 才终结（预算耗尽）。
func TestErrorRetryOnce(t *testing.T) {
	r := NewRound("t", oneTask(), 0)
	task1 := pull1(t, r, mspb.Lane_LANE_HTTP) // httpDisp=1
	// 第 1 次 ERROR：回灌，未终结。
	rr1, _ := rep(t, r, task1, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "ERROR", Stage: "raw"})
	if rr1.GetAction() != mspb.Action_ASSIGN {
		t.Fatalf("error retry1: want ASSIGN, got %v", rr1.GetAction())
	}
	if _, _, _, _, rp := snap5(r); rp != 1 {
		t.Fatalf("error retry1: 累计回传应为1, got %d", rp)
	}
	task2 := pull1(t, r, mspb.Lane_LANE_HTTP) // httpDisp=2（回灌后可重领）
	// 第 2 次 ERROR：预算耗尽 → 终结 → 收口 → 哨兵。
	rr2, _ := rep(t, r, task2, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "ERROR", Stage: "raw"})
	if rr2.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("error retry exhausted: want ROUND_DONE, got %v", rr2.GetAction())
	}
}

// IGNORE（工具链/站点超时）回 Browser 队列重试，不终结。
func TestIgnoreRetriesBrowser(t *testing.T) {
	r := NewRound("t", oneTask(), 0)
	h := pull1(t, r, mspb.Lane_LANE_HTTP)
	rep(t, r, h, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "LOST", Escalate: true}) // escalate→Browser
	b := pull1(t, r, mspb.Lane_LANE_BROWSER)                                         // brDisp=1
	rr, _ := rep(t, r, b, mspb.Lane_LANE_BROWSER, &mspb.Verdict{Stage: "js", NavTimeout: "nav timeout: x"})
	if rr.GetAction() != mspb.Action_ASSIGN {
		t.Fatalf("ignore: want ASSIGN(重试), got %v", rr.GetAction())
	}
	if _, _, _, _, rp := snap5(r); rp != 2 {
		t.Fatalf("ignore: 累计回传应为2(升级+ignore各一次), got %d", rp)
	}
	if p, _ := r.Pull(tok("t"), &mspb.PullRequest{Lane: mspb.Lane_LANE_BROWSER, Want: 1}); len(p.GetTasks()) != 1 {
		t.Fatalf("ignore: browser 队列应有回灌任务, got %d", len(p.GetTasks()))
	}
}

// RUN 直接终结 → 收口 → 哨兵。
func TestRunTerminates(t *testing.T) {
	r := NewRound("t", oneTask(), 0)
	task := pull1(t, r, mspb.Lane_LANE_HTTP)
	rr, _ := rep(t, r, task, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "RUN", Stage: "raw"})
	if rr.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("run: want ROUND_DONE, got %v", rr.GetAction())
	}
	if p := r.Snapshot(); p.HTTPDone != 1 || p.BrowserDone != 0 || p.Reports != 1 {
		t.Fatalf("响应计数应归 HTTP 腿: %+v", p)
	}
}

// 迟到/重复/错 nonce 的回传一律丢弃（per-dispatch token 的守卫）。
func TestStaleNonceDropped(t *testing.T) {
	r := NewRound("t", oneTask(), 0)
	task := pull1(t, r, mspb.Lane_LANE_HTTP)
	// 用错误 nonce 回传：不认账，reports 不推进。
	_, _ = r.Report(tok("t"), &mspb.ReportRequest{Lane: mspb.Lane_LANE_HTTP,
		Verdict: &mspb.Verdict{SiteId: 1, Nonce: task.GetNonce() + 999, Status: "RUN", Stage: "raw"}})
	if _, _, _, _, rp := snap5(r); rp != 0 {
		t.Fatalf("错 nonce 回传必须丢弃, reports=%d", rp)
	}
	// 正确 nonce 回传：认账。
	rr, _ := rep(t, r, task, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "RUN", Stage: "raw"})
	if rr.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("正确 nonce 应认账收口, got %v", rr.GetAction())
	}
}

// Snapshot 口径：分母=已派+待派；升级转 Browser 会新增派发，把两腿分母各自推高。
func TestSnapshotProgressAccounting(t *testing.T) {
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1"}, {Id: 2, Url: "u2"}}, 0)
	if hd, ht, bd, bt, rep := snap5(r); hd != 0 || ht != 2 || bd != 0 || bt != 0 || rep != 0 {
		t.Fatalf("初始: got %d/%d %d/%d rep=%d", hd, ht, bd, bt, rep)
	}
	t1 := pull1(t, r, mspb.Lane_LANE_HTTP)
	rep(t, r, t1, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "LOST", Escalate: true}) // 1 号升级
	t2 := pull1(t, r, mspb.Lane_LANE_HTTP)
	rep(t, r, t2, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "LOST", Escalate: true}) // 2 号升级
	hdp, htp, bdp, btp, rr := snap5(r)
	if hdp != 2 || htp != 2 || rr != 2 {
		t.Fatalf("HTTP 腿后: http=%d/%d rep=%d", hdp, htp, rr)
	}
	if bdp != 0 || btp != 2 {
		t.Fatalf("Browser 待派应为 0/2, got %d/%d", bdp, btp)
	}
	b1 := pull1(t, r, mspb.Lane_LANE_BROWSER)
	rep(t, r, b1, mspb.Lane_LANE_BROWSER, &mspb.Verdict{Status: "RUN", Stage: "js"})
	b2 := pull1(t, r, mspb.Lane_LANE_BROWSER)
	rep(t, r, b2, mspb.Lane_LANE_BROWSER, &mspb.Verdict{Status: "RUN", Stage: "js"})
	if _, _, bdp, btp, rr := snap5(r); bdp != 2 || btp != 2 || rr != 4 {
		t.Fatalf("收口: browser=%d/%d rep=%d", bdp, btp, rr)
	}
}

// 已响应队列 = 终判集：只有终结的站进 Verdicts()，escalate/重试中间回传不进。
func TestVerdictsTerminalOnly(t *testing.T) {
	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1"}, {Id: 2, Url: "u2"}}, 0)
	t1 := pull1(t, r, mspb.Lane_LANE_HTTP)
	t2 := pull1(t, r, mspb.Lane_LANE_HTTP)
	// 1 号 escalate：不终结，不该进 final。
	rep(t, r, t1, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "LOST", Escalate: true})
	if got := r.Verdicts(); len(got) != 0 {
		t.Fatalf("escalate 不该终结: final=%d", len(got))
	}
	// 2 号 RUN：终结 → 进 final 一条；1 号仍在浏览器腿待判 → 未收口。
	rr, _ := rep(t, r, t2, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "RUN", Stage: "raw"})
	fs := r.Verdicts()
	if len(fs) != 1 || fs[0].GetSiteId() != 2 {
		t.Fatalf("final 应只含终结的 2 号: %d 条", len(fs))
	}
	if rr.GetAction() != mspb.Action_ASSIGN {
		t.Fatalf("1 号仍在浏览器腿待判，未收口，want ASSIGN got %v", rr.GetAction())
	}
	// 1 号浏览器腿出 RUN 终判 → 收口，final 两条。
	b1 := pull1(t, r, mspb.Lane_LANE_BROWSER)
	rr2, _ := rep(t, r, b1, mspb.Lane_LANE_BROWSER, &mspb.Verdict{Status: "RUN", Stage: "js"})
	if rr2.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("两站齐了应收口哨兵, got %v", rr2.GetAction())
	}
	if got := r.Verdicts(); len(got) != 2 {
		t.Fatalf("收口 final 应 2 条, got %d", len(got))
	}
}

// 基准语义（--http-retries 0）：HTTP 腿 ERROR 不重派、直接终结——派发数不再因重试
// 虚增，与单机 bot run（HTTP 腿从不重试）逐条等价。锁住「参数完全对应单机」的承诺。
func TestHTTPRetriesZeroMatchesStandalone(t *testing.T) {
	r := NewRound("t", oneTask(), 0)
	r.SetHTTPRetries(0)
	task := pull1(t, r, mspb.Lane_LANE_HTTP)
	rr, _ := rep(t, r, task, mspb.Lane_LANE_HTTP, &mspb.Verdict{Status: "ERROR", Stage: "raw"})
	if rr.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("0 预算下首个 ERROR 即终结收口, got %v", rr.GetAction())
	}
	if p := snapStruct(r); p.HTTPDisp != 1 {
		t.Fatalf("0 预算下不应有第二次派发: disp=%d", p.HTTPDisp)
	}
}

// snapStruct 便捷取 master.Progress（回归测里解构五值太啰嗦）。
func snapStruct(r *Round) Progress { return r.Snapshot() }

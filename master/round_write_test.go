package master

import (
	"path/filepath"
	"testing"

	"travellings-bot-go/db"
	"travellings-bot-go/internal/report"
	mspb "travellings-bot-go/proto/mspb"
)

// M2c 端到端：worker 回传的 proto Verdict → ToVerdicts 搬运 → report.FinishRound 写回，
// 证明 master 真实走的写库链字段无损、闸门与 WAIT 语义生效（锁住 M2 的「闸门→写库」验收）。
func TestRoundWritesBackEnd(t *testing.T) {
	d, err := db.Open("sqlite", filepath.Join(t.TempDir(), "webs.db"), "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := d.Exec("CREATE TABLE webs (id INTEGER PRIMARY KEY, status TEXT, name TEXT, link TEXT, " +
		"tag TEXT, failedReason TEXT, lastManualCheck DATETIME)").Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	// 两站：id=1 将被判 RUN，id=2 人工 WAIT。
	for _, w := range []struct {
		id     int
		status string
	}{{1, "RUN"}, {2, "WAIT"}} {
		if err := d.Exec("INSERT INTO webs (id, status, name, link) VALUES (?, ?, ?, ?)",
			w.id, w.status, "n", "l").Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	t.Cleanup(func() {
		if sql, e := d.DB(); e == nil {
			_ = sql.Close()
		}
	})

	r := NewRound("t", []*mspb.Task{{Id: 1, Url: "u1", Name: "一号"}, {Id: 2, Url: "u2", Name: "二号"}}, 0)
	// 逐条领→逐条回传（helper 自动携带 per-dispatch nonce）。
	t1 := pull1(t, r, mspb.Lane_LANE_HTTP) // 1 号含入口判 RUN
	if _, err := rep(t, r, t1, mspb.Lane_LANE_HTTP,
		&mspb.Verdict{Url: "u1", Status: "RUN", Stage: "raw"}); err != nil {
		t.Fatal(err)
	}
	t2 := pull1(t, r, mspb.Lane_LANE_HTTP) // 2 号无入口判 LOST（带 evidence→写进 failedReason）
	rr, _ := rep(t, r, t2, mspb.Lane_LANE_HTTP,
		&mspb.Verdict{Url: "u2", Status: "LOST", Stage: "raw", Evidence: "no entry"})
	if rr.GetAction() != mspb.Action_ROUND_DONE {
		t.Fatalf("两站齐了应收口, got %v", rr.GetAction())
	}

	vs := r.ToVerdicts()
	if len(vs) != 2 {
		t.Fatalf("终判应 2 条, got %d", len(vs))
	}
	if vs[0].Name != "一号" {
		t.Fatalf("name 未从 proto 搬运到 pipeline.Verdict: %q", vs[0].Name)
	}

	// 全量轮（limit=0）走占比闸门：2 站 1 RUN = 50% ≥ 50 → 写库。
	written, ferr := report.FinishRound(d, vs, 50, 0)
	if ferr != nil || !written {
		t.Fatalf("应过闸门写库: written=%v err=%v", written, ferr)
	}
	var st1, st2, fr2 string
	d.Raw("SELECT status FROM webs WHERE id=1").Row().Scan(&st1)
	d.Raw("SELECT status, failedReason FROM webs WHERE id=2").Row().Scan(&st2, &fr2)
	if st1 != "RUN" {
		t.Errorf("id1 应写 RUN, got %s", st1)
	}
	// WAIT 不变量（SQL 层）：WAIT 站判 LOST 应保持 WAIT。
	if st2 != "WAIT" {
		t.Errorf("id2 WAIT 站判 LOST 应保持 WAIT, got %s", st2)
	}
}

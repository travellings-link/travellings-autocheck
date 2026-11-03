package report

import (
	"path/filepath"
	"testing"

	"travellings-bot-go/check"
	"travellings-bot-go/db"
	"travellings-bot-go/pipeline"

	"gorm.io/gorm"
)

// 内联建库（同 run_test.go 约定：clone 即可跑，不依赖外部 db 文件）。
func newWebs(t *testing.T) *gorm.DB {
	t.Helper()
	d, err := db.Open("sqlite", filepath.Join(t.TempDir(), "webs.db"), "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := d.Exec("CREATE TABLE webs (id INTEGER PRIMARY KEY, status TEXT, name TEXT, link TEXT, " +
		"tag TEXT, failedReason TEXT, lastManualCheck DATETIME)").Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, w := range []struct {
		id     int
		status string
		link   string
	}{{1, "RUN", "a"}, {2, "LOST", "b"}, {3, "WAIT", "c"}} {
		if err := d.Exec("INSERT INTO webs (id, status, name, link) VALUES (?, ?, ?, ?)",
			w.id, w.status, "n", w.link).Error; err != nil {
			t.Fatalf("seed %d: %v", w.id, err)
		}
	}
	t.Cleanup(func() {
		if sql, e := d.DB(); e == nil {
			_ = sql.Close()
		}
	})
	return d
}

// 达标轮：写回只动可写站点，工具链故障/站点超时跳过，WAIT 保持不变。
func TestFinalizeWritesAndSkipsIgnored(t *testing.T) {
	d := newWebs(t)
	vs := []pipeline.Verdict{
		{SiteID: 1, Status: check.Run},
		{SiteID: 2, Status: check.Lost, Evidence: "no entry"},
		{SiteID: 3, Status: check.Run}, // 人工 WAIT 站，本轮判 RUN → 应写 RUN
		{SiteID: 4, Status: check.Error, InfraErr: "cdp down"},
	}
	written, err := Finalize(d, vs, 50)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !written {
		t.Fatal("3/4 可写站 RUN 占比 75%，应过闸门写库")
	}
	var st1, st3 string
	d.Raw("SELECT status FROM webs WHERE id=1").Row().Scan(&st1)
	d.Raw("SELECT status FROM webs WHERE id=3").Row().Scan(&st3)
	if st1 != "RUN" {
		t.Errorf("id1 应写 RUN, got %s", st1)
	}
	if st3 != "RUN" {
		t.Errorf("id3 WAIT→RUN 允许写, got %s", st3)
	}
}

// 未达标轮：整轮丢弃不写库（written=false），库里 status 原样。
func TestFinalizeGateDropsRound(t *testing.T) {
	d := newWebs(t)
	vs := []pipeline.Verdict{
		{SiteID: 1, Status: check.Run},
		{SiteID: 2, Status: check.Lost},
		{SiteID: 3, Status: check.Lost},
		{SiteID: 4, Status: check.Lost},
	}
	written, err := Finalize(d, vs, 50)
	if err != nil || written {
		t.Fatalf("RUN 占比 25%% < 50 应丢弃: written=%v err=%v", written, err)
	}
	var st2 string
	d.Raw("SELECT status FROM webs WHERE id=2").Row().Scan(&st2)
	if st2 != "LOST" {
		t.Errorf("丢弃轮不得改库, got %s", st2)
	}
}

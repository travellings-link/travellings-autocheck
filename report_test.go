package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"travellings-bot-go/check"
	"travellings-bot-go/internal/ui"
	"travellings-bot-go/pipeline"
)

// 人类可读报表必须列出问题站点明细，而不是只给一行计数。
func TestPrintReportListsProblemSites(t *testing.T) {
	vs := []pipeline.Verdict{
		{SiteID: 1, URL: "https://a.example", Status: check.Run, Stage: check.Raw, Evidence: "ok"},
		{SiteID: 2, URL: "https://b.example", Status: check.Lost, Stage: check.JS, Evidence: "no entry after render"},
		{SiteID: 3, URL: "https://c.example", Status: check.Error, Stage: check.Raw, Evidence: "HTTP 403"},
	}
	var buf bytes.Buffer
	printReport(&buf, vs, 1234*time.Millisecond, false)
	got := buf.String()

	for _, want := range []string{"巡查完成", "3 站", "状态分布", "问题站点（2）", "https://b.example", "https://c.example"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q\n---\n%s", want, got)
		}
	}
	// RUN 站不进问题列表
	if strings.Contains(got, "https://a.example") {
		t.Errorf("RUN site must not be listed as problem\n%s", got)
	}
	if !strings.Contains(got, "1.234s") {
		t.Errorf("elapsed missing\n%s", got)
	}
}

// 中文表头不能把列挤歪：CJK 记 2 列。
func TestPadCJKWidth(t *testing.T) {
	if got := ui.DisplayWidth("判定"); got != 4 {
		t.Fatalf("ui.DisplayWidth(判定) = %d, want 4", got)
	}
	if got := ui.DisplayWidth("raw"); got != 3 {
		t.Fatalf("ui.DisplayWidth(raw) = %d, want 3", got)
	}
	if ui.Pad("判定", 6) != "判定  " {
		t.Fatalf("ui.Pad(判定,6) = %q", ui.Pad("判定", 6))
	}
}

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"travellings-bot-go/check"
	"travellings-bot-go/pipeline"
)

// IGNORE 必须把"不写库"的两类结果合并成一项，
// 且在明细表里显示为 IGNORE（原始 status 仍在 evidence 里）。
func TestReportShowsIgnoreBucket(t *testing.T) {
	vs := []pipeline.Verdict{
		{SiteID: 1, URL: "https://ok.example", Status: check.Run, Stage: check.Raw},
		{SiteID: 2, URL: "https://slow.example", Status: check.Timeout, Stage: check.JS,
			Evidence: "nav timeout: exceeded", NavTimeout: "exceeded"},
		{SiteID: 3, URL: "https://cdp.example", Status: check.Error, Stage: check.JS,
			Evidence: "infra js-leg: attach cdp failed", InfraErr: "attach cdp failed"},
	}
	var buf bytes.Buffer
	printReport(&buf, vs, time.Second, false)
	out := buf.String()

	if !strings.Contains(out, "IGNORE 2") {
		t.Errorf("状态分布应出现 IGNORE 2：\n%s", out)
	}
	// 细分原因：工具链故障 1 / 站点超时 1
	if !strings.Contains(out, "工具链故障 1") || !strings.Contains(out, "站点超时 1") {
		t.Errorf("头部应给出 IGNORE 的细分原因：\n%s", out)
	}
	// 冗余表述不该出现
	if strings.Contains(out, "未写库") {
		t.Errorf("IGNORE 已表达不写库，不应再写未写库：\n%s", out)
	}
}

// 没有不写库结果时，不应出现 IGNORE 行。
func TestReportNoIgnoreWhenClean(t *testing.T) {
	vs := []pipeline.Verdict{
		{SiteID: 1, Status: check.Run, Stage: check.Raw},
		{SiteID: 2, Status: check.Lost, Stage: check.JS},
	}
	var buf bytes.Buffer
	printReport(&buf, vs, time.Second, false)
	if strings.Contains(buf.String(), "IGNORE") {
		t.Errorf("无 IGNORE 时不该出现该项：\n%s", buf.String())
	}
}

// isIgnore 的语义边界
func TestIsIgnore(t *testing.T) {
	if !isIgnore(pipeline.Verdict{InfraErr: "x"}) {
		t.Error("InfraErr 应判 IGNORE")
	}
	if !isIgnore(pipeline.Verdict{NavTimeout: "x"}) {
		t.Error("NavTimeout 应判 IGNORE")
	}
	if isIgnore(pipeline.Verdict{Status: check.Error}) {
		t.Error("普通 ERROR 不应判 IGNORE")
	}
}

package main

import (
	"testing"

	"travellings-bot-go/check"
	"travellings-bot-go/pipeline"
)

// 工具链故障（CDP 挂掉、导航超时）绝不能写进数据库——
// 那是我们自己的问题，写进去等于用故障判站点死刑。
// 真实案例：wznmickey.com 因渲染超时被判 ERROR，实际站点有链接。
func TestInfraErrorNotWrittenToDB(t *testing.T) {
	vs := []pipeline.Verdict{
		{SiteID: 1, URL: "https://ok.example", Status: check.Run, Stage: check.JS, Evidence: "ok"},
		{SiteID: 15, URL: "https://wznmickey.com", Status: check.Error, Stage: check.JS, Evidence: "infra js-leg: navigate timeout", InfraErr: "navigate timeout"},
	}
	ups := verdicts2updates(vs)
	if len(ups) != 1 {
		t.Fatalf("infra 故障必须被跳过，期望 1 条，实际 %d 条", len(ups))
	}
	if ups[0].SiteID != 1 {
		t.Errorf("应保留正常站点 id=1，实际 %d", ups[0].SiteID)
	}
}

// 全部因工具链故障失败时，不应产生任何写库更新。
func TestAllInfraErrYieldsNoUpdates(t *testing.T) {
	vs := []pipeline.Verdict{
		{SiteID: 1, Status: check.Error, InfraErr: "cdp down"},
		{SiteID: 2, Status: check.Error, InfraErr: "timeout"},
	}
	if ups := verdicts2updates(vs); len(ups) != 0 {
		t.Fatalf("全部 infra 故障时不应写库，实际 %d 条", len(ups))
	}
}

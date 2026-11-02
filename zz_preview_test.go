package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"travellings-bot-go/check"
	"travellings-bot-go/pipeline"
)

// .card 是飞书「卡片搭建工具」的模板文件格式：{name, dsl, variables}。
// 生成两份样例便于在 open.feishu.cn 卡片预览器里直接打开对照；
// webhook 实际发送用的是 dsl 本体 + msg_type=interactive 外层（见 webhook 产物）。
type cardFile struct {
	Name      string        `json:"name"`
	DSL       feishuCard    `json:"dsl"`
	Variables []cardVar     `json:"variables"`
}
type cardVar struct {
	Type     string `json:"type"`
	APIName  string `json:"apiName"`
	Name     string `json:"name"`
	Desc     string `json:"desc"`
	MockData string `json:"mockData"`
}

func writeCardFile(name string, card feishuCard) {
	b, _ := json.Marshal(cardFile{
		Name: "开往巡查",
		DSL:  card,
		Variables: []cardVar{{Type: "text", APIName: "var_now", Name: "now", Desc: "报告时间", MockData: "2026/11/02 23:03"}},
	})
	// 压成单行，与用户示例文件的紧凑风格一致
	var compact any
	_ = json.Unmarshal(b, &compact)
	c, _ := json.Marshal(compact)
	os.WriteFile(name, c, 0o644)
}

func writeWebhookPayload(name string, card feishuCard) {
	b, _ := json.MarshalIndent(map[string]any{"msg_type": "interactive", "card": card}, "", "  ")
	os.WriteFile(name, b, 0o644)
}

func TestZZWritePreview(t *testing.T) {
	vs := []pipeline.Verdict{}
	add := func(s check.Verdict, n int, tag string, ev string, infra, nav bool) {
		for i := 0; i < n; i++ {
			id := len(vs) + 1
			v := pipeline.Verdict{SiteID: id, Status: s,
				Name: tag + "-" + itoa(id), URL: "https://" + tag + itoa(id) + ".example.com/",
				Evidence: ev}
			if infra { v.InfraErr = ev }
			if nav { v.NavTimeout = ev }
			vs = append(vs, v)
		}
	}
	add(check.Run, 1000, "ok", "https://www.travellings.cn/go.html", false, false)
	add(check.Timeout, 325, "tmo", "timeout: i/o timeout", false, false)
	add(check.Error, 114, "err", "x509: certificate has expired", false, false)
	add(check.Run, 67, "ig", "nav timeout: exceeded 30000ms deadline", false, true)
	add(check.Verdict("403"), 233, "cf", "HTTP 403", false, false)
	add(check.Lost, 61, "lost", "no entry after render", false, false)
	add(check.Run, 9, "infra", "infra js-leg: attach cdp", true, false)
	card := buildFeishuCard(vs, 1*time.Hour+23*time.Minute+46*time.Second)
	writeCardFile("card_preview_prod.card", card)
	writeWebhookPayload("card_preview_prod.json", card)

	small := []pipeline.Verdict{
		{SiteID: 7, Status: check.Run, Name: "khbit", URL: "https://khbit.cn/", Evidence: "https://www.travellings.cn/go.html"},
		{SiteID: 12, Status: check.Run, Name: "wayjam", URL: "https://wayjam.me/", Evidence: "https://www.travellings.cn/go.html"},
		{SiteID: 88, Status: check.Lost, Name: "example", URL: "https://example.com/", Evidence: "no entry after render"},
		{SiteID: 101, Status: check.Error, Name: "oalo", URL: "https://blog.oalo.cn/", Evidence: "x509: certificate has expired"},
		{SiteID: 205, Status: check.Verdict("526"), Name: "cfbadssl", URL: "https://cf.example/", Evidence: "HTTP 526"},
		{SiteID: 300, Status: check.Timeout, Name: "slowSPA", URL: "https://slow.example/", NavTimeout: "nav timeout", Evidence: "nav timeout: exceeded 30000ms deadline"},
	}
	smallCard := buildFeishuCard(small, 18*time.Minute+32*time.Second)
	writeCardFile("card_preview_small.card", smallCard)
	writeWebhookPayload("card_preview_small.json", smallCard)
}

func itoa(n int) string {
	return fmt.Sprint(n)
}


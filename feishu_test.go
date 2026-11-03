package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"travellings-bot-go/check"
	"travellings-bot-go/pipeline"
)

// 未配置 FEISHU_WEBHOOK 必须一个请求都不发——跳过不是失败，巡查照常。
func TestFeishuSkipsWithoutWebhook(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	t.Setenv("FEISHU_WEBHOOK", "")
	if err := notifyFeishu(context.Background(), buildFeishuCard(nil, time.Second, false)); err != nil {
		t.Fatalf("未配置应静默跳过，得到错误: %v", err)
	}
	if called {
		t.Fatal("未配置 webhook 不应发请求")
	}
}

// 载荷 msg_type=interactive；文本含「巡查」（关键词鉴权）；
// 明细表绝不允许出现可信 RUN 站点；HTTP 码在图里合并成一个桶。
func TestFeishuCardCarriesPatrolKeyword(t *testing.T) {
	var got struct {
		MsgType string          `json:"msg_type"`
		Card    json.RawMessage `json:"card"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"code":0,"StatusCode":0,"msg":"success"}`)
	}))
	defer srv.Close()
	t.Setenv("FEISHU_WEBHOOK", srv.URL)

	vs := []pipeline.Verdict{
		{SiteID: 1, Status: check.Run, Name: "khbit", URL: "https://khbit.cn"},
		{SiteID: 3, Status: check.Lost, Name: "example", URL: "https://example.com"},
		{SiteID: 4, Status: check.Verdict("403"), Name: "spa", URL: "https://lakeus.example"},
		{SiteID: 5, Status: check.Timeout, NavTimeout: "nav timeout", Name: "slow", URL: "https://slow.example"},
		{SiteID: 9, Status: check.Error, InfraErr: "attach cdp: connection refused", Name: "brk", URL: "https://brk.example"},
	}
	if err := notifyFeishu(context.Background(), buildFeishuCard(vs, 37*time.Minute, true)); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if got.MsgType != "interactive" {
		t.Fatalf("msg_type 应为 interactive，得到 %q", got.MsgType)
	}
	card := string(got.Card)
	if !strings.Contains(card, "巡查") {
		t.Fatalf("文本必须含「巡查」二字（关键词鉴权），得到: %s", card)
	}
	if !strings.Contains(card, "\"RUN\"") {
		t.Fatalf("图内应有 RUN 桶，得到: %s", card)
	}
	if !strings.Contains(card, "\"HTTP\"") {
		t.Fatalf("裸 HTTP 码应在图里合并为 HTTP 桶，得到: %s", card)
	}
	if !strings.Contains(card, "RUN 占比 **20.0%**") {
		t.Fatalf("5 站 1 RUN 应得 20.0%%，得到: %s", card)
	}
	// 明细表只进 InfraErr 站：khbit/wayjam/example/spa/slow（站点侧）都不进
	for _, n := range []string{"khbit", "example.com", "spa", "slow"} {
		if strings.Contains(card, "\"name\":\""+n+"\"") {
			t.Fatalf("非工具链错误站点 %s 不应进明细表，得到: %s", n, card)
		}
	}
	if !strings.Contains(card, "brk") || !strings.Contains(card, "attach cdp") {
		t.Fatalf("未归类工具链错误 brk 应进明细表，得到: %s", card)
	}
}

// 标题色 = 是否实际写库：green 只在 written=true；其余路径一律 red。
func TestFeishuTemplateReflectsWrite(t *testing.T) {
	vs := []pipeline.Verdict{{SiteID: 1, Status: check.Run, Name: "a", URL: "https://a"}}
	for _, c := range []struct {
		written bool
		want    string
	}{{true, "green"}, {false, "red"}, {false, "red"}} {
		b, _ := json.Marshal(buildFeishuCard(vs, time.Minute, c.written))
		if !strings.Contains(string(b), "\"template\":\""+c.want+"\"") {
			t.Fatalf("written=%v 应 template=%s，得到: %s", c.written, c.want, b)
		}
	}
	// 高占比但没写库：必须还是 red（质量指标不能抢闭环状态的位）
	high := []pipeline.Verdict{{SiteID: 1, Status: check.Run, Name: "a", URL: "https://a"}}
	b, _ := json.Marshal(buildFeishuCard(high, time.Minute, false))
	if !strings.Contains(string(b), "red") {
		t.Fatalf("未写库的满分轮也应 red: %s", b)
	}
}

// schema V2 不支持 note 元素（线上实测 code=11246 拒绝）：溢出截断路径
// 也必须只用 markdown/hr/table/chart。
func TestFeishuNoNoteElement(t *testing.T) {
	vs := make([]pipeline.Verdict, 0, feishuMaxRows+5)
	for i := 0; i < feishuMaxRows+5; i++ {
		vs = append(vs, pipeline.Verdict{SiteID: i + 1, Status: check.Error, InfraErr: "attach cdp", Name: "x", URL: "https://x"})
	}
	b, _ := json.Marshal(buildFeishuCard(vs, time.Minute, false))
	if strings.Contains(string(b), "\"note\"") {
		t.Fatalf("载荷仍含 note 元素（V2 会 11246 拒绝）: %s", b)
	}
	if !strings.Contains(string(b), "仅展示前") {
		t.Fatalf("溢出提示应在 markdown 里保留，得到: %s", b)
	}
}

// HTTP 200 但 body 非零 code（关键词不匹配这类业务失败）必须算发送失败。
func TestFeishuNonZeroCodeIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":19021,"StatusCode":19021,"msg":"key word not match"}`)
	}))
	defer srv.Close()
	t.Setenv("FEISHU_WEBHOOK", srv.URL)
	err := notifyFeishu(context.Background(), buildFeishuCard(nil, time.Second, false))
	if err == nil {
		t.Fatal("非零 code 必须返回错误")
	}
	t.Logf("按预期拒绝: %v", err)
}

// 没有未归类工具链错误 → 表格与引题、分隔线整段消失（哪怕站点侧问题成堆）。
func TestFeishuTableOnlyWhenInfraUnlisted(t *testing.T) {
	vs := []pipeline.Verdict{
		{SiteID: 1, Status: check.Run, Name: "a", URL: "https://a"},
		{SiteID: 2, Status: check.Lost, Name: "b", URL: "https://b"},
		{SiteID: 3, Status: check.Timeout, NavTimeout: "nav timeout", Name: "c", URL: "https://c"},
		{SiteID: 4, Status: check.Verdict("503"), Name: "d", URL: "https://d"},
	}
	b, _ := json.Marshal(buildFeishuCard(vs, time.Minute, false))
	s := string(b)
	if strings.Contains(s, "\"table\"") || strings.Contains(s, "未被自动归类识别") {
		t.Fatalf("无 InfraErr 时不应出表格，得到: %s", s)
	}
}

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
		{SiteID: 2, Status: check.Run, Name: "wayjam", URL: "https://wayjam.me"},
		{SiteID: 3, Status: check.Lost, Name: "example", URL: "https://example.com"},
		{SiteID: 4, Status: check.Verdict("403"), Name: "spa", URL: "https://lakeus.example"},
		{SiteID: 5, Status: check.Timeout, NavTimeout: "nav timeout", Name: "slow", URL: "https://slow.example"}, // IGNORE
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
	if strings.Contains(card, "40.0%") == false {
		t.Fatalf("应展示 RUN 占比 40.0%%，得到: %s", card)
	}
	// 表格里不得出现 khbit / wayjam（可信 RUN 站被误判 IGNORE 的 5 号除外——它进表）
	if strings.Contains(card, "khbit") || strings.Contains(card, "wayjam") {
		t.Fatalf("RUN 站点不应进明细表，得到: %s", card)
	}
	if !strings.Contains(card, "example") || !strings.Contains(card, "slow") {
		t.Fatalf("LOST/IGNORE 站点应进明细表，得到: %s", card)
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
		if !strings.Contains(string(b), "\"template\":\"" + c.want + "\"") {
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

// 全 RUN 的轮次：表格缺席但卡片仍要有 note（不炸空数组）
func TestFeishuAllRunNoTable(t *testing.T) {
	vs := []pipeline.Verdict{{SiteID: 1, Status: check.Run, Name: "a", URL: "https://a"}}
	b, _ := json.Marshal(buildFeishuCard(vs, time.Minute, false))
	if !strings.Contains(string(b), "本轮无问题站点") {
		t.Fatalf("全 RUN 轮应给出无问题提示: %s", b)
	}
}

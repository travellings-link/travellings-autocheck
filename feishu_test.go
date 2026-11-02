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
	if err := notifyFeishu(context.Background(), buildFeishuCard(nil, time.Second)); err != nil {
		t.Fatalf("未配置应静默跳过，得到错误: %v", err)
	}
	if called {
		t.Fatal("未配置 webhook 不应发请求")
	}
}

// 载荷结构 msg_type=interactive + card；卡片序列化后的文本必须含「巡查」（关键词鉴权）。
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
		{Status: check.Run}, {Status: check.Run},
		{Status: check.Lost},
		{Status: check.Verdict("403")},
		{Status: check.Timeout, NavTimeout: "nav timeout"}, // IGNORE，不混进 TIMEOUT
	}
	if err := notifyFeishu(context.Background(), buildFeishuCard(vs, 37*time.Minute)); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if got.MsgType != "interactive" {
		t.Fatalf("msg_type 应为 interactive，得到 %q", got.MsgType)
	}
	// 「巡查」必须出现在卡片文本里——关键词鉴权查的是整条消息的可检索文本
	if !strings.Contains(string(got.Card), "巡查") {
		t.Fatalf("卡片文本必须含「巡查」二字，得到: %s", got.Card)
	}
	if !strings.Contains(string(got.Card), "column_set") {
		t.Fatalf("应为分栏排版，得到: %s", got.Card)
	}
	if !strings.Contains(string(got.Card), "IGNORE") {
		t.Fatalf("IGNORE 应单独成列，得到: %s", got.Card)
	}
	if !strings.Contains(string(got.Card), "RUN 占比 40.0%") {
		t.Fatalf("次要行应含 40.0%% 占比，得到: %s", got.Card)
	}
	if !strings.Contains(string(got.Card), "403") {
		t.Fatalf("裸 HTTP 码判定应入卡片，得到: %s", got.Card)
	}
	t.Logf("实际卡片:\n%s", got.Card)
}

// HTTP 200 但 body 非零 code（关键词不匹配这类业务失败）必须算发送失败。
func TestFeishuNonZeroCodeIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":19021,"StatusCode":19021,"msg":"key word not match"}`)
	}))
	defer srv.Close()
	t.Setenv("FEISHU_WEBHOOK", srv.URL)
	err := notifyFeishu(context.Background(), buildFeishuCard(nil, time.Second))
	if err == nil {
		t.Fatal("非零 code 必须返回错误")
	}
	t.Logf("按预期拒绝: %v", err)
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"travellings-bot-go/pipeline"
)

// 飞书群机器人通知（自定义机器人 webhook，交互卡片）。
// 两条硬约束：
//   - 环境变量 FEISHU_WEBHOOK 未配置 = 静默跳过，巡查照常（跳过不是失败）
//   - 消息文本必须含「巡查」二字：机器人开了关键词安全设置，缺了会被服务端拒

// feishuCard 是 schema 2.0 交互卡片。
type feishuCard struct {
	Schema string      `json:"schema"`
	Header cardHeader  `json:"header"`
	Body   cardBody    `json:"body"`
}

type cardHeader struct {
	Title    cardText `json:"title"`
	Template string   `json:"template"`
}

type cardBody struct {
	Elements []cardElement `json:"elements"`
}

type cardText struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type cardElement struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

// buildFeishuCard 把判定列表排成卡片。计数口径与终端报告一致：
// IGNORE（工具链故障/站点超时，不写库的那批）不混进正常状态，单独成项。
func buildFeishuCard(vs []pipeline.Verdict, elapsed time.Duration) feishuCard {
	counts := map[string]int{}
	ignore := 0
	for _, v := range vs {
		if isIgnore(v) {
			ignore++
			continue
		}
		counts[string(v.Status)]++
	}
	total := len(vs)

	// 占比分子必须在 tag() 删除计数前抓走：tag("RUN") 会把 counts["RUN"] 删掉，
	// 之后再取就恒为 0（测试实测出 RUN 占比 0.0% 就是这个）。
	runN := counts["RUN"]

	var parts []string
	tag := func(k, icon string) {
		if n := counts[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %s %d", icon, k, n))
			delete(counts, k)
		}
	}
	tag("RUN", "\U0001F7E2")
	tag("LOST", "\U0001F7E1")
	tag("ERROR", "\U0001F534")
	tag("TIMEOUT", "\u23F0")
	// 裸 HTTP 码（403/526…）按数值排序补在后面
	others := make([]string, 0, len(counts))
	for k := range counts {
		others = append(others, k)
	}
	sort.Strings(others)
	for _, k := range others {
		parts = append(parts, fmt.Sprintf("\u26AA %s %d", k, counts[k]))
	}
	if ignore > 0 {
		parts = append(parts, fmt.Sprintf("\U0001F6AB IGNORE %d", ignore))
	}

	runPct := 0.0
	if total > 0 {
		runPct = 100 * float64(runN) / float64(total)
	}
	// 关键词「巡查」在标题与正文各出现一次，双保险。
	md := fmt.Sprintf("**巡查完成 %d 站** \u00b7 用时 %s\n%s\nRUN 占比 **%.1f%%**",
		total, elapsed.Round(time.Second), strings.Join(parts, " \u2002|\u2002 "), runPct)
	return feishuCard{
		Schema: "2.0",
		Header: cardHeader{Title: cardText{Tag: "plain_text", Content: "开往巡查报告"}, Template: "turquoise"},
		Body:   cardBody{Elements: []cardElement{{Tag: "markdown", Content: md}}},
	}
}

// notifyFeishu POST 交互卡片到群机器人 webhook。
// 未配置 FEISHU_WEBHOOK 直接返回 nil（跳过）；失败只报告，不推翻巡查结果。
func notifyFeishu(ctx context.Context, card feishuCard) error {
	hook := os.Getenv("FEISHU_WEBHOOK")
	if hook == "" {
		log.Debug().Msg("未配置 FEISHU_WEBHOOK，跳过飞书通知")
		return nil
	}
	body, err := json.Marshal(map[string]any{"msg_type": "interactive", "card": card})
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, hook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("feishu webhook: http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	// 群机器人的业务失败（如关键词不匹配）是 HTTP 200 + 非零 code，必须看 body。
	var r struct {
		Code       int    `json:"code"`
		StatusCode int    `json:"StatusCode"`
		Msg        string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("feishu 响应解析失败: %s: %w", truncate(string(raw), 200), err)
	}
	if r.Code != 0 || r.StatusCode != 0 {
		return fmt.Errorf("feishu 拒绝: code=%d StatusCode=%d msg=%s", r.Code, r.StatusCode, r.Msg)
	}
	return nil
}

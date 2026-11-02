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

type cardText struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type cardElement struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type cardColumn struct {
	Tag      string        `json:"tag"`
	Width    string        `json:"width"`
	Weight   int           `json:"weight"`
	Elements []cardElement `json:"elements"`
}

type columnSet struct {
	Tag     string       `json:"tag"`
	Columns []cardColumn `json:"columns"`
}

type noteElement struct {
	Tag      string     `json:"tag"`
	Elements []cardText `json:"elements"`
}

type cardHeader struct {
	Title    cardText `json:"title"`
	Template string   `json:"template"`
}

type feishuCard struct {
	Schema string         `json:"schema"`
	Header cardHeader     `json:"header"`
	Body   map[string]any `json:"body"`
}

// buildFeishuCard 排版：主状态分栏（大数字），次要信息收进 note 行。
// 计数口径与终端报告一致：IGNORE（工具链故障/站点超时，不写库那批）单独成列。
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
	runN := counts["RUN"]

	col := func(n int, icon, label string) cardColumn {
		return cardColumn{
			Tag: "column", Width: "weighted", Weight: 1,
			Elements: []cardElement{{Tag: "markdown", Content: fmt.Sprintf("**%d**\n%s %s", n, icon, label)}},
		}
	}

	cols := []cardColumn{col(runN, "\U0001F7E2", "RUN"), col(counts["LOST"], "\U0001F7E1", "LOST")}
	delete(counts, "RUN")
	delete(counts, "LOST")
	cols = append(cols, col(counts["ERROR"]+counts["TIMEOUT"], "\U0001F534", "ERR/TMO"))
	delete(counts, "ERROR")
	delete(counts, "TIMEOUT")
	if ignore > 0 {
		cols = append(cols, col(ignore, "\U0001F6AB", "IGNORE"))
	}

	// 裸 HTTP 码（403/526…）进次要行，不跟主状态抢栏位
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var others []string
	for _, k := range keys {
		others = append(others, fmt.Sprintf("%s\u00d7%d", k, counts[k]))
	}

	// 占比分子在 delete 前已抓走（runN），别再从 counts 里取
	runPct := 0.0
	if total > 0 {
		runPct = 100 * float64(runN) / float64(total)
	}
	note := fmt.Sprintf("巡查 %d 站 \u00b7 用时 %s \u00b7 RUN 占比 %.1f%%", total, elapsed.Round(time.Second), runPct)
	if len(others) > 0 {
		note += " \u00b7 其他 " + strings.Join(others, " \u00b7 ")
	}

	// 标题色与健康度一致，阈值同终端报告的 runPctTag（90/70）
	template := "red"
	if runPct >= 70 {
		template = "orange"
	}
	if runPct >= 90 {
		template = "green"
	}

	return feishuCard{
		Schema: "2.0",
		Header: cardHeader{Title: cardText{Tag: "plain_text", Content: "开往巡查报告"}, Template: template},
		Body: map[string]any{"elements": []any{
			columnSet{Tag: "column_set", Columns: cols},
			noteElement{Tag: "note", Elements: []cardText{{Tag: "plain_text", Content: note}}},
		}},
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

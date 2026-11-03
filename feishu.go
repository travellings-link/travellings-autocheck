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
	"strconv"
	"strings"
	"time"

	"travellings-bot-go/check"
	"travellings-bot-go/pipeline"
)

// 飞书群机器人通知（自定义机器人 webhook，交互卡片）。
// 两条硬约束：
//   - 环境变量 FEISHU_WEBHOOK 未配置 = 静默跳过，巡查照常（跳过不是失败）
//   - 消息文本必须含「巡查」二字：机器人开了关键词安全设置，缺了会被服务端拒
// 排版对齐组里定稿的样式：状态分布柱图 + 汇总行 + 问题站点明细表（RUN 不进表）。

// 明细表最多展示的问题站数：webhook 载荷有体积上限，且刷屏比信息缺失更糟；
// 完整清单永远在终端报告 / --format json 里。
const feishuMaxRows = 20

type cardText struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type cardElement struct {
	Tag     string `json:"tag"`
	Content string `json:"content,omitempty"`
	Margin  string `json:"margin,omitempty"`
}

type chartDatum struct {
	Type  string `json:"type"`
	Value int    `json:"value"`
}

type chartSpec struct {
	Type  string `json:"type"`
	Title struct {
		Text string `json:"text"`
	} `json:"title"`
	Data struct {
		Values []chartDatum `json:"values"`
	} `json:"data"`
	XField      []string `json:"xField"`
	YField      string   `json:"yField"`
	SeriesField string   `json:"seriesField"`
	Legends     struct {
		Visible bool   `json:"visible"`
		Orient  string `json:"orient"`
	} `json:"legends"`
}

type chartElement struct {
	Tag        string    `json:"tag"`
	ChartSpec  chartSpec `json:"chart_spec"`
	Preview    bool      `json:"preview"`
	ColorTheme string    `json:"color_theme"`
	Height     string    `json:"height"`
	Margin     string    `json:"margin"`
}

type tableColumn struct {
	DataType        string         `json:"data_type"`
	Name            string         `json:"name"`
	DisplayName     string         `json:"display_name"`
	HorizontalAlign string         `json:"horizontal_align"`
	VerticalAlign    string         `json:"vertical_align,omitempty"`
	Width           string         `json:"width"`
	Format          map[string]any `json:"format,omitempty"`
}

type tableRow struct {
	ID    int    `json:"ids"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

type tableElement struct {
	Tag         string         `json:"tag"`
	Columns     []tableColumn  `json:"columns"`
	Rows        []tableRow     `json:"rows"`
	RowHeight   string         `json:"row_height"`
	HeaderStyle map[string]any `json:"header_style"`
	PageSize    int            `json:"page_size"`
	Margin      string         `json:"margin"`
}

type cardHeader struct {
	Title    cardText `json:"title"`
	Subtitle cardText `json:"subtitle,omitempty"`
	Template string   `json:"template"`
	Padding  string   `json:"padding"`
}

type feishuCard struct {
	Schema string         `json:"schema"`
	Config map[string]any `json:"config"`
	Header cardHeader     `json:"header"`
	Body   map[string]any `json:"body"`
}

// buildFeishuCard 生成巡查报告卡片：柱图看分布、明细表给原因。
// 计数口径与终端报告一致（IGNORE 不混进正常状态）；RUN 站点不进明细表。
func buildFeishuCard(vs []pipeline.Verdict, elapsed time.Duration, written bool) feishuCard {
	runN, lostN, errN, tmoN, httpN, ignoreN := 0, 0, 0, 0, 0, 0
	for _, v := range vs {
		if isIgnore(v) {
			ignoreN++
			continue
		}
		switch string(v.Status) {
		case string(check.Run):
			runN++
		case string(check.Lost):
			lostN++
		case string(check.Error):
			errN++
		case string(check.Timeout):
			tmoN++
		default:
			httpN++
		}
	}
	total := len(vs)
	runPct := 0.0
	if total > 0 {
		runPct = 100 * float64(runN) / float64(total)
	}

	// 柱图：只画非零桶，全绿的轮不会拖一排空柱子
	var vals []chartDatum
	addVal := func(t string, n int) {
		if n > 0 {
			vals = append(vals, chartDatum{Type: t, Value: n})
		}
	}
	addVal("RUN", runN)
	addVal("LOST", lostN)
	addVal("ERROR", errN)
	addVal("TIMEOUT", tmoN)
	addVal("HTTP", httpN)
	addVal("IGNORE", ignoreN)
	var spec chartSpec
	spec.Type = "bar"
	spec.Title.Text = "状态分布"
	spec.Data.Values = vals
	spec.XField = []string{"type"}
	spec.YField = "value"
	spec.SeriesField = "type"
	spec.Legends.Visible = true
	spec.Legends.Orient = "bottom"
	chart := chartElement{
		Tag: "chart", ChartSpec: spec, Preview: true,
		ColorTheme: "brand", Height: "auto", Margin: "0px 0px 0px 0px",
	}

	// 明细表只列**未被自动归类识别的工具链错误**（InfraErr：CDP 断连、浏览器崩溃、
// target 丢失这类落不进「站点慢/站点坏了」的错误）。站点侧问题（LOST/ERROR/
// HTTP 码/站点超时）不进表——它们的结论已写在库和终端报告里，塞进群只是噪声。
	rows := make([]tableRow, 0, len(vs))
	for _, v := range vs {
		if v.InfraErr == "" {
			continue
		}
		name := v.Name
		if name == "" {
			name = "-"
		}
		rows = append(rows, tableRow{
			ID: v.SiteID, Name: name, URL: v.URL,
			Error: truncate(strings.ReplaceAll(v.InfraErr, "\n", " "), 120),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	elements := []any{chart, cardElement{
		Tag:     "markdown",
		Content: fmt.Sprintf("巡查 **%d** 站 · 用时 %s · RUN 占比 **%s%%**", total, elapsed.Round(time.Second), strconv.FormatFloat(runPct, 'f', 1, 64)),
		Margin:  "0px 0px 0px 0px",
	}}


	// 有未归类的工具链错误才出表格；没有就整段消失（含分隔线）
	if len(rows) > 0 {
		elements = append(elements,
			cardElement{Tag: "hr", Margin: "0px 0px 0px 0px"},
			cardElement{Tag: "markdown", Content: fmt.Sprintf("以下 %d 个站点的失败**未被自动归类识别**，疑似工具链问题（本轮结论未写库）：", len(rows)), Margin: "0px 0px 0px 0px"},
		)
		shown := rows
		if len(shown) > feishuMaxRows {
			shown = shown[:feishuMaxRows]
		}
		elements = append(elements, tableElement{
			Tag: "table",
			Columns: []tableColumn{
				{DataType: "number", Name: "ids", DisplayName: "网站 ID", HorizontalAlign: "right", VerticalAlign: "bottom", Width: "auto", Format: map[string]any{"precision": 0}},
				{DataType: "text", Name: "name", DisplayName: "网站名称", HorizontalAlign: "left", Width: "auto"},
				{DataType: "text", Name: "url", DisplayName: "网站链接", HorizontalAlign: "left", Width: "auto"},
				{DataType: "text", Name: "error", DisplayName: "错误原因", HorizontalAlign: "left", Width: "auto"},
			},
			Rows: shown, RowHeight: "low",
			HeaderStyle: map[string]any{"background_style": "grey", "bold": true, "lines": 1},
			PageSize:    5, Margin: "0px 0px 0px 0px",
		})
		if len(rows) > feishuMaxRows {
			elements = append(elements, cardElement{Tag: "markdown",
				Content: fmt.Sprintf("未归类工具链错误共 %d 条，仅展示前 %d 条；完整清单见运行日志或 --format json", len(rows), feishuMaxRows),
				Margin:  "0px 0px 0px 0px",
			})
		}
	}
	template := "red"
	if written {
		template = "green"
	}

	return feishuCard{
		Schema: "2.0",
		Config: map[string]any{"update_multi": true},
		Header: cardHeader{
			Title:    cardText{Tag: "plain_text", Content: "开往自动化巡查报告"},
			Subtitle: cardText{Tag: "plain_text", Content: time.Now().Format("2006-01-02 15:04 MST")},
			Template: template,
			Padding: "12px 12px 12px 12px",
		},
		Body: map[string]any{"direction": "vertical", "padding": "12px 12px 12px 12px", "elements": elements},
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
	// WithoutCancel：中断轮同样要发通知，那时父 ctx 已取消，直接继承会秒失败。
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
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

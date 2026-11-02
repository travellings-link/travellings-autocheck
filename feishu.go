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

type noteElement struct {
	Tag      string     `json:"tag"`
	Elements []cardText `json:"elements"`
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

type optCell struct {
	Text  string `json:"text"`
	Color string `json:"color"`
}

type tableRow struct {
	ID       int       `json:"ids"`
	Name     string    `json:"name"`
	URL      string    `json:"url"`
	Category []optCell `json:"category"`
	Error    string    `json:"error"`
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

// categoryFor 给问题站点的「错误分类」列定标签与颜色。
func categoryFor(v pipeline.Verdict) optCell {
	if isIgnore(v) {
		return optCell{Text: "IGNORE", Color: "turquoise"}
	}
	switch string(v.Status) {
	case string(check.Lost):
		return optCell{Text: "LOST", Color: "orange"}
	case string(check.Error):
		return optCell{Text: "ERROR", Color: "red"}
	case string(check.Timeout):
		return optCell{Text: "TIMEOUT", Color: "carmine"}
	default:
		return optCell{Text: "HTTP " + string(v.Status), Color: "violet"}
	}
}

// buildFeishuCard 生成巡查报告卡片：柱图看分布、明细表给原因。
// 计数口径与终端报告一致（IGNORE 不混进正常状态）；RUN 站点不进明细表。
func buildFeishuCard(vs []pipeline.Verdict, elapsed time.Duration) feishuCard {
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

	// 明细行：可信 RUN 不进表；其余按 ID 排序
	rows := make([]tableRow, 0, len(vs))
	for _, v := range vs {
		if v.Status == check.Run && !isIgnore(v) {
			continue
		}
		name := v.Name
		if name == "" {
			name = "-"
		}
		rows = append(rows, tableRow{
			ID: v.SiteID, Name: name, URL: v.URL,
			Category: []optCell{categoryFor(v)},
			Error:    truncate(strings.ReplaceAll(v.Evidence, "\n", " "), 120),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	elements := []any{chart, cardElement{
		Tag:     "markdown",
		Content: fmt.Sprintf("巡查 **%d** 站 · 用时 %s · RUN 占比 **%s%%**", total, elapsed.Round(time.Second), strconv.FormatFloat(runPct, 'f', 1, 64)),
		Margin:  "0px 0px 0px 0px",
	}, cardElement{Tag: "hr", Margin: "0px 0px 0px 0px"}}

	if len(rows) == 0 {
		elements = append(elements, noteElement{Tag: "note", Elements: []cardText{{Tag: "plain_text", Content: "本轮无问题站点"}}})
	} else {
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
				{DataType: "options", Name: "category", DisplayName: "错误分类", HorizontalAlign: "left", Width: "auto"},
				{DataType: "text", Name: "error", DisplayName: "错误原因", HorizontalAlign: "left", Width: "auto"},
			},
			Rows: shown, RowHeight: "low",
			HeaderStyle: map[string]any{"background_style": "grey", "bold": true, "lines": 1},
			PageSize:    5, Margin: "0px 0px 0px 0px",
		})
		if len(rows) > feishuMaxRows {
			elements = append(elements, noteElement{Tag: "note", Elements: []cardText{{Tag: "plain_text",
				Content: fmt.Sprintf("问题站点共 %d 个，仅展示前 %d 条；完整清单见运行日志或 --format json", len(rows), feishuMaxRows)}}})
		}
	}

	template := "red"
	if runPct >= 70 {
		template = "orange"
	}
	if runPct >= 90 {
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

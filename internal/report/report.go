// Package report 是一份巡查结果的多出口：终端人读报表、逐站 JSON、写库更新集。
//
// 从 package main 抽出（MS 重构 A-4）。verdicts2updates 原只被 main 用，但主从模式
// 的 master 轮末也要走「更新→闸门→写库」，而 main 不能反向 import master——
// 收敛到本包供二者共用（对齐 MS_TASK §5「写库闸门+写回抽共享函数」）。
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"travellings-bot-go/check"
	"travellings-bot-go/db"
	"travellings-bot-go/internal/ui"
	"travellings-bot-go/pipeline"

	"github.com/charmbracelet/lipgloss"
)

// Print 输出人类可读的巡查报告：状态分布 + 问题站点明细。
// 排版与着色一律交给 bubbles（table 负责表格，lipgloss 样式由 table.Styles 承载），
// 不再手工补空格或自己算 CJK 宽度。
func Print(w io.Writer, vs []pipeline.Verdict, elapsed time.Duration, interrupted bool) {
	r := ui.NewReporter(w)
	stats := countStats(vs)
	total := len(vs)
	if total == 0 {
		r.Text("本次没有巡查任何站点")
		return
	}

	runN := stats["RUN"]
	pct := 100 * float64(runN) / float64(total)

	// 两类"未写库"分开计数，因为它们指向完全不同的排查方向：
	//   infra   → 查 CDP/浏览器（我们的问题）
	//   navTimeout → 站点太慢或 js-settle 太小（站点/配置问题）
	infraN, navTimeoutN, ignoreN := 0, 0, 0
	for _, v := range vs {
		switch {
		case v.InfraErr != "":
			infraN++
		case v.NavTimeout != "":
			navTimeoutN++
		}
		if v.Ignored() {
			ignoreN++
		}
	}

	r.Blank()
	// 中断与正常跑完的标题必须一眼可分：中断时这 N 站不代表全量清单，
	// 误当成「巡查完成」会让人以为剩下的站都还没开始就已经有结论了。
	headText := "巡查完成 " + strconv.Itoa(total) + " 站"
	if interrupted {
		headText = "巡查中断 · 已判定 " + strconv.Itoa(total) + " 站"
	}
	head := ui.HeadingStyle.Render(headText) + " · " +
		runPctTag(pct) + " · " +
		ui.DimStyle.Render("用时 "+elapsed.Round(time.Millisecond).String())
	// IGNORE 用紫色：既不是错误（红会让人以为站点坏了），
	// 也不是正常（淡色会被忽略），而是"本轮结论不可信、不写库"。
	// 括号里给出细分原因，方便一眼判断该查 CDP 还是该调 js-settle。
	if ignoreN > 0 {
		detail := ""
		switch {
		case infraN > 0 && navTimeoutN > 0:
			detail = fmt.Sprintf("，工具链故障 %d / 站点超时 %d", infraN, navTimeoutN)
		case infraN > 0:
			detail = fmt.Sprintf("，工具链故障 %d", infraN)
		case navTimeoutN > 0:
			detail = fmt.Sprintf("，站点超时 %d", navTimeoutN)
		}
		head += " · " + ui.UnwrittenStyle.Render(fmt.Sprintf("IGNORE %d%s", ignoreN, detail))
	}
	r.Text(head)
	r.Blank()

	// —— 状态分布：bubbles/table ——
	type row struct {
		name string
		n    int
	}
	// 状态分布按"最终归类"统计：判了 IGNORE 的站点从原始 status 里扣掉，
	// 避免同一站在 TIMEOUT 和 IGNORE 里各算一次、占比相加超过 100%。
	rows := make([]row, 0, len(stats)+1)
	counts := map[string]int{}
	for _, v := range vs {
		if v.Ignored() {
			continue // IGNORE 单独成行
		}
		counts[string(v.Status)]++
	}
	for k, n := range counts {
		rows = append(rows, row{k, n})
	}
	if ignoreN > 0 {
		rows = append(rows, row{"IGNORE", ignoreN})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].name < rows[j].name
	})

	r.Section("状态分布")
	r.Text(ui.JoinRow(
		ui.Cell("判定", 10, ui.DimStyle),
		ui.Cell("数量", 8, ui.DimStyle),
		ui.Cell("占比", 8, ui.DimStyle),
	))
	for _, x := range rows {
		r.Text(ui.JoinRow(
			ui.Cell(x.name, 10, ui.StatusColor(x.name)),
			ui.Cell(strconv.Itoa(x.n), 8, ui.BoldStyle),
			ui.Cell(fmt.Sprintf("%.1f%%", 100*float64(x.n)/float64(total)), 8, ui.DimStyle),
		))
	}

	// —— 问题站点明细 ——
	bad := make([]pipeline.Verdict, 0, total-runN)
	for _, v := range vs {
		if v.Status != check.Run {
			bad = append(bad, v)
		}
	}
	if len(bad) == 0 {
		r.Blank()
		r.Text(ui.OkStyle.Render("无问题站点"))
		return
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].SiteID < bad[j].SiteID })

	r.Blank()
	r.Text(ui.HeadingStyle.Render(fmt.Sprintf("问题站点（%d）", len(bad))))
	// 「原因」列按终端剩余宽度自适应：错误原文是诊断的关键，
	// 固定 60 列会把 TLS 错误链、CDP 超时详情都切掉，让人看不懂为什么失败。
	const (
		idW, stW, stageW, urlW = 6, 8, 6, 38
	)
	reasonW := ui.TerminalWidth() - (idW + stW + stageW + urlW) - 6
	if reasonW < 30 {
		reasonW = 30 // 窄终端下保底，宁可折行也别丢信息
	}

	r.Text(ui.JoinRow(
		ui.Cell("ID", idW, ui.DimStyle),
		ui.Cell("判定", stW, ui.DimStyle),
		ui.Cell("阶段", stageW, ui.DimStyle),
		ui.Cell("站点", urlW, ui.DimStyle),
		ui.Cell("原因", reasonW, ui.DimStyle),
	))
	for _, v := range bad {
		// 不写库的判定在明细里统一显示 IGNORE，与状态分布对得上；
		// 原始 status（TIMEOUT/ERROR）已含在 evidence 里，不丢信息。
		st := string(v.Status)
		if v.Ignored() {
			st = "IGNORE"
		}
		r.Text(ui.JoinRow(
			ui.Cell(strconv.Itoa(v.SiteID), idW, ui.DimStyle),
			ui.Cell(st, stW, ui.StatusColor(st)),
			ui.Cell(string(v.Stage), stageW, ui.DimStyle),
			ui.Cell(ui.Truncate(v.URL, urlW-1), urlW, lipgloss.NewStyle()),
			ui.Cell(ui.Truncate(v.Evidence, reasonW-1), reasonW, ui.DimStyle),
		))
	}
}

// runPctTag 给 RUN 占比上色：>=90 绿、>=70 黄、其余红（与闸门直觉一致）。
func runPctTag(pct float64) string {
	s := strconv.FormatFloat(pct, 'f', 1, 64) + "% RUN"
	switch {
	case pct >= 90:
		return ui.Colorize(ui.Green, s)
	case pct >= 70:
		return ui.Colorize(ui.Yellow, s)
	default:
		return ui.Colorize(ui.Red, s)
	}
}

func countStats(vs []pipeline.Verdict) map[string]int {
	m := map[string]int{}
	for _, v := range vs {
		m[string(v.Status)]++
	}
	return m
}

func WriteJSON(vs []pipeline.Verdict) error {
	enc := json.NewEncoder(os.Stdout)
	for _, v := range vs {
		if err := enc.Encode(map[string]any{"id": v.SiteID, "url": v.URL, "verdict": string(v.Status), "stage": string(v.Stage), "evidence": v.Evidence}); err != nil {
			return err
		}
	}
	return nil
}

// verdicts2updates 把判定转成待写库更新。
// 关键：InfraErr 非空的条目**跳过**——那是我们自己的工具链故障，
// 写进去等于用自己的失败去改站点的线上状态（既定约定）。
func Updates(vs []pipeline.Verdict) []db.VerdictUpdate {
	out := make([]db.VerdictUpdate, 0, len(vs))
	for _, v := range vs {
		// 工具链故障与站点超时都不写库：
		// 前者是我们的 CDP 出了问题，后者是预算不足以证明站点真的坏了。
		if v.InfraErr != "" || v.NavTimeout != "" {
			continue
		}
		out = append(out, db.VerdictUpdate{SiteID: v.SiteID, Status: string(v.Status), Reason: v.Evidence})
	}
	return out
}

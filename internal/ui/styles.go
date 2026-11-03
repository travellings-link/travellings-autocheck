package ui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

// 样式统一由 bubbles/lipgloss 提供，这里只做语义命名，不自己算排版。
var (
	HeadingStyle   = lipgloss.NewStyle().Bold(true)
	SectionStyle   = lipgloss.NewStyle().Faint(true)
	BoldStyle      = lipgloss.NewStyle().Bold(true)
	DimStyle       = lipgloss.NewStyle().Faint(true)
	OkStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	WarnStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	UnwrittenStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("5")) // 紫：「未写库」，区别于红/黄
	LabelStyle     = lipgloss.NewStyle().Faint(true)
	ErrStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

// StatusColor 按判定语义词返回样式：RUN 绿、LOST 黄、ERROR/TIMEOUT 红、IGNORE 紫。
func StatusColor(s string) lipgloss.Style {
	switch s {
	case "RUN":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	case "LOST":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	case "ERROR", "TIMEOUT":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	case "IGNORE":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // 裸 HTTP 码等
	}
}

// Cell 构造一个定宽单元格。lipgloss 的 Width() 按显示宽度填充，
// 正确跳过 ANSI——彩色内容用它排版不会被算错（table 组件做不到）。
func Cell(s string, width int, style lipgloss.Style) string {
	return style.Width(width).Render(s)
}

// JoinRow 把若干已定宽的单元格拼成一行。
func JoinRow(cells ...string) string { return strings.Join(cells, " ") }

// NewTable 构造一张静态表（不含 ANSI 的纯文本用它，边框与对齐由 bubbles 负责）。
// 注意：bubbles 的 table 用 len() 计算单元格宽度，含 ANSI 的字符串会被
// 当成可见字符而截断（实测出现 "ERR…"），所以带颜色的内容不要喂给它。
func NewTable(cols []table.Column, rows []table.Row) table.Model {
	sum := 0
	for _, c := range cols {
		sum += c.Width
	}
	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithWidth(sum+len(cols)*3),
		table.WithHeight(len(rows)+1),
		table.WithFocused(false),
	)
	styles := table.DefaultStyles()
	// Header 与 Cell 必须同样的内边距，否则表头和数据列会错开一格。
	styles.Header = styles.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("8")).
		BorderBottom(true).
		Bold(true).
		Padding(0, 1)
	styles.Cell = styles.Cell.Padding(0, 1)
	styles.Selected = lipgloss.NewStyle()
	t.SetStyles(styles)
	return t
}

// Reporter 是报表的唯一输出出口（渲染好的字符串写出去，其余代码不直接调 fmt）。
type Reporter struct{ w io.Writer }

func NewReporter(w io.Writer) *Reporter { return &Reporter{w: w} }

// Write 输出一行；空字符串即空行。
func (r *Reporter) Write(s string) { _, _ = fmt.Fprintln(r.w, s) }

// Title 输出报告主标题：加粗，片段用 · 分隔。
func (r *Reporter) Title(parts ...string) { r.Write(HeadingStyle.Render(strings.Join(parts, " · "))) }

// Section 输出小节标题：暗色，作为内容分组。
func (r *Reporter) Section(s string) { r.Write(SectionStyle.Render(s)) }

// Blank 输出一个空行。
func (r *Reporter) Blank() { r.Write("") }

// Text 输出一段已渲染的文本。
func (r *Reporter) Text(s string) { r.Write(s) }

// Block 输出组件渲染出的多行文本（如 table.View()），逐行裁掉行尾空格。
func (r *Reporter) Block(s string) {
	for _, ln := range strings.Split(s, "\n") {
		r.Write(strings.TrimRight(ln, " "))
	}
}

package render

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// This file is the full card's channel layout (design "C"): one grey block per
// channel, a large remaining share on the right, a small sub line under it, and
// every account and window folded into a single panel at the bottom.
//
// Every field used here is taken from the official Feishu Card JSON 2.0 docs:
//   - column.background_style / column.padding / column_set.margin (column-set)
//   - column_set nested inside a column (column-set "嵌套规则")
//   - markdown.text_size / markdown.text_align (rich-text)
//   - header.subtitle (title)
// column has no corner_radius or single-side border field, so blocks are
// square and an abnormal block is marked by a light red background instead.

const (
	// blockBackground is the neutral block colour: #f2f3f5 light / #292929 dark.
	blockBackground = "grey-100"
	// blockBackgroundAlert marks a channel that cannot be used: #FEF0F0 light /
	// #3D1A19 dark. It replaces the prototype's left red edge, which Feishu
	// cannot draw.
	blockBackgroundAlert = "red-50"
	// maxBlockAlertRows bounds the abnormal accounts listed inside a block; the
	// rest are in the detail panel.
	maxBlockAlertRows = 3
)

// blockElements is the full card body: summary, one block per channel, alert
// evidence, the buttons, the folded detail panel and the footer, kept under
// Feishu's per-card component ceiling.
func (r *Renderer) blockElements(msg domain.Message, charts bool) []any {
	if isAlertNotice(msg) {
		return r.alertNoticeElements(msg)
	}
	var head []any
	if s := r.summaryHeading(msg); s != "" {
		// Heading and sentence are separate elements: a closing ** directly
		// before a line break is fragile across clients.
		head = append(head, md("**总体判断**"), md(s))
	}
	if msg.Notice != "" {
		head = append(head, md("⚠️ **"+oneLine(msg.Notice)+"**"))
	}
	if msg.Report.Degraded {
		head = append(head, md("⚠️ **"+r.degradedLine(msg.Report)+"**"))
	}

	var alerts []any
	if len(msg.Alerts) > 0 {
		alerts = append(alerts, hr())
		alerts = append(alerts, r.cardAlerts(msg.Alerts)...)
	}

	views, _ := r.summarizeAll(msg.Report, msg.Detailed)

	var tail []any
	if buttons := r.cardButtons(msg); buttons != nil {
		tail = append(tail, buttons)
	}
	panel := r.detailPanel(views, msg.Detailed)
	footer := r.cardFooter(msg)

	var blocks []any
	if charts {
		if ch, ok := r.cardChart(views); ok {
			blocks = append(blocks, ch)
		}
	}
	budget := maxCardElements - countElements(head) - countElements(alerts) - countElements(tail) - 1 - countElements(blocks)
	truncated := false
	for _, v := range orderForBlocks(views) {
		blk := r.channelCard(v)
		cost := countElements([]any{blk})
		if cost > budget {
			truncated = true
			break
		}
		blocks = append(blocks, blk)
		budget -= cost
	}
	if truncated {
		blocks = append(blocks, md("（部分渠道因组件数量上限未展开，可用 @我 <渠道名> 查看）"))
	}
	if panel != nil {
		if cost := countElements([]any{panel}); cost <= budget {
			tail = append(tail, panel)
		}
	}
	tail = append(tail, footer)

	out := make([]any, 0, len(head)+len(blocks)+len(alerts)+len(tail))
	out = append(out, head...)
	out = append(out, blocks...)
	out = append(out, alerts...)
	out = append(out, tail...)
	return out
}

// headerSubtitle is "scope · HH:MM · date". Feishu shows a subtitle-only header
// as a title, so it is only ever set alongside the title.
func (r *Renderer) headerSubtitle(msg domain.Message) string {
	if msg.Report == nil || msg.Report.GeneratedAt.IsZero() {
		return ""
	}
	scope := "全渠道"
	switch {
	case isAlertNotice(msg) && len(msg.Alerts) == 1:
		a := msg.Alerts[0]
		scope = r.displayName(a.Credential.Provider) + " " + shortCredentialName(a.Credential)
	case isAlertNotice(msg):
		scope = strconv.Itoa(len(msg.Alerts)) + " 条变化"
	case msg.Detailed && len(msg.Report.Providers) == 1:
		scope = r.displayName(msg.Report.Providers[0].Provider)
	}
	t := msg.Report.GeneratedAt.In(r.location())
	return scope + " · " + t.Format("15:04") + " · " + t.Format("2006-01-02")
}

// summaryHeading is the conclusion sentence shown under "总体判断".
func (r *Renderer) summaryHeading(msg domain.Message) string {
	return strings.TrimPrefix(r.kicker(msg), "**结论：** ")
}

// orderForBlocks puts unusable channels first, then the rest by remaining share,
// largest first, so the first healthy block is the one to use.
func orderForBlocks(views []providerView) []providerView {
	out := append([]providerView(nil), views...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := blockRank(out[i].worstState), blockRank(out[j].worstState)
		if ri != rj {
			return ri < rj
		}
		return usedOrMax(out[i]) < usedOrMax(out[j])
	})
	return out
}

func blockRank(s domain.CredentialState) int {
	switch s {
	case domain.StateInvalid, domain.StateExhausted:
		return 0
	case domain.StateHealthy, domain.StateLimited, domain.StateNotice, domain.StateWarning:
		return 1
	default:
		// Stale, suspect, unknown: no trustworthy number to rank by.
		return 2
	}
}

func usedOrMax(v providerView) float64 {
	if v.worst == nil {
		return 101
	}
	return *v.worst
}

// blockUnusable reports a channel whose block gets the alert background.
func blockUnusable(s domain.CredentialState) bool {
	return s == domain.StateInvalid || s == domain.StateExhausted
}

// pctFontColor is the colour of the large remaining share. It follows the
// channel's state, never the brand.
func pctFontColor(s domain.CredentialState) string {
	switch s {
	case domain.StateHealthy, domain.StateLimited:
		return "green"
	case domain.StateNotice, domain.StateWarning, domain.StateSuspect:
		return "orange"
	case domain.StateInvalid, domain.StateExhausted:
		return "red"
	default:
		return "grey"
	}
}

// channelCard is one channel's block.
func (r *Renderer) channelCard(v providerView) map[string]any {
	bg := blockBackground
	if blockUnusable(v.worstState) {
		bg = blockBackgroundAlert
	}

	name := fmt.Sprintf("**<font color='%s'>%s</font>**", brandTagColor(v.provider), v.name)
	if v.worstState != "" {
		name += " " + inlineTag(stateTagColor(v.worstState), shortStateLabel(v.worstState))
	}

	pct := "—"
	if v.worst != nil {
		pct = remainingPct(*v.worst)
	}
	big := map[string]any{
		"tag":        "markdown",
		"content":    fmt.Sprintf("**<font color='%s'>%s</font>**", pctFontColor(v.worstState), pct),
		"text_align": "right",
		"text_size":  "heading-3",
	}

	top := map[string]any{
		"tag":       "column_set",
		"flex_mode": "none",
		"columns": []any{
			map[string]any{
				"tag": "column", "width": "weighted", "weight": 1, "vertical_align": "center",
				"elements": []any{md(name)},
			},
			map[string]any{
				"tag": "column", "width": "auto", "vertical_align": "center",
				"elements": []any{big},
			},
		},
	}

	inner := []any{top}
	if lines := r.blockSubLines(v); len(lines) > 0 {
		inner = append(inner, map[string]any{
			"tag":       "markdown",
			"content":   strings.Join(lines, "\n"),
			"text_size": "notation",
		})
	}

	return map[string]any{
		"tag":       "column_set",
		"flex_mode": "none",
		"margin":    "0px 0px 8px 0px",
		"columns": []any{
			map[string]any{
				"tag":              "column",
				"width":            "weighted",
				"weight":           1,
				"background_style": bg,
				"padding":          "10px 12px 10px 12px",
				"elements":         inner,
			},
		},
	}
}

// blockSubLines is the small text under a channel's name: counts, the tightest
// window and when it resets, then the accounts that need action.
func (r *Renderer) blockSubLines(v providerView) []string {
	var head []string
	head = append(head, strconv.Itoa(v.total)+" 个号")
	// The breakdown is only noise when every account is plainly normal; limited
	// (partial coverage) and abnormal counts are evidence and stay visible.
	if v.abnormal > 0 || v.limited > 0 {
		head = append(head, v.breakdown...)
	}
	if w, ok := providerTightest(v); ok {
		head = append(head, "最紧 "+shortWindowName(w))
		if v.recoverAt != nil && blockUnusable(v.worstState) {
			head = append(head, "最早 "+r.formatShort(*v.recoverAt)+" 恢复")
		} else if txt := r.resetText(w); txt != "" {
			head = append(head, "重置 "+txt)
		}
	}
	if v.worstStale {
		head = append(head, "旧值")
	}
	if v.unknownScope {
		// Unknown applicability is evidence; it is never rounded to account scope.
		head = append(head, "含范围未知窗口")
	}
	if v.caution != "" {
		head = append(head, v.caution)
	}
	if v.failure != "" {
		head = append(head, "渠道错误："+oneLine(v.failure))
	}
	lines := []string{grey(strings.Join(head, " · "))}

	shown := 0
	for _, row := range v.rows {
		if shown == maxBlockAlertRows {
			lines = append(lines, grey(fmt.Sprintf("另 %d 个异常号见下方明细", len(v.rows)-shown)))
			break
		}
		lines = append(lines, r.blockAlertRow(v.provider, row))
		shown++
	}
	return lines
}

// blockAlertRow is one account that needs action, in a single line.
func (r *Renderer) blockAlertRow(p domain.ProviderKind, row credRow) string {
	line := "`" + row.label + "` " + shortStateLabel(row.state)
	if row.code != "" {
		line += " · " + row.code
	}
	if row.failure != domain.FailureNone && row.failure != "" {
		line += " · " + domain.FailureReason(row.failure)
	} else if w, ok := tightestWindow(row.windows); ok && w.UsedPercent != nil {
		line += " · " + shortWindowName(w) + " 剩 " + remainingPct(*w.UsedPercent)
		if txt := r.resetText(w); txt != "" {
			line += " · 重置 " + txt
		}
	}
	if row.state == domain.StateInvalid {
		if p == domain.ProviderOllama {
			line += " · 去更新 Cookie"
		} else {
			line += " · 去 CPA 重新登录"
		}
	}
	return line
}

// providerTightest is the window with the highest reported usage across every
// account of the channel.
func providerTightest(v providerView) (domain.QuotaWindow, bool) {
	var best domain.QuotaWindow
	found := false
	for _, row := range v.allRows {
		for _, w := range row.windows {
			if w.UsedPercent == nil {
				continue
			}
			if !found || *w.UsedPercent > *best.UsedPercent {
				best, found = w, true
			}
		}
	}
	return best, found
}

// detailPanel folds every account and window into one panel. It contains only
// markdown: collapsible_panel rejects a nested table in strict JSON 2.0.
func (r *Renderer) detailPanel(views []providerView, detailed bool) map[string]any {
	total := 0
	var inner []any
	for _, v := range views {
		total += v.total
		lines := []string{fmt.Sprintf("**<font color='%s'>%s</font>**", brandTagColor(v.provider), v.name)}
		if len(v.plans) > 0 {
			lines = append(lines, "套餐 "+strings.Join(v.plans, "/"))
		}
		if note := v.coverageNote(); note != "" {
			lines = append(lines, note)
		}
		for _, row := range v.allRows {
			lines = append(lines, r.panelAccountLines(row, detailed)...)
		}
		if v.advice != "" && len(v.rows) > 0 {
			lines = append(lines, "处置建议："+toRemainingCaliber(v.advice))
		}
		if len(lines) > maxPanelRows {
			lines = lines[:maxPanelRows]
		}
		inner = append(inner, md(strings.Join(lines, "\n")))
	}
	if len(inner) == 0 {
		return nil
	}
	return map[string]any{
		"tag":      "collapsible_panel",
		"expanded": detailed,
		"header": map[string]any{
			"title":          map[string]any{"tag": "plain_text", "content": fmt.Sprintf("展开 %d 个账号及窗口明细", total)},
			"vertical_align": "center",
		},
		"elements": inner,
	}
}

// panelAccountLines is one account in the detail panel. A healthy account is a
// single line with every window's remaining share. An account that needs action
// — or any account in a single-channel query (@我 codex) — is expanded: a head
// line with state, error code and reason, then its windows grouped by model,
// each named once in normalized form, never the raw upstream bucket label.
func (r *Renderer) panelAccountLines(row credRow, detailed bool) []string {
	if !detailed && !needsDetail(row.state) {
		return []string{r.formatCompactNormalCredential(row)}
	}
	head := "  · `" + row.label + "`"
	if row.plan != "" {
		head += " " + inlineTag("neutral", row.plan)
	}
	head += "  " + shortStateLabel(row.state)
	if row.code != "" {
		head += " · " + row.code
	}
	if row.failure != domain.FailureNone && row.failure != "" {
		head += " · " + domain.FailureReason(row.failure)
	}
	if len(row.windows) == 0 {
		return []string{head + "  最后成功 " + r.lastSuccessText(row.lastOK)}
	}
	return append([]string{head}, r.groupAndWindowLines(row, true)...)
}

// grey wraps one line in the neutral note colour.
func grey(s string) string {
	return "<font color='grey'>" + s + "</font>"
}

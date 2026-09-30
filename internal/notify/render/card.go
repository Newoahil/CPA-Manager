package render

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// maxCardElements is Feishu's hard per-card component ceiling. We stay a
// little below it: the count includes nested panel elements and table rows, and
// exceeding it makes the whole card fail to send in strict JSON 2.0.
const maxCardElements = 200

// maxPanelRows bounds how many detail rows one collapsible panel contributes,
// so a provider with a hundred credentials cannot blow the component budget.
const maxPanelRows = 50

// Card builds the full Feishu interactive-card (schema 2.0) structure.
//
// Layout: header (title + colour + abnormal-channel text tags), a one-line
// conclusion, then a per-channel block — a status line, a linearProgress chart
// of the remaining share of every credential, and, for abnormal channels only,
// a collapsible detail table — then the footer (a read-only refresh button, an
// optional CPA link, and the data-time/caliber line).
//
// The display caliber is REMAINING (100 - used), matching the CPA console. The
// evaluation thresholds are still used-percentage; only the presentation
// changed.
func (r *Renderer) Card(msg domain.Message) map[string]any {
	return r.card(msg, true)
}

// CardSimple is the degraded card: no chart, no collapsible_panel, no table.
//
// It exists because strict JSON 2.0 rejects a card wholesale when one element
// is malformed, and the chart component's client support is the least certain
// thing in the card. The channel falls back to it automatically, and the
// channel-agnostic evidence survives in markdown.
func (r *Renderer) CardSimple(msg domain.Message) map[string]any {
	return r.card(msg, false)
}

func (r *Renderer) card(msg domain.Message, full bool) map[string]any {
	charts := full && r.charts
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true, "wide_screen_mode": true},
		"header": r.cardHeader(msg),
		"body":   map[string]any{"elements": r.cardElements(msg, full, charts)},
	}
}

// cardHeader is the title bar. The text_tag_list names abnormal channels only,
// at most three (Feishu keeps the first three), and the header colour is the
// worst severity present.
func (r *Renderer) cardHeader(msg domain.Message) map[string]any {
	header := map[string]any{
		"title":    map[string]any{"tag": "plain_text", "content": r.titleFor(msg)},
		"template": r.cardColor(msg),
	}
	var tags []any
	if msg.Report != nil {
		for _, p := range msg.Report.Providers {
			st := providerWorst(p)
			if st == "" || domain.ClassOf(st) != domain.ClassAbnormal {
				continue
			}
			tags = append(tags, map[string]any{
				"tag":   "text_tag",
				"text":  map[string]any{"tag": "plain_text", "content": r.displayName(p.Provider) + " " + shortStateLabel(st)},
				"color": stateTagColor(st),
			})
			if len(tags) == 3 {
				break
			}
		}
	}
	if len(tags) > 0 {
		header["text_tag_list"] = tags
	}
	return header
}

// cardElements assembles the body under the component budget.
func (r *Renderer) cardElements(msg domain.Message, full, charts bool) []any {
	var head []any
	if k := r.kicker(msg); k != "" {
		head = append(head, md(k))
	}
	if msg.Notice != "" {
		head = append(head, md("⚠️ **"+oneLine(msg.Notice)+"**"))
	}
	if msg.Report != nil && msg.Report.Degraded {
		head = append(head, md("⚠️ **"+r.degradedLine(msg.Report)+"**"))
	}
	if len(head) > 0 {
		head = append(head, hr())
	}

	// Alert evidence stays on the card even when a report is present: an alert
	// is often the reason the notification fired, and its facts carry the
	// timestamps and provenance the compact channel lines folded away.
	var alerts []any
	if len(msg.Alerts) > 0 {
		alerts = append(alerts, hr())
		if msg.Report == nil {
			// Alert-only notifications have no channel section, so the title
			// goes here; the header already names the kind.
			alerts = append(alerts, md("**告警明细**"))
		}
		alerts = append(alerts, r.cardAlerts(msg.Alerts)...)
	}

	tail := []any{hr()}
	if buttons := r.cardButtons(msg); buttons != nil {
		tail = append(tail, buttons)
	}
	tail = append(tail, r.cardFooter(msg))

	var blocks []any
	hasChannels := msg.Report != nil && len(msg.Report.Providers) > 0
	if hasChannels {
		views, _ := r.summarizeAll(msg.Report, msg.Detailed)
		budget := maxCardElements - countElements(head) - countElements(alerts) - countElements(tail)
		if budget < 1 {
			budget = 1
		}
		truncated := false
		for _, v := range views {
			block, cost := r.channelBlock(v, msg.Detailed, full, charts)
			if cost > budget {
				// Not enough room for the full block: keep the status line,
				// which is the part a reader scans, and say so.
				block, cost = []any{md(r.cardChannelLine(v))}, 1
				truncated = true
			}
			if cost > budget {
				truncated = true
				break
			}
			blocks = append(blocks, block...)
			budget -= cost
		}
		if truncated {
			blocks = append(blocks, md("（部分渠道因组件数量上限未展开，可用 @我 <渠道名> 查看）"))
		}
	}

	out := make([]any, 0, len(head)+len(blocks)+len(alerts)+len(tail))
	out = append(out, head...)
	out = append(out, blocks...)
	out = append(out, alerts...)
	out = append(out, tail...)
	if !hasChannels && len(msg.Alerts) == 0 && len(head) == 0 {
		out = append([]any{md("暂无可展示的额度信息。")}, out...)
	}
	return out
}

// channelBlock is one channel's elements plus its component cost.
func (r *Renderer) channelBlock(v providerView, detailed, full, charts bool) ([]any, int) {
	out := []any{md(r.cardChannelLine(v))}
	cost := 1
	if charts && len(v.bars) > 0 {
		out = append(out, chartElement(v))
		cost++
	}
	if full && len(v.rows) > 0 {
		// A panel exists exactly when there are credentials that need
		// per-credential evidence: warning, exhausted, invalid, stale, unknown.
		// Healthy and limited channels keep only their overview line.
		panel, rows := r.channelPanel(v, detailed)
		out = append(out, panel)
		cost += 1 + rows
		return out, cost
	}
	if !full {
		// Degraded card: the same evidence as markdown, no panel/table, in the
		// card's remaining caliber.
		for _, line := range r.cardEvidenceLines(v, detailed) {
			out = append(out, md(line))
			cost++
		}
	}
	return out, cost
}

// cardEvidenceLines is the panel's evidence as one markdown line per
// credential, for the degraded card. It never drops a fact the table would
// have shown: the window name, exact remaining share, applicability, reset
// time, upstream limit marker, failure reason and last success all survive.
func (r *Renderer) cardEvidenceLines(v providerView, detailed bool) []string {
	var out []string
	for _, row := range v.rows {
		head := "  · " + row.label + "  " + shortStateLabel(row.state)
		if row.failure != domain.FailureNone && row.failure != "" {
			head += "  依据：" + domain.FailureReason(row.failure)
		}
		if len(row.windows) == 0 {
			out = append(out, head+"  最后成功 "+r.lastSuccessText(row.lastOK))
			continue
		}
		windows := row.windows
		folded := 0
		if !detailed {
			if w, ok := tightestWindow(windows); ok {
				folded = len(windows) - 1
				windows = []domain.QuotaWindow{w}
			}
		}
		first := head + "  " + r.remainingCell(windows[0]) + "  重置 " + r.resetText(windows[0])
		if windows[0].LimitReached && (windows[0].UsedPercent == nil || *windows[0].UsedPercent < 100) {
			first += "  [上游标记已达上限]"
		}
		if row.stale {
			first += " ［旧值 · 最后成功 " + r.lastSuccessText(row.lastOK) + "］"
		}
		if folded > 0 {
			first += "  · 另 " + strconv.Itoa(folded) + " 个窗口"
		}
		out = append(out, first)
		for _, w := range windows[1:] {
			line := "    " + r.remainingCell(w) + "  重置 " + r.resetText(w)
			if w.LimitReached && (w.UsedPercent == nil || *w.UsedPercent < 100) {
				line += "  [上游标记已达上限]"
			}
			out = append(out, line)
		}
	}
	return out
}

// cardChannelLine is one channel's headline on the card: counts, a status
// text_tag, and the tightest remaining share.
func (r *Renderer) cardChannelLine(v providerView) string {
	var parts []string
	parts = append(parts, strconv.Itoa(v.total)+" 个号")
	parts = append(parts, v.breakdown...)
	if v.worstState != "" {
		parts = append(parts, inlineTag(stateTagColor(v.worstState), shortStateLabel(v.worstState)))
	}
	switch {
	case v.worst != nil && v.worstStale:
		parts = append(parts, "最紧剩余 "+remainingPct(*v.worst)+"（旧值）")
	case v.worst != nil:
		parts = append(parts, "最紧剩余 "+remainingPct(*v.worst))
	default:
		parts = append(parts, "用量未上报")
	}
	if v.unknownScope {
		// Unknown applicability is preserved, never rounded to account scope.
		parts = append(parts, "含范围未知窗口")
	}
	if len(v.plans) > 0 {
		parts = append(parts, "套餐 "+strings.Join(v.plans, "/"))
	}
	if v.caution != "" {
		parts = append(parts, v.caution)
	}
	if note := v.coverageNote(); note != "" {
		parts = append(parts, note)
	}
	if v.failure != "" {
		parts = append(parts, "渠道错误："+v.failure)
	}
	line := "**" + v.name + "** · " + strings.Join(parts, " · ")
	if v.advice != "" && (len(v.rows) > 0 || v.failure != "") {
		line += " · 建议：" + v.advice
	}
	return line
}

// chartElement renders the channel's per-credential remaining share as a
// horizontal linearProgress chart, one bar per credential that reported a
// number. value is the 0–1 remaining fraction and text is its percentage, so
// the two can never disagree.
func chartElement(v providerView) map[string]any {
	values := make([]any, 0, len(v.bars))
	for _, b := range v.bars {
		if b.used == nil {
			continue
		}
		rem := remainingFraction(*b.used)
		values = append(values, map[string]any{
			"type":  b.label,
			"value": rem,
			"text":  fmt.Sprintf("%.0f%%", rem*100),
		})
	}
	return map[string]any{
		"tag":          "chart",
		"aspect_ratio": "2:1",
		"chart_spec": map[string]any{
			"type":        "linearProgress",
			"direction":   "horizontal",
			"data":        map[string]any{"values": values},
			"xField":      "value",
			"yField":      "type",
			"seriesField": "type",
			"axes": []any{
				map[string]any{"orient": "left", "domainLine": map[string]any{"visible": false}},
			},
		},
	}
}

// channelPanel is the abnormal-channel detail, folded away unless the reader
// asked for a single channel. It returns the panel and its row count for the
// budget.
func (r *Renderer) channelPanel(v providerView, detailed bool) (map[string]any, int) {
	rows := make([]any, 0, len(v.rows))
	for _, row := range v.rows {
		if detailed && len(row.windows) > 0 {
			for _, w := range row.windows {
				rows = append(rows, r.panelRow(row, w, true))
				if len(rows) >= maxPanelRows {
					break
				}
			}
			continue
		}
		if w, ok := tightestWindow(row.windows); ok {
			rows = append(rows, r.panelRow(row, w, true))
		} else {
			rows = append(rows, r.panelRow(row, domain.QuotaWindow{}, false))
		}
		if len(rows) >= maxPanelRows {
			break
		}
	}

	title := v.name + " 异常明细（" + strconv.Itoa(v.total) + " 个号）"
	if detailed {
		title = v.name + " 全部窗口（" + strconv.Itoa(v.total) + " 个号）"
	}
	panelElements := []any{tableElement(rows)}
	if v.advice != "" {
		panelElements = append(panelElements, md("处置建议："+v.advice))
	}
	panel := map[string]any{
		"tag":      "collapsible_panel",
		"expanded": detailed,
		"header": map[string]any{
			"title":          map[string]any{"tag": "plain_text", "content": title},
			"vertical_align": "center",
		},
		"elements": panelElements,
	}
	return panel, len(rows)
}

// panelRow is one table row: credential, state (with its failure reason),
// remaining share, and reset time (with a stale credential's own last success).
func (r *Renderer) panelRow(row credRow, w domain.QuotaWindow, hasWindow bool) map[string]any {
	state := shortStateLabel(row.state)
	if row.failure != domain.FailureNone && row.failure != "" {
		state += "（" + domain.FailureReason(row.failure) + "）"
	}
	if row.stale {
		state += "（旧值）"
	}
	remaining, reset := "未上报", "未上报"
	if hasWindow {
		remaining = r.remainingCell(w)
		reset = "重置 " + r.resetText(w)
		if w.LimitReached && (w.UsedPercent == nil || *w.UsedPercent < 100) {
			remaining += " [上游标记已达上限]"
		}
		if row.stale {
			reset += " · 最后成功 " + r.lastSuccessText(row.lastOK)
		}
	} else {
		reset = "最后成功 " + r.lastSuccessText(row.lastOK)
	}
	return map[string]any{
		"cred":      row.label,
		"state":     state,
		"remaining": remaining,
		"reset":     reset,
	}
}

func tableElement(rows []any) map[string]any {
	return map[string]any{
		"tag": "table",
		"columns": []any{
			tableColumn("cred", "凭证"),
			tableColumn("state", "状态"),
			tableColumn("remaining", "剩余"),
			tableColumn("reset", "重置时间"),
		},
		"rows": rows,
	}
}

func tableColumn(name, display string) map[string]any {
	return map[string]any{"name": name, "display_name": display, "data_type": "text"}
}

// remainingCell is one window's table cell: readable name, exact remaining
// share, applicability, and an upstream remaining count if any.
func (r *Renderer) remainingCell(w domain.QuotaWindow) string {
	cell := w.DisplayLabel() + " " + remainingPctOrUnknown(w.UsedPercent)
	if tag := scopeTag(w); tag != "" {
		cell += "  [" + tag + "]"
	}
	if w.RemainingAmount != "" {
		cell += "  剩余 " + w.RemainingAmount
	}
	return cell
}

// remainingPctOrUnknown keeps a missing reading visibly missing: nil is never
// printed as 0% or 100%.
func remainingPctOrUnknown(v *float64) string {
	if v == nil {
		return "未上报"
	}
	return remainingPct(*v)
}

// cardButtons is the read-only action row. A callback button always exists; the
// CPA link is added only when a credential is actually broken and the URL is
// configured, so a healthy card never offers a pointless jump.
func (r *Renderer) cardButtons(msg domain.Message) map[string]any {
	columns := []any{
		columnElement(map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "plain_text", "content": "刷新额度"},
			"type": "primary",
			// value must be an object: Feishu rejects a bare string here.
			"behaviors": []any{
				map[string]any{"type": "callback", "value": refreshValue},
			},
		}),
	}
	if u := r.cpaPageURL(); u != "" && hasInvalidCredential(msg) {
		columns = append(columns, columnElement(map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "plain_text", "content": "去 CPA"},
			"type": "default",
			"behaviors": []any{
				map[string]any{"type": "open_url", "default_url": u},
			},
		}))
	}
	return map[string]any{
		"tag":                "column_set",
		"flex_mode":          "none",
		"horizontal_spacing": "default",
		"columns":            columns,
	}
}

func columnElement(el map[string]any) map[string]any {
	return map[string]any{"tag": "column", "width": "auto", "elements": []any{el}}
}

// cardFooter is the single place the data caliber is explained.
func (r *Renderer) cardFooter(msg domain.Message) map[string]any {
	line := "数据时间 " + r.reportTime(msg)
	if msg.Freshness != "" {
		line += " · " + msg.Freshness
	}
	line += " · 口径：剩余 = 100% − 已用%（阈值仍按已用判定）"
	return div(line)
}

// hasInvalidCredential reports whether any credential is confirmed broken, the
// condition for surfacing the CPA link.
func hasInvalidCredential(msg domain.Message) bool {
	for _, a := range msg.Alerts {
		if a.Kind == domain.AlertCredential {
			return true
		}
	}
	if msg.Report == nil {
		return false
	}
	for _, p := range msg.Report.Providers {
		for _, st := range p.States {
			if st == domain.StateInvalid {
				return true
			}
		}
	}
	return false
}

// providerWorst is the most severe state of a provider, from the evaluator's
// States map with WorstState as the fallback. Empty means no judgement was
// made and must not be treated as abnormal.
func providerWorst(p domain.ProviderReport) domain.CredentialState {
	worst := p.WorstState
	for _, st := range p.States {
		if worst == "" || breakdownRank(st) > breakdownRank(worst) {
			worst = st
		}
	}
	return worst
}

// stateTagColor maps a credential state onto a text_tag colour. Colour and
// text always appear together, so meaning never depends on colour alone.
func stateTagColor(s domain.CredentialState) string {
	switch s {
	case domain.StateHealthy:
		return "green"
	case domain.StateNotice, domain.StateWarning:
		return "orange"
	case domain.StateExhausted, domain.StateInvalid:
		return "red"
	case domain.StateLimited:
		return "blue"
	case domain.StateSuspect:
		return "orange"
	default:
		// Stale / unknown / unset: no usable data.
		return "neutral"
	}
}

func inlineTag(color, text string) string {
	return fmt.Sprintf("<text_tag color='%s'>%s</text_tag>", color, text)
}

// remainingOf is the display caliber: remaining = 100 - used, never negative.
func remainingOf(used float64) float64 {
	rem := 100 - used
	if rem < 0 {
		rem = 0
	}
	return rem
}

func remainingPct(used float64) string {
	return fmt.Sprintf("%.1f%%", remainingOf(used))
}

// remainingFraction is the chart's 0–1 value, clamped so a negative or
// over-100 upstream figure cannot produce an invalid bar.
func remainingFraction(used float64) float64 {
	rem := remainingOf(used) / 100
	if rem > 1 {
		return 1
	}
	return rem
}

// countElements counts every component a card contributes, including nested
// panel elements, column elements and table rows, because that is what Feishu
// counts against its ceiling.
func countElements(els []any) int {
	n := 0
	for _, e := range els {
		n++
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if sub, ok := m["elements"].([]any); ok {
			n += countElements(sub)
		}
		if cols, ok := m["columns"].([]any); ok {
			for _, c := range cols {
				if cm, ok := c.(map[string]any); ok {
					if se, ok := cm["elements"].([]any); ok {
						n += countElements(se)
					}
				}
			}
		}
		if rows, ok := m["rows"].([]any); ok {
			n += len(rows)
		}
	}
	return n
}

// kicker is the first-screen conclusion: who to use more, who to ease off, who
// must re-authenticate.
func (r *Renderer) kicker(msg domain.Message) string {
	if msg.Report != nil && len(msg.Report.Providers) > 0 {
		views, _ := r.summarizeAll(msg.Report, msg.Detailed)
		if c := r.conclusion(msg.Report, views); c != "" {
			return "**结论：**" + c
		}
	}
	// Alert-only notifications still lead with the single most actionable line.
	var reauth, ease []string
	for _, a := range msg.Alerts {
		switch a.Kind {
		case domain.AlertCredential:
			reauth = append(reauth, r.displayName(a.Credential.Provider))
		case domain.AlertQuotaExhausted:
			if a.Scope == domain.ScopeAccount {
				ease = append(ease, a.Credential.Label()+"（账号）")
			} else {
				ease = append(ease, a.Credential.Label()+"（仅 "+domain.ScopeText(a.Scope, a.ScopeID)+"）")
			}
		}
	}
	var clauses []string
	if len(ease) > 0 {
		clauses = append(clauses, strings.Join(dedup(ease), "、")+" 的所示范围额度耗尽，请查看各 scope 说明")
	}
	if len(reauth) > 0 {
		clauses = append(clauses, strings.Join(dedup(reauth), "、")+" 需要重新登录")
	}
	if len(clauses) == 0 {
		return ""
	}
	return "**结论：**" + strings.Join(clauses, "；")
}

// reportTime is the generation time of the report the card was built from.
func (r *Renderer) reportTime(msg domain.Message) string {
	if msg.Report == nil {
		return "未知"
	}
	return r.formatTime(msg.Report.GeneratedAt)
}

func (r *Renderer) cardAlerts(alerts []domain.Alert) []any {
	out := []any{}
	var normal []domain.Alert
	for _, a := range alerts {
		if a.Severity == domain.SeverityInfo && a.Kind != domain.AlertQuotaThreshold && a.Scope == "" {
			normal = append(normal, a)
			continue
		}
		out = append(out, md(r.alertBlock(a)))
	}
	for _, a := range normal {
		out = append(out, md(fmt.Sprintf("-%s：%s（%s）", a.Credential.Label(), oneLine(a.Title), evidenceLabel(a.Evidence))))
	}
	return out
}

func (r *Renderer) alertBlock(a domain.Alert) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**[%s·%s] %s**\n", severityLabel(a.Severity), evidenceLabel(a.Evidence), a.Credential.Label())
	if a.Title != "" {
		fmt.Fprintf(&b, "%s\n", oneLine(a.Title))
	}
	if a.Detail != "" {
		fmt.Fprintf(&b, "%s\n", oneLine(a.Detail))
	}
	for _, f := range a.Facts {
		fmt.Fprintf(&b, "- 证据：%s\n", oneLine(f))
	}
	if a.Advice != "" {
		fmt.Fprintf(&b, "处置建议：%s\n", oneLine(a.Advice))
	}
	if u := r.cpaPageURL(); u != "" {
		fmt.Fprintf(&b, "管理页：%s\n", u)
	}
	return strings.TrimRight(b.String(), "\n")
}

// cardColor picks a header colour from the most severe signal present.
func (r *Renderer) cardColor(msg domain.Message) string {
	worst := domain.SeverityInfo
	for _, a := range msg.Alerts {
		if severityRank(a.Severity) > severityRank(worst) {
			worst = a.Severity
		}
	}
	if msg.Report != nil {
		for _, p := range msg.Report.Providers {
			switch providerWorst(p) {
			case domain.StateExhausted, domain.StateInvalid:
				if severityRank(domain.SeverityUrgent) > severityRank(worst) {
					worst = domain.SeverityUrgent
				}
			case domain.StateWarning, domain.StateLimited, domain.StateNotice, domain.StateSuspect:
				if severityRank(domain.SeverityWarn) > severityRank(worst) {
					worst = domain.SeverityWarn
				}
			}
		}
	}
	switch worst {
	case domain.SeverityUrgent:
		return "red"
	case domain.SeverityWarn:
		return "orange"
	default:
		return "blue"
	}
}

func severityRank(s domain.Severity) int {
	switch s {
	case domain.SeverityUrgent:
		return 3
	case domain.SeverityWarn:
		return 2
	default:
		return 1
	}
}

func md(content string) map[string]any {
	return map[string]any{"tag": "markdown", "content": content}
}

func div(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func hr() map[string]any { return map[string]any{"tag": "hr"} }

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

package render

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// This file is the one visual system every card uses: a header, then grey
// blocks (column_set → column with background_style), each with a name and a
// status text_tag on the left, one large number on the right and one short grey
// line under it. Queries, digests, alerts and the fallback card all build from
// the same block, so no card falls back to a wall of plain text.
//
// Every field used here is taken from the official Feishu Card JSON 2.0 docs:
//   - column.background_style / column.padding / column_set.margin (column-set)
//   - column_set nested inside a column (column-set "嵌套规则")
//   - markdown.text_size / markdown.text_align (rich-text)
//   - header.subtitle (title)
// column has no corner_radius or single-side border field, so blocks are
// square and an alarming block is marked by a light red background instead.

const (
	// blockBackground is the neutral block colour: #f2f3f5 light / #292929 dark.
	blockBackground = "grey-50"
	// blockBackgroundAlert marks something that cannot be used: #FEF0F0 light /
	// #3D1A19 dark.
	blockBackgroundAlert = "red-50"
	// maxBlockAlertRows bounds the accounts listed inside a channel block; the
	// rest are in the detail panel.
	maxBlockAlertRows = 3
	// barCells is the width of the text progress bar.
	barCells = 10
)

// blockSpec is one block of the shared system.
type blockSpec struct {
	alarm    bool     // red background
	left     string   // markdown: name + status tag
	big      string   // large right-hand value, may be empty
	bigColor string
	bigSub   string   // small grey text under the big value, may be empty
	sub      []string // short grey lines under the top row
	facts    []string // vertical full-width window facts
	dualSet  any      // side-by-side Option B column_set (flex_mode: flow)
}

// block renders one blockSpec as a single compact row: the left text (with
// its grey detail line folded into the same markdown) and the number on the
// right. One column_set, no nested set: every extra component costs vertical
// space on a phone. Every row has a background: grey-50 (#f5f6f7 light /
// #1A1A1A dark, darker than the card in dark mode, where grey-100 matched the
// card and vanished), and red-50 for a row that cannot be used.
// NoopAction is the callback value of a row container. interactive_container
// requires behaviors; the channel acknowledges it silently.
const NoopAction = "noop"

// blockStyled renders a row. rounded=true wraps it in an interactive_container
// with disabled=true, the only JSON 2.0 container with corner_radius; disabled
// ensures tapping the status row does not invoke callbacks, while keeping
// corner_radius and padding intact. The fallback card uses the plain column_set
// so it never depends on the more recent component.
//
// Every row is exactly two lines — the name line and one grey line — so the
// rows are the same height.
func blockStyled(b blockSpec, rounded bool) map[string]any {
	bg := blockBackground
	if b.alarm {
		bg = blockBackgroundAlert
	}
	if b.dualSet != nil {
		if !rounded {
			// Fallback simple card: plain column_set containing header and dualSet
			return map[string]any{
				"tag":              "column_set",
				"flex_mode":        "none",
				"background_style": bg,
				"margin":           "0px 0px 4px 0px",
				"columns": []any{
					map[string]any{
						"tag":            "column",
						"width":          "weighted",
						"weight":         1,
						"vertical_align": "center",
						"padding":        "6px 8px 6px 8px",
						"elements":       []any{md(b.left), b.dualSet},
					},
				},
			}
		}
		// Regular card: interactive_container with callback behavior
		row := map[string]any{
			"tag":       "column_set",
			"flex_mode": "none",
			"columns": []any{
				map[string]any{
					"tag":            "column",
					"width":          "weighted",
					"weight":         1,
					"vertical_align": "center",
					"elements":       []any{md(b.left)},
				},
			},
		}
		return map[string]any{
			"tag":              "interactive_container",
			"width":            "fill",
			"background_style": bg,
			"corner_radius":    "8px",
			"padding":          "8px 12px 8px 12px",
			"margin":           "0px 0px 8px 0px",
			"disabled":         true,
			"behaviors": []any{
				map[string]any{"type": "callback", "value": map[string]any{"action": NoopAction}},
			},
			"elements": []any{row, b.dualSet},
		}
	}
	if len(b.facts) > 0 {
		factsEl := md(strings.Join(b.facts, "\n"))
		if !rounded {
			// Fallback simple card: plain column_set, no interactive_container
			return map[string]any{
				"tag":              "column_set",
				"flex_mode":        "none",
				"background_style": bg,
				"margin":           "0px 0px 4px 0px",
				"columns": []any{
					map[string]any{
						"tag":            "column",
						"width":          "weighted",
						"weight":         1,
						"vertical_align": "center",
						"padding":        "6px 8px 6px 8px",
						"elements":       []any{md(b.left), factsEl},
					},
				},
			}
		}
		// Regular card: interactive_container with callback behavior
		row := map[string]any{
			"tag":       "column_set",
			"flex_mode": "none",
			"columns": []any{
				map[string]any{
					"tag":            "column",
					"width":          "weighted",
					"weight":         1,
					"vertical_align": "center",
					"elements":       []any{md(b.left)},
				},
			},
		}
		return map[string]any{
			"tag":              "interactive_container",
			"width":            "fill",
			"background_style": bg,
			"corner_radius":    "8px",
			"padding":          "8px 12px 8px 12px",
			"margin":           "0px 0px 8px 0px",
			"disabled":         true,
			"behaviors": []any{
				map[string]any{"type": "callback", "value": map[string]any{"action": NoopAction}},
			},
			"elements": []any{row, factsEl},
		}
	}

	left := b.left
	if len(b.sub) > 0 {
		left += "\n" + b.sub[0]
	}
	cols := []any{
		map[string]any{
			"tag": "column", "width": "weighted", "weight": 1, "vertical_align": "center",
			"elements": []any{md(left)},
		},
	}
	if b.big != "" {
		color := b.bigColor
		if color == "" {
			color = "grey"
		}
		cols = append(cols, map[string]any{
			"tag": "column", "width": "auto", "vertical_align": "center",
			"elements": bigElements(b, color),
		})
	}
	row := map[string]any{
		"tag":       "column_set",
		"flex_mode": "none",
		"columns":   cols,
	}
	if !rounded {
		row["margin"] = "0px 0px 4px 0px"
		row["background_style"] = bg
		for _, c := range cols {
			c.(map[string]any)["padding"] = "6px 8px 6px 8px"
		}
		return row
	}
	return map[string]any{
		"tag":              "interactive_container",
		"width":            "fill",
		"background_style": bg,
		"corner_radius":    "8px",
		"padding":          "8px 12px 8px 12px",
		"margin":           "0px 0px 8px 0px",
		"disabled":         true,
		"behaviors": []any{
			map[string]any{"type": "callback", "value": map[string]any{"action": NoopAction}},
		},
		"elements": []any{row},
	}
}

// bigElements is the right-hand column: the value, and optionally a small
// grey line under it.
func bigElements(b blockSpec, color string) []any {
	out := []any{map[string]any{
		"tag":        "markdown",
		"content":    fmt.Sprintf("**<font color='%s'>%s</font>**", color, b.big),
		"text_align": "right",
		"text_size":  "heading-4",
	}}
	if b.bigSub != "" {
		out = append(out, map[string]any{
			"tag":        "markdown",
			"content":    grey(b.bigSub),
			"text_align": "right",
			"text_size":  "notation",
		})
	}
	return out
}

// brandName is a provider name in its (non-status) brand colour.
func (r *Renderer) brandName(p domain.ProviderKind) string {
	return fmt.Sprintf("**<font color='%s'>%s</font>**", brandTagColor(p), r.displayName(p))
}

// collectFailed reports a query or digest whose live collection failed: the
// numbers on hand are old, so the card leads with the error, not a verdict.
func collectFailed(msg domain.Message) bool {
	return msg.Notice != "" || (msg.Report != nil && msg.Report.Degraded)
}

// blockElements is the query / digest card body. full=false is the fallback
// card: the same blocks, without the collapsible panel.
func (r *Renderer) blockElements(msg domain.Message, full bool) []any {
	views, _ := r.summarizeAll(msg.Report, msg.Detailed)
	if collectFailed(msg) {
		return r.failedElements(msg, views, full)
	}

	var head []any
	if s := r.shortConclusion(views); s != "" {
		head = append(head, md(s))
	}

	var tail []any
	if buttons := r.cardButtons(msg); buttons != nil {
		tail = append(tail, buttons)
	}
	var panel map[string]any
	if full {
		panel = r.detailPanel(views, msg.Detailed, fmt.Sprintf("明细 · %d 个号", totalAccounts(views)))
	}
	footer := r.cardFooter(msg)

	var blocks []any
	if rl := r.rateLimitBlock(msg.Report, full); rl != nil {
		blocks = append(blocks, rl)
	}
	budget := maxCardElements - countElements(head) - countElements(tail) - 1 - countElements(blocks)
	ordered := orderForBlocks(views)
	var channels []any
	var omitted []string
	for i, v := range ordered {
		blk := r.channelCard(v, msg.Detailed, full)
		cost := countElements([]any{blk})
		if cost > budget {
			for _, rem := range ordered[i:] {
				omitted = append(omitted, rem.name)
			}
			break
		}
		channels = append(channels, blk)
		budget -= cost
	}
	if len(omitted) > 0 {
		channels = append(channels, md(grey(fmt.Sprintf("因卡片容量省略 %d 个渠道（%s），可 @我 <渠道名> 查看", len(omitted), strings.Join(omitted, "、")))))
	}
	blocks = append(channels, blocks...)
	if panel != nil {
		if cost := countElements([]any{panel}); cost <= budget {
			tail = append(tail, panel)
		}
	}
	tail = append(tail, footer)

	out := make([]any, 0, len(head)+len(blocks)+len(tail))
	out = append(out, head...)
	out = append(out, blocks...)
	out = append(out, tail...)
	return out
}

// failedElements is the card for a collection that did not produce live data:
// the error first, and the old numbers only behind a fold, never as a verdict.
func (r *Renderer) failedElements(msg domain.Message, views []providerView, full bool) []any {
	last := "未知"
	if msg.Report != nil && !msg.Report.GeneratedAt.IsZero() {
		last = r.formatShort(msg.Report.GeneratedAt)
	}
	out := []any{blockStyled(blockSpec{
		alarm:    true,
		left:     "**实时采集失败** " + inlineTag("red", "未取到新数据"),
		sub:      []string{grey("可点「刷新额度」重试")},
		big:      last,
		bigColor: "grey",
		bigSub:   "上次成功",
	}, full)}
	if buttons := r.cardButtons(msg); buttons != nil {
		out = append(out, buttons)
	}
	if full && msg.Report != nil {
		if panel := r.detailPanel(views, false, "查看上次数据（"+last+"，可能已过期）"); panel != nil {
			out = append(out, panel)
		}
	}
	return append(out, r.cardFooter(msg))
}

func totalAccounts(views []providerView) int {
	n := 0
	for _, v := range views {
		n += v.total
	}
	return n
}

// headerSubtitle is the one-line context under the title. For a query or
// digest it is "scope · HH:MM · date". For an alert notice it names the single
// changed account and its decisive fact, or the count for a batch; the data
// time is left to the footer, where it belongs on every card.
func (r *Renderer) headerSubtitle(msg domain.Message) string {
	if isAlertNotice(msg) {
		return r.alertSubtitle(msg)
	}
	if msg.Report == nil || msg.Report.GeneratedAt.IsZero() {
		return ""
	}
	scope := "全渠道"
	if msg.Detailed && len(msg.Report.Providers) == 1 {
		scope = r.displayName(msg.Report.Providers[0].Provider)
	}
	t := msg.Report.GeneratedAt.In(r.location())
	return scope + " · " + t.Format("15:04") + " · " + t.Format("2006-01-02")
}

// alertSubtitle is the alert card's subtitle: the most severe alert, named by
// provider and alias, with the fact a reader needs at a glance.
func (r *Renderer) alertSubtitle(msg domain.Message) string {
	alerts := append([]domain.Alert(nil), msg.Alerts...)
	sort.SliceStable(alerts, func(i, j int) bool {
		return severityRank(alerts[i].Severity) > severityRank(alerts[j].Severity)
	})
	if len(alerts) == 0 {
		return ""
	}
	groups := alertGroups(alerts)
	if len(groups) > 1 {
		return strconv.Itoa(len(groups)) + " 条变化"
	}
	a := groups[0].representative
	if a.Credential.Provider == "" && strings.TrimSpace(a.Credential.Alias) == "" && strings.TrimSpace(a.Credential.ShortID) == "" {
		// A channel-wide change has no account to name.
		return ""
	}
	provider := r.displayName(a.Credential.Provider)
	alias := shortCredentialName(a.Credential)
	// shortCredentialName falls back to the raw provider; do not repeat it.
	if alias == string(a.Credential.Provider) {
		alias = ""
	}
	switch a.Kind {
	case domain.AlertCredential:
		return alertSubtitleJoin(provider, alias, "需重新登录")
	case domain.AlertRateLimited:
		if t, err := time.Parse(time.RFC3339, alertFacts(a)[domain.FactPrefixRecovery]); err == nil {
			return alertSubtitleJoin(provider, alias, r.formatShort(t)+" 恢复")
		}
		return alertSubtitleJoin(provider, alias, "冷却中")
	case domain.AlertRateLimitCleared, domain.AlertRecovered:
		return alertSubtitleJoin(provider, alias, "已恢复")
	case domain.AlertQuotaReset:
		return alertSubtitleJoin(provider, alias, "已刷新")
	case domain.AlertStale, domain.AlertSuspect:
		return alertSubtitleJoin(provider, alias, shortStateLabel(stateForAlert(a)))
	}
	// Threshold / exhausted: name the provider and the window that is running
	// out, with its remaining share and refresh (or recovery) time. The alias
	// is in the conclusion and the rows, so it is not repeated here.
	w, ok := alertWindow(a, msg.Report)
	if !ok || w.UsedPercent == nil {
		return provider
	}
	window := shortWindowName(w)
	remain := remainingPct(*w.UsedPercent)
	if a.Kind == domain.AlertQuotaExhausted || *w.UsedPercent >= 100 {
		remain = "0.0%"
	}
	base := provider + " · " + window + " 剩 " + remain
	if reset := r.resetText(w); reset != "未上报" {
		if a.Kind == domain.AlertQuotaExhausted {
			return base + " · " + reset + " 恢复"
		}
		return base + " · " + reset + " 刷新"
	}
	return base
}

// stateForAlert maps an alert kind onto a credential state for its subtitle.
func stateForAlert(a domain.Alert) domain.CredentialState {
	switch a.Kind {
	case domain.AlertStale:
		return domain.StateStale
	case domain.AlertSuspect:
		return domain.StateSuspect
	default:
		return domain.StateUnknown
	}
}

// alertSubtitleJoin joins the non-empty parts of an alert subtitle with " · ".
func alertSubtitleJoin(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " · ")
}

// summaryHeading is the conclusion sentence shown under "总体判断".
func (r *Renderer) summaryHeading(msg domain.Message) string {
	return strings.TrimPrefix(r.kicker(msg), "**结论：** ")
}

// usableState reports a credential that can take traffic right now.
func usableState(s domain.CredentialState) bool {
	switch s {
	case domain.StateHealthy, domain.StateLimited, domain.StateNotice, domain.StateWarning:
		return true
	}
	return false
}

// channelHeadroom is the honest big number of a channel: the tightest window
// among the accounts that can still be used, coloured by their own state. A
// broken sibling never lends its colour to a healthy account's number, and a
// healthy sibling's headroom is never painted red.
//
// This is the GRADING number: grade(), ordering and the conclusion keep using
// it, so the 5h headline below never changes whether a channel is usable.
func channelHeadroom(v providerView) (used *float64, state domain.CredentialState, usable bool) {
	for _, row := range v.allRows {
		if !usableState(row.state) {
			continue
		}
		usable = true
		if state == "" || breakdownRank(row.state) > breakdownRank(state) {
			state = row.state
		}
		if w, ok := tightestWindow(row.windows); ok && w.UsedPercent != nil {
			if used == nil || *w.UsedPercent > *used {
				val := *w.UsedPercent
				used = &val
			}
		}
	}
	return used, state, usable
}

// channelHeadline is the displayed headline window among the usable accounts:
// the one window whose number heads the channel row. The window is chosen by
// headlineWindow (5h when present, unless a non-5h window is binding). The big
// number and the refresh time under it both describe this one window. exception
// is true when the pick was the binding-window case, so the row can say "最紧".
func channelHeadline(v providerView) (used *float64, w domain.QuotaWindow, exception, usable bool) {
	for _, row := range v.allRows {
		if !usableState(row.state) {
			continue
		}
		usable = true
		hw, ex, ok := headlinePick(row.windows)
		if !ok || hw.UsedPercent == nil {
			continue
		}
		if used == nil || *hw.UsedPercent > *used {
			val := *hw.UsedPercent
			used, w, exception = &val, hw, ex
		}
	}
	return used, w, exception, usable
}

// channelUnusable is a channel with no account that can take traffic.
func channelUnusable(v providerView) bool {
	if v.total == 0 {
		return false
	}
	_, _, usable := channelHeadroom(v)
	return !usable && blockUnusable(v.worstState)
}

// orderForBlocks puts unusable channels first, then the rest by remaining
// share, largest first, so the first healthy block is the one to use.
func orderForBlocks(views []providerView) []providerView {
	out := append([]providerView(nil), views...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := blockRank(out[i]), blockRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return usedOrMax(out[i]) < usedOrMax(out[j])
	})
	return out
}

func blockRank(v providerView) int {
	switch {
	case channelUnusable(v):
		return 0
	case v.abnormal > 0 || v.failure != "":
		if _, _, usable := channelHeadroom(v); usable {
			// A channel with a broken account goes up, where it is seen.
			return 1
		}
	case v.worstState == domain.StateStale, v.worstState == domain.StateSuspect, v.worstState == domain.StateUnknown:
		if _, _, usable := channelHeadroom(v); !usable {
			// No trustworthy number to rank by.
			return 3
		}
	}
	return 2
}

func usedOrMax(v providerView) float64 {
	used, _, _ := channelHeadroom(v)
	if used == nil {
		return 101
	}
	return *used
}

// blockUnusable reports a state with nothing left to use.
func blockUnusable(s domain.CredentialState) bool {
	return s == domain.StateInvalid || s == domain.StateExhausted
}

// pctFontColor is the colour of a remaining share. It follows the state,
// never the brand.
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

// alertValueColor colours a remaining share by its own value: >=100% used is
// red, >=90% orange, else green. It is used where the headline picks a window
// independently of the channel state, so the number's colour and its value can
// never disagree.
func alertValueColor(used float64) string {
	switch {
	case used >= 100:
		return "red"
	case used >= 90:
		return "orange"
	default:
		return "green"
	}
}

// channelState is the state a channel is judged by: the worst state among the
// accounts that can still take traffic. One exhausted account next to one
// with 80% left is a usable channel, not an emergency; the broken account is
// named on the row's second line instead. Only a channel with no usable
// account takes its worst (broken) state.
func channelState(v providerView) domain.CredentialState {
	if _, st, usable := channelHeadroom(v); usable {
		return st
	}
	return v.worstState
}

type channelWindowsInfo struct {
	headline      domain.QuotaWindow
	headlineUsed  *float64
	headlineRow   credRow
	headlineEx    bool
	hasHeadline   bool
	secondary     domain.QuotaWindow
	secondaryUsed *float64
	secondaryRow  credRow
	hasSecondary  bool
	diffAccounts  bool
	usable        bool
}

// channelQuotaWindowsInfo extracts both the primary headline window and
// the secondary window (e.g. 5h vs 7d) across usable credentials without
// conflating different credentials.
func channelQuotaWindowsInfo(v providerView) channelWindowsInfo {
	var info channelWindowsInfo

	// 1. Find headline window across usable accounts
	for _, row := range v.allRows {
		if !usableState(row.state) {
			continue
		}
		info.usable = true
		hw, ex, ok := headlinePick(row.windows)
		if !ok {
			continue
		}
		if hw.UsedPercent != nil {
			if info.headlineUsed == nil || *hw.UsedPercent > *info.headlineUsed {
				val := *hw.UsedPercent
				info.headlineUsed = &val
				info.headline = hw
				info.headlineEx = ex
				info.headlineRow = row
				info.hasHeadline = true
			}
		} else if !info.hasHeadline {
			info.headline = hw
			info.headlineEx = ex
			info.headlineRow = row
			info.hasHeadline = true
		}
	}

	if !info.hasHeadline {
		return info
	}

	// 2. Identify what the secondary window should be
	headlineIs5h := is5hWindow(info.headline)
	targetCheck := is7dWindow
	if !headlineIs5h {
		targetCheck = is5hWindow
	}

	// 3. Search usable accounts for the secondary window
	for _, row := range v.allRows {
		if !usableState(row.state) {
			continue
		}
		for _, w := range row.windows {
			if targetCheck(w) {
				if w.UsedPercent != nil {
					if info.secondaryUsed == nil || *w.UsedPercent > *info.secondaryUsed {
						val := *w.UsedPercent
						info.secondaryUsed = &val
						info.secondary = w
						info.secondaryRow = row
						info.hasSecondary = true
					}
				} else if !info.hasSecondary {
					info.secondary = w
					info.secondaryRow = row
					info.hasSecondary = true
				}
			}
		}
	}

	if info.hasSecondary && v.total > 1 && info.secondaryRow.label != info.headlineRow.label {
		info.diffAccounts = true
	}

	return info
}

// isLongAlias returns true if an alias/identifier is long enough that displaying
// it in a compact two-column layout risks horizontal truncation on mobile.
func isLongAlias(s string) bool {
	return len([]rune(strings.TrimSpace(s))) > 6
}

// credentialAliasOrShort returns the trimmed credential alias without provider prefix,
// or the short ID if alias is absent. It returns empty string if neither is set.
func credentialAliasOrShort(alias, shortID string, provider domain.ProviderKind) string {
	if strings.TrimSpace(alias) != "" {
		return domain.TrimProviderPrefix(strings.TrimSpace(alias), provider)
	}
	if strings.TrimSpace(shortID) != "" {
		return strings.TrimSpace(shortID)
	}
	return ""
}

// formatFactLine formats one quota window fact as a full-width line.
func (r *Renderer) formatFactLine(w domain.QuotaWindow, used *float64, row credRow, prov domain.ProviderKind, showAcct bool, ex bool) string {
	winName := shortWindowName(w)
	acct := credentialAliasOrShort(row.alias, row.shortID, prov)
	prefix := winName
	if showAcct && acct != "" && acct != string(prov) {
		prefix += " · " + acct
	}
	var remText string
	if used != nil {
		rem := remainingPct(*used)
		color := alertValueColor(*used)
		remText = fmt.Sprintf("<font color='%s'>%s</font>", color, rem)
	}
	var resetText string
	if w.ResetAt != nil {
		resetText = r.formatShort(*w.ResetAt) + " 刷新"
	}
	var line string
	if used != nil {
		line = grey(prefix+" · 剩 ") + remText
		if resetText != "" {
			line += grey(" · " + resetText)
		}
	} else {
		line = grey(prefix + " · 未上报")
		if resetText != "" {
			line += grey(" · " + resetText)
		}
	}
	if ex {
		line += " " + grey("(最紧)")
	}
	return line
}

// channelCard is one channel's row.
func (r *Renderer) channelCard(v providerView, detailed, full bool) map[string]any {
	st := channelState(v)
	left := r.brandName(v.provider)
	if st != "" {
		left += " " + inlineTag(stateTagColor(st), shortStateLabel(st))
	}
	spec := blockSpec{alarm: channelUnusable(v), left: left, big: "—", bigColor: "grey"}
	info := channelQuotaWindowsInfo(v)

	if info.usable && info.hasHeadline {
		hwLabel := credentialAliasOrShort(info.headlineRow.alias, info.headlineRow.shortID, v.provider)
		secLabel := credentialAliasOrShort(info.secondaryRow.alias, info.secondaryRow.shortID, v.provider)
		useVerticalFacts := info.hasSecondary && (info.diffAccounts || v.total > 1 || isLongAlias(hwLabel) || isLongAlias(secLabel))

		if useVerticalFacts {
			type winItem struct {
				win  domain.QuotaWindow
				used *float64
				row  credRow
				ex   bool
			}
			itemHead := winItem{win: info.headline, used: info.headlineUsed, row: info.headlineRow, ex: info.headlineEx}
			itemSec := winItem{win: info.secondary, used: info.secondaryUsed, row: info.secondaryRow}
			first, second := itemHead, itemSec
			if is5hWindow(itemSec.win) && !is5hWindow(itemHead.win) {
				first, second = itemSec, itemHead
			}
			showAcct := info.diffAccounts || v.total > 1 || isLongAlias(hwLabel) || isLongAlias(secLabel)
			line1 := r.formatFactLine(first.win, first.used, first.row, v.provider, showAcct, first.ex)
			line2 := r.formatFactLine(second.win, second.used, second.row, v.provider, showAcct, second.ex)
			spec.facts = []string{line1, line2}
			spec.big = ""
			spec.bigColor = ""
			spec.bigSub = ""
			spec.sub = nil
		} else {
			if info.headlineUsed != nil {
				spec.big, spec.bigColor = remainingPct(*info.headlineUsed), alertValueColor(*info.headlineUsed)
			} else {
				spec.big, spec.bigColor = "未上报", "grey"
			}
			hwName := shortWindowName(info.headline)
			if info.diffAccounts {
				hwAcct := shortCredentialName(domain.Credential{Alias: info.headlineRow.alias, ShortID: info.headlineRow.shortID, Provider: v.provider})
				hwName += "(" + hwAcct + ")"
			}
			if info.headline.ResetAt != nil {
				spec.bigSub = hwName + " · " + r.formatShort(*info.headline.ResetAt) + " 刷新"
			} else {
				spec.bigSub = hwName
			}
			if info.headlineEx {
				spec.bigSub += " (最紧)"
			}

			if info.hasSecondary {
				secName := shortWindowName(info.secondary)
				if info.diffAccounts {
					secAcct := shortCredentialName(domain.Credential{Alias: info.secondaryRow.alias, ShortID: info.secondaryRow.shortID, Provider: v.provider})
					secName += "(" + secAcct + ")"
				}
				var line2 string
				if info.secondaryUsed != nil {
					secRem := remainingPct(*info.secondaryUsed)
					secColor := alertValueColor(*info.secondaryUsed)
					remText := fmt.Sprintf("<font color='%s'>%s</font>", secColor, secRem)
					if info.secondary.ResetAt != nil {
						line2 = grey(secName+" 剩 ") + remText + grey(" · "+r.formatShort(*info.secondary.ResetAt)+" 刷新")
					} else {
						line2 = grey(secName+" 剩 ") + remText
					}
				} else {
					if info.secondary.ResetAt != nil {
						line2 = grey(secName + " 未上报 · " + r.formatShort(*info.secondary.ResetAt) + " 刷新")
					} else {
						line2 = grey(secName + " 未上报")
					}
				}
				spec.sub = []string{line2}
			} else if v.total > 1 {
				spec.sub = []string{grey(strconv.Itoa(v.total) + " 个号")}
			}
		}
	} else if spec.alarm {
		spec.bigColor = "red"
		if v.worstState == domain.StateExhausted {
			spec.big = "0.0%"
		}
		if v.recoverAt != nil {
			spec.bigSub = "最早 " + r.formatShort(*v.recoverAt) + " 恢复"
		}
		if v.total > 1 && v.recoverAt != nil {
			spec.sub = []string{grey(strconv.Itoa(v.total) + " 个号 · 最早 " + r.formatShort(*v.recoverAt) + " 恢复")}
		}
	}

	if color, text := r.problemTag(v); text != "" {
		spec.left = r.brandName(v.provider) + " " + inlineTag(color, text)
	}
	_ = detailed
	return blockStyled(spec, full)
}

// problemTag summarises the accounts that need action in one short tag.
func (r *Renderer) problemTag(v providerView) (color, text string) {
	var rows []credRow
	for _, row := range v.allRows {
		if needsDetail(row.state) {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		if v.failure != "" {
			return "red", "渠道错误"
		}
		return "", ""
	}
	sort.SliceStable(rows, func(i, j int) bool { return breakdownRank(rows[i].state) > breakdownRank(rows[j].state) })
	first := rows[0]
	color = stateTagColor(first.state)
	if len(rows) > 1 {
		same := true
		for _, row := range rows[1:] {
			if row.state != first.state {
				same = false
			}
		}
		if !same {
			// Name the severest problem so the tag is never vaguer than it
			// has to be.
			return color, fmt.Sprintf("%d 号异常 · 含%s", len(rows), shortStateLabel(first.state))
		}
	}
	text = fmt.Sprintf("%d 号%s", len(rows), shortStateLabel(first.state))
	if channelUnusable(v) && len(rows) == v.total {
		text = shortStateLabel(first.state)
	}
	switch {
	case first.state == domain.StateExhausted:
		if w, ok := tightestWindow(first.windows); ok && w.ResetAt != nil {
			text += " · " + w.ResetAt.In(r.location()).Format("15:04") + " 恢复"
		}
	case first.code != "" && len(rows) == 1:
		text += " " + first.code
	}
	return color, text
}

// blockProblemLine is the row's second line when some account needs action:
// the first such account, and how many more there are. Everything else is in
// the fold.
func (r *Renderer) blockProblemLine(v providerView) string {
	var first string
	n := 0
	for _, row := range v.allRows {
		if !needsDetail(row.state) {
			continue
		}
		if n == 0 {
			first = r.problemBrief(row)
		}
		n++
	}
	switch {
	case n == 0 && v.failure != "":
		return grey("渠道错误，见明细")
	case n == 0:
		return ""
	case n > 1:
		first += fmt.Sprintf(" 等 %d 个号", n)
	}
	return grey(first)
}

// problemBrief is "`alias` 已用满 · 19:29 恢复" / "`alias` 凭证失效 401". The
// state word is coloured, so a broken account stands out on a usable row.
func (r *Renderer) problemBrief(row credRow) string {
	line := "`" + row.label + "` " + fmt.Sprintf("<font color='%s'>%s</font>", pctFontColor(row.state), shortStateLabel(row.state))
	if row.code != "" {
		line += " " + row.code
	}
	w, ok := headlineWindow(row.windows)
	switch {
	case row.state == domain.StateExhausted && ok && w.ResetAt != nil:
		line += " · " + r.formatShort(*w.ResetAt) + " 恢复"
	case row.failure == domain.FailureNone && ok && w.UsedPercent != nil:
		line += " · " + shortWindowName(w) + " 剩 " + remainingPct(*w.UsedPercent)
	}
	return line
}

// shortConclusion is the one-line verdict on top of the card: who to avoid,
// who to save, who to use. The reasons are in the rows below it.
func (r *Renderer) shortConclusion(views []providerView) string {
	var avoid, tightNames, use []string
	var ample []providerView
	for _, v := range views {
		switch v.grade() {
		case unusable:
			avoid = append(avoid, v.name)
		case tight:
			tightNames = append(tightNames, v.name)
		case 3: // ample
			ample = append(ample, v)
		}
	}
	sort.SliceStable(ample, func(i, j int) bool { return usedOrMax(ample[i]) < usedOrMax(ample[j]) })
	for _, v := range ample {
		use = append(use, v.name)
	}
	var parts []string
	if len(avoid) > 0 {
		parts = append(parts, strings.Join(avoid, "、")+" 先别用")
	}
	if len(tightNames) > 0 {
		parts = append(parts, strings.Join(tightNames, "、")+" 省着用")
	}
	switch {
	case len(use) > 0 && len(parts) > 0:
		parts = append(parts, "改用 "+strings.Join(use, " / "))
	case len(use) > 0:
		parts = append(parts, "优先用 "+strings.Join(use, " > "))
	}
	return strings.TrimSpace(strings.Join(parts, "，"))
}

// blockSubLines is a channel's summary inside the fold: account count, the
// tightest usable window and when it refreshes.
func (r *Renderer) blockSubLines(v providerView, detailed bool) []string {
	head := []string{strconv.Itoa(v.total) + " 个号"}
	if v.recoverAt != nil && channelUnusable(v) {
		head = append(head, "最早 "+r.formatShort(*v.recoverAt)+" 恢复")
	} else if _, w, exception, ok := channelHeadline(v); ok {
		// "最紧" is only true when a longer window really is the binding one;
		// a 5h headline is just the window a reader plans around.
		prefix := ""
		if exception {
			prefix = "最紧 "
		}
		if txt := r.resetText(w); txt != "未上报" {
			// Name the window and its refresh together, so the summary line
			// and the big number beside it describe the same window.
			head = append(head, prefix+shortWindowName(w)+" · "+txt+" 刷新")
		} else {
			head = append(head, prefix+shortWindowName(w))
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
		head = append(head, "渠道错误")
	}
	lines := []string{grey(strings.Join(head, " · "))}
	if detailed {
		// A single-channel query expands every account in the panel below;
		// listing them here too was the duplication.
		return lines
	}
	shown := 0
	for _, row := range v.allRows {
		if !needsDetail(row.state) {
			continue
		}
		if shown == maxBlockAlertRows {
			lines = append(lines, grey("其余异常号见下方明细"))
			break
		}
		lines = append(lines, r.blockAlertRow(row))
		shown++
	}
	return lines
}

// blockAlertRow is one account that needs action, in a single line.
func (r *Renderer) blockAlertRow(row credRow) string {
	line := "`" + row.label + "` " + inlineTag(stateTagColor(row.state), shortStateLabel(row.state))
	if row.code != "" {
		line += " " + row.code
	}
	if row.failure != domain.FailureNone && row.failure != "" {
		line += " · " + domain.FailureReason(row.failure)
	} else if w, ok := headlineWindow(row.windows); ok && w.UsedPercent != nil {
		line += " · " + shortWindowName(w) + " 剩 " + remainingPct(*w.UsedPercent)
		if txt := r.resetText(w); txt != "未上报" {
			line += " · " + txt + " 刷新"
		}
	}
	return line
}

// rateLimitBlock is the digest's tally of short cooldowns that did not last
// long enough to alert. Absent when there were none.
func (r *Renderer) rateLimitBlock(rep *domain.Report, full bool) map[string]any {
	if rep == nil || len(rep.RateLimits) == 0 {
		return nil
	}
	tallies := append([]domain.RateLimitTally(nil), rep.RateLimits...)
	sort.SliceStable(tallies, func(i, j int) bool { return tallies[i].Count > tallies[j].Count })
	var sub []string
	for i, t := range tallies {
		if i == maxBlockAlertRows*2 {
			sub = append(sub, grey(fmt.Sprintf("另 %d 个号", len(tallies)-i)))
			break
		}
		line := fmt.Sprintf("%s `%s` %d 次", r.displayName(t.Credential.Provider), shortCredentialName(t.Credential), t.Count)
		if t.Longest > 0 {
			line += " · 最长 " + humanDuration(t.Longest)
		}
		sub = append(sub, line)
	}
	return blockStyled(blockSpec{
		left: "**短暂限流** " + inlineTag("neutral", "未达告警"),
		sub:  []string{grey(strings.Join(sub, "；"))},
	}, full)
}

// detailPanel folds every account and window into one panel. It contains only
// markdown: collapsible_panel rejects a nested table in strict JSON 2.0.
func (r *Renderer) detailPanel(views []providerView, expanded bool, title string) map[string]any {
	var inner []any
	for _, v := range views {
		lines := []string{r.brandName(v.provider) + " " + strings.Join(r.blockSubLines(v, true), "")}
		for _, row := range v.allRows {
			lines = append(lines, r.panelAccountLines(row)...)
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
		"expanded": expanded,
		"header": map[string]any{
			"title":          map[string]any{"tag": "plain_text", "content": title},
			"vertical_align": "center",
		},
		"elements": inner,
	}
}

// panelAccountLines is one account: its alias (and state when it is not
// plainly fine), then one progress bar per window.
func (r *Renderer) panelAccountLines(row credRow) []string {
	head := "`" + row.label + "`"
	if needsDetail(row.state) {
		head += " " + inlineTag(stateTagColor(row.state), shortStateLabel(row.state))
	}
	if row.code != "" {
		head += " " + row.code
	}
	if row.failure != domain.FailureNone && row.failure != "" {
		head += " · " + domain.FailureReason(row.failure)
	}
	if len(row.windows) == 0 {
		return []string{head + grey(" · 上次成功 "+r.lastSuccessText(row.lastOK))}
	}
	if row.stale {
		head += grey(" · 旧值，上次成功 " + r.lastSuccessText(row.lastOK))
	}
	out := []string{head}
	for _, w := range orderedWindows(row.windows) {
		out = append(out, r.windowBarLine(w, row.stale))
	}
	return out
}

// windowBarLine is "5h ▰▰▰▰▰▰▰▰▱▱ 83.0% · 10-08 16:30 刷新".
func (r *Renderer) windowBarLine(w domain.QuotaWindow, stale bool) string {
	line := shortWindowName(w) + " "
	if w.UsedPercent == nil {
		line += grey(strings.Repeat("▱", barCells)) + " 未上报"
	} else {
		line += progressBar(*w.UsedPercent, stale) + " " + remainingPct(*w.UsedPercent)
	}
	if w.Scope.Normalized() == domain.ScopeUnknown {
		line += "（范围未知）"
	}
	if w.RemainingAmount != "" {
		line += " · 剩余 " + w.RemainingAmount
	}
	if txt := r.resetText(w); txt != "未上报" {
		line += grey(" · " + txt + " 刷新")
	}
	if w.LimitReached && (w.UsedPercent == nil || *w.UsedPercent < 100) {
		line += " · 上游标记已达上限"
	}
	return line
}

// progressBar draws the remaining share as barCells text cells. Feishu has no
// progress-bar component and charts are off, so the bar is markdown: filled
// cells in the state colour, empty cells grey.
func progressBar(used float64, stale bool) string {
	rem := remainingOf(used)
	filled := int(math.Round(rem / 100 * barCells))
	if filled > barCells {
		filled = barCells
	}
	if filled == 0 && rem > 0 {
		filled = 1
	}
	color := "green"
	switch {
	case stale:
		color = "grey"
	case used >= 100:
		color = "red"
	case used >= 90:
		color = "orange"
	}
	out := ""
	if filled > 0 {
		out += fmt.Sprintf("<font color='%s'>%s</font>", color, strings.Repeat("▰", filled))
	}
	if filled < barCells {
		out += grey(strings.Repeat("▱", barCells-filled))
	}
	return out
}

// humanDuration is a duration the way a person says it.
func humanDuration(d interface{ Minutes() float64 }) string {
	m := int(math.Round(d.Minutes()))
	switch {
	case m < 1:
		return "不到 1 分钟"
	case m < 60:
		return strconv.Itoa(m) + " 分钟"
	case m%60 == 0:
		return strconv.Itoa(m/60) + " 小时"
	default:
		return fmt.Sprintf("%d 小时 %d 分钟", m/60, m%60)
	}
}

// grey wraps one line in the neutral note colour.
func grey(s string) string {
	return "<font color='grey'>" + s + "</font>"
}

package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// An alert notice says what changed and nothing else: one line per account
// with the window, its remaining share and when it resets. The full overview of
// every channel belongs to queries and the scheduled digest; repeating it under
// every reset notification buried the one line that mattered.

// maxAlertLines bounds the lines on one notice; the rest are in the panel.
const maxAlertLines = 10

// isAlertNotice reports a notification driven by state changes. The first-run
// bootstrap is excluded: it is the initial inventory and keeps the overview.
func isAlertNotice(msg domain.Message) bool {
	if len(msg.Alerts) == 0 {
		return false
	}
	for _, a := range msg.Alerts {
		if a.Kind == domain.AlertBootstrap {
			return false
		}
	}
	return true
}

func (r *Renderer) alertNoticeElements(msg domain.Message) []any {
	alerts := append([]domain.Alert(nil), msg.Alerts...)
	sort.SliceStable(alerts, func(i, j int) bool {
		return severityRank(alerts[i].Severity) > severityRank(alerts[j].Severity)
	})

	snaps := snapshotIndex(msg.Report)
	var lines []string
	seen := map[string]bool{}
	for _, a := range alerts {
		line := r.alertLine(a, snaps)
		if seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	if len(lines) > maxAlertLines {
		rest := len(lines) - maxAlertLines
		lines = append(lines[:maxAlertLines], grey(fmt.Sprintf("另 %d 条变化见下方明细", rest)))
	}

	var out []any
	if msg.Notice != "" {
		out = append(out, md("⚠️ **"+oneLine(msg.Notice)+"**"))
	}
	if msg.Report != nil && msg.Report.Degraded {
		out = append(out, md("⚠️ **"+r.degradedLine(msg.Report)+"**"))
	}
	out = append(out, md(strings.Join(lines, "\n")))

	// Advice only when something got worse: a reset or recovery needs no
	// "use X instead".
	if needsAdvice(alerts) {
		if c := r.summaryHeading(msg); c != "" {
			out = append(out, md(grey("建议："+c)))
		}
	}

	if buttons := r.cardButtons(msg); buttons != nil {
		out = append(out, buttons)
	}
	if msg.Report != nil && len(msg.Report.Providers) > 0 {
		views, _ := r.summarizeAll(msg.Report, false)
		if panel := r.detailPanel(views, false); panel != nil {
			out = append(out, panel)
		}
	}
	return append(out, r.cardFooter(msg))
}

func needsAdvice(alerts []domain.Alert) bool {
	for _, a := range alerts {
		if a.Severity == domain.SeverityWarn || a.Severity == domain.SeverityUrgent {
			return true
		}
	}
	return false
}

// alertLine is one changed account: brand, alias, what happened, and the facts
// that let a reader act — window, remaining share, next reset or the error.
func (r *Renderer) alertLine(a domain.Alert, snaps map[string]domain.QuotaSnapshot) string {
	line := r.alertLineBody(a, snaps)
	// A scoped change applies to that model/group only, never the account.
	sc := a.Scope.Normalized()
	if a.Scope != "" && sc != domain.ScopeAccount {
		line += " · 仅 " + domain.ScopeText(a.Scope, a.ScopeID)
		if a.Kind == domain.AlertRecovered {
			line += "，不代表全账号恢复"
		}
	}
	return line
}

func (r *Renderer) alertLineBody(a domain.Alert, snaps map[string]domain.QuotaSnapshot) string {
	p := a.Credential.Provider
	color, phrase := alertPhrase(a)
	tag := inlineTag(color, phrase)
	if a.Credential.Key == "" && strings.TrimSpace(a.Credential.Alias) == "" && strings.TrimSpace(a.Credential.ShortID) == "" {
		// Not about one account (e.g. a channel-wide change): no brand or alias
		// to name, so say what changed.
		return tag
	}
	line := fmt.Sprintf("**<font color='%s'>%s</font>** `%s` %s",
		brandTagColor(p), r.displayName(p), shortCredentialName(a.Credential), tag)

	snap, ok := snaps[a.Credential.Key]
	switch a.Kind {
	case domain.AlertCredential:
		if ok && snap.Code != "" {
			line += " · " + snap.Code
		}
		if ok && snap.Failure != domain.FailureNone && snap.Failure != "" {
			line += " · " + domain.FailureReason(snap.Failure)
		}
		if p == domain.ProviderOllama {
			line += " · 去更新 Cookie"
		} else {
			line += " · 去 CPA 重新登录"
		}
		return line
	case domain.AlertStale, domain.AlertSuspect:
		if ok && snap.Code != "" {
			line += " · " + snap.Code
		}
		if ok {
			line += " · 最后成功 " + r.lastSuccessText(snap.LastSuccessAt)
		}
		return line
	}

	if !ok {
		return line
	}
	if w, found := matchAlertWindow(a, snap.Windows); found && w.UsedPercent != nil {
		line += " · " + shortWindowName(w) + " 剩 " + remainingPct(*w.UsedPercent)
		if txt := r.resetText(w); txt != "" {
			if a.Kind == domain.AlertQuotaExhausted {
				line += " · " + txt + " 恢复"
			} else {
				line += " · 下次重置 " + txt
			}
		}
		return line
	}
	// No single window named: show every window, as on a folded account line.
	var parts []string
	for _, w := range snap.Windows {
		if w.UsedPercent == nil {
			continue
		}
		part := shortWindowName(w) + " 剩 " + remainingPct(*w.UsedPercent)
		if w.Scope.Normalized() == domain.ScopeUnknown {
			// Unknown applicability is evidence; never let it read as account-wide.
			part += "（范围未知）"
		}
		parts = append(parts, part)
	}
	if len(parts) > 0 {
		line += " · " + strings.Join(parts, " ｜ ")
	}
	return line
}

// alertPhrase is the status tag of a change. A suspected diagnosis is always
// labelled as suspected: a flat "凭证失效" would overstate the evidence.
func alertPhrase(a domain.Alert) (color, phrase string) {
	color, phrase = alertKindPhrase(a)
	if a.Evidence == domain.EvidenceSuspected && a.Kind != domain.AlertSuspect {
		return "orange", "疑似" + phrase
	}
	return color, phrase
}

func alertKindPhrase(a domain.Alert) (color, phrase string) {
	switch a.Kind {
	case domain.AlertQuotaReset:
		return "green", "额度已重置"
	case domain.AlertRecovered:
		return "green", "已恢复"
	case domain.AlertQuotaThreshold:
		return "orange", "接近上限"
	case domain.AlertQuotaExhausted:
		return "red", "已用满"
	case domain.AlertCredential:
		return "red", "凭证失效"
	case domain.AlertSuspect:
		return "orange", "疑似异常"
	case domain.AlertStale:
		return "neutral", "数据过期"
	default:
		if a.Title != "" {
			return "neutral", a.Title
		}
		return "neutral", "状态变化"
	}
}

// matchAlertWindow finds the window an alert is about. The evaluator names it
// in the first "窗口: " fact; the scope narrows it when several windows share a
// label.
func matchAlertWindow(a domain.Alert, windows []domain.QuotaWindow) (domain.QuotaWindow, bool) {
	label := ""
	for _, f := range a.Facts {
		if strings.HasPrefix(f, "窗口: ") {
			label = strings.TrimSpace(strings.TrimPrefix(f, "窗口: "))
			break
		}
	}
	if label == "" {
		return domain.QuotaWindow{}, false
	}
	for _, w := range windows {
		if w.DisplayLabel() != label {
			continue
		}
		if a.ScopeID != "" && w.ScopeID != a.ScopeID {
			continue
		}
		return w, true
	}
	return domain.QuotaWindow{}, false
}

// snapshotIndex maps credential key to its latest snapshot.
func snapshotIndex(rep *domain.Report) map[string]domain.QuotaSnapshot {
	out := map[string]domain.QuotaSnapshot{}
	if rep == nil {
		return out
	}
	for _, p := range rep.Providers {
		for _, s := range p.Snapshots {
			out[s.Credential.Key] = s
		}
	}
	return out
}

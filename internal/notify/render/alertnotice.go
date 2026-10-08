package render

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// An alert notice says what changed and nothing else: one block per changed
// account, in the same block system as the query card, with the window, its
// remaining share and when it refreshes (or the error code). The overview of
// every channel belongs to queries and the scheduled digest.

// maxAlertLines bounds the blocks on one notice; the rest are in the panel.
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

func (r *Renderer) alertNoticeElements(msg domain.Message, full bool) []any {
	alerts := append([]domain.Alert(nil), msg.Alerts...)
	sort.SliceStable(alerts, func(i, j int) bool {
		return severityRank(alerts[i].Severity) > severityRank(alerts[j].Severity)
	})

	snaps := snapshotIndex(msg.Report)
	var out []any
	// The alert stands, but say when the data behind it is partial.
	if msg.Report != nil && msg.Report.Degraded {
		out = append(out, md(grey(oneLine(r.degradedLine(msg.Report)))))
	}
	seen := map[string]bool{}
	shown := 0
	for _, a := range alerts {
		spec := r.alertBlock(a, snaps, msg.Report)
		key := spec.left + "|" + strings.Join(spec.sub, "|")
		if seen[key] {
			continue
		}
		seen[key] = true
		if shown == maxAlertLines {
			out = append(out, md(grey(fmt.Sprintf("另有 %d 条变化，见下方明细", len(alerts)-shown))))
			break
		}
		out = append(out, blockStyled(spec, full))
		shown++
	}

	// Advice only when something got worse, and only the channels to switch
	// to: the blocks above already say what is wrong.
	if needsAdvice(alerts) {
		if names := r.switchTargets(msg, alerts); len(names) > 0 {
			out = append(out, md(grey("建议改用 "+strings.Join(names, " / "))))
		}
	}

	if buttons := r.alertButtons(msg); buttons != nil {
		out = append(out, buttons)
	}
	if full && msg.Report != nil && len(msg.Report.Providers) > 0 {
		views, _ := r.summarizeAll(msg.Report, false)
		if panel := r.detailPanel(views, false, fmt.Sprintf("明细 · %d 个号", totalAccounts(views))); panel != nil {
			out = append(out, panel)
		}
	}
	return append(out, r.cardFooter(msg))
}

func needsAdvice(alerts []domain.Alert) bool {
	for _, a := range alerts {
		if worsening(a) {
			return true
		}
	}
	return false
}

// worsening is an alert that takes capacity away.
func worsening(a domain.Alert) bool {
	switch a.Kind {
	case domain.AlertQuotaReset, domain.AlertRecovered, domain.AlertRateLimitCleared, domain.AlertStale:
		return false
	}
	return a.Severity == domain.SeverityWarn || a.Severity == domain.SeverityUrgent
}

// switchTargets are channels with real headroom that are not the subject of a
// worsening alert.
func (r *Renderer) switchTargets(msg domain.Message, alerts []domain.Alert) []string {
	if msg.Report == nil {
		return nil
	}
	hit := map[domain.ProviderKind]bool{}
	for _, a := range alerts {
		if worsening(a) {
			hit[a.Credential.Provider] = true
		}
	}
	views, _ := r.summarizeAll(msg.Report, false)
	var names []string
	for _, v := range orderForBlocks(views) {
		if hit[v.provider] || v.grade() != ample {
			continue
		}
		names = append(names, v.name)
	}
	return names
}

// alertBlock is one changed account as a block: brand, alias and what
// happened on the left, the number that matters on the right, and one short
// grey line with the window, time and scope.
func (r *Renderer) alertBlock(a domain.Alert, snaps map[string]domain.QuotaSnapshot, rep *domain.Report) blockSpec {
	color, phrase := alertPhrase(a)
	spec := blockSpec{alarm: color == "red"}
	if spec.alarm {
		if _, usable := r.siblingHeadroom(rep, a.Credential); usable {
			// The account is out, its channel is not: no red row.
			spec.alarm = false
		}
	}
	if a.Credential.Key == "" && strings.TrimSpace(a.Credential.Alias) == "" && strings.TrimSpace(a.Credential.ShortID) == "" {
		// Not about one account (e.g. a channel-wide change).
		spec.left = inlineTag(color, phrase)
		var sub []string
		if t := oneLine(a.Title); t != "" {
			sub = append(sub, t)
		}
		if s := alertScopeText(a); s != "" {
			sub = append(sub, s)
		}
		if len(sub) > 0 {
			spec.sub = []string{grey(strings.Join(sub, " · "))}
		}
		return spec
	}
	p := a.Credential.Provider
	spec.left = r.brandName(p) + " `" + shortCredentialName(a.Credential) + "` " + inlineTag(color, phrase)

	// Design C: the number that matters on the right with its time in small
	// grey under it; the left keeps name and tag, plus one grey line only for
	// context a reader acts on (scope, what the channel has left).
	var sub []string
	snap, ok := snaps[a.Credential.Key]
	switch a.Kind {
	case domain.AlertRateLimited, domain.AlertRateLimitCleared:
		facts := alertFacts(a)
		if code := facts[domain.FactPrefixStatus]; code != "" {
			spec.big, spec.bigColor = code, "orange"
			if a.Kind == domain.AlertRateLimitCleared {
				spec.bigColor = "green"
			} else if quotaCooldown(a) {
				spec.bigColor = "red"
			}
		}
		if a.Kind == domain.AlertRateLimited {
			if t, err := time.Parse(time.RFC3339, facts[domain.FactPrefixRecovery]); err == nil {
				spec.bigSub = "预计 " + r.formatShort(t) + " 恢复"
			}
		} else if d := durationFact(facts[domain.FactPrefixDuration]); d != "" {
			spec.bigSub = "共 " + d
		}
	case domain.AlertCredential:
		if ok && snap.Code != "" {
			spec.big, spec.bigColor = snap.Code, color
		}
		if ok && snap.Failure != domain.FailureNone && snap.Failure != "" {
			spec.bigSub = domain.FailureReason(snap.Failure)
		}
		if p == domain.ProviderOllama {
			sub = append(sub, "需更新 Cookie")
		}
	case domain.AlertStale, domain.AlertSuspect:
		if ok && snap.Code != "" {
			spec.big, spec.bigColor = snap.Code, "grey"
		}
		if ok {
			if snap.LastSuccessAt.IsZero() {
				spec.bigSub = "从未成功"
			} else {
				spec.bigSub = "上次成功 " + r.formatShort(snap.LastSuccessAt)
			}
		}
	default:
		if !ok {
			break
		}
		if w, found := matchAlertWindow(a, snap.Windows); found && w.UsedPercent != nil {
			spec.big, spec.bigColor = remainingPct(*w.UsedPercent), alertPctColor(a, *w.UsedPercent)
			if txt := r.resetText(w); txt != "未上报" {
				if a.Kind == domain.AlertQuotaExhausted {
					spec.bigSub = shortWindowName(w) + " · " + txt + " 恢复"
				} else {
					spec.bigSub = shortWindowName(w) + " · " + txt + " 刷新"
				}
			} else {
				spec.bigSub = shortWindowName(w)
			}
			break
		}
		// No single window named: every window, as in the panel.
		for _, w := range orderedWindows(snap.Windows) {
			if w.UsedPercent == nil {
				continue
			}
			part := shortWindowName(w) + " 剩 " + remainingPct(*w.UsedPercent)
			if w.Scope.Normalized() == domain.ScopeUnknown {
				// Unknown applicability is evidence; never let it read as account-wide.
				part += "（范围未知）"
			}
			sub = append(sub, part)
		}
	}
	// An account that went out of use: say whether its channel still has room.
	if a.Kind == domain.AlertQuotaExhausted || a.Kind == domain.AlertCredential || a.Kind == domain.AlertRateLimited {
		if used, usable := r.siblingHeadroom(rep, a.Credential); usable && used != nil {
			sub = append(sub, "其余号剩 "+remainingPct(*used))
		} else if !usable && rep != nil {
			sub = append(sub, "已无可用号")
		}
	}

	if s := alertScopeText(a); s != "" {
		sub = append(sub, s)
	}
	if len(sub) > 0 {
		spec.sub = []string{grey(strings.Join(sub, " · "))}
	}
	return spec
}

// alertScopeText is "仅 模型 X" for a change that applies to one model or
// group only; a scoped recovery never reads as an account-wide one.
func alertScopeText(a domain.Alert) string {
	if sc := a.Scope.Normalized(); a.Scope == "" || sc == domain.ScopeAccount {
		return ""
	}
	scope := "仅 " + domain.ScopeText(a.Scope, a.ScopeID)
	if a.Kind == domain.AlertRecovered || a.Kind == domain.AlertRateLimitCleared {
		scope += "，不代表全账号恢复"
	}
	return scope
}

// siblingHeadroom is the tightest remaining share among the OTHER usable
// accounts of a credential's channel, so an alert about one exhausted account
// can say the channel itself is still fine.
func (r *Renderer) siblingHeadroom(rep *domain.Report, c domain.Credential) (*float64, bool) {
	if rep == nil {
		return nil, false
	}
	for _, p := range rep.Providers {
		if p.Provider != c.Provider {
			continue
		}
		v := r.summarize(rep, p, false)
		var rows []credRow
		for _, row := range v.allRows {
			if row.label != shortCredentialName(c) {
				rows = append(rows, row)
			}
		}
		v.allRows = rows
		used, _, usable := channelHeadroom(v)
		return used, usable
	}
	return nil, false
}

// urgentAlertsLeaveHeadroom reports that every urgent alert is about an
// account whose channel still has usable siblings.
func (r *Renderer) urgentAlertsLeaveHeadroom(msg domain.Message) bool {
	found := false
	for _, a := range msg.Alerts {
		if a.Severity != domain.SeverityUrgent {
			continue
		}
		found = true
		if _, usable := r.siblingHeadroom(msg.Report, a.Credential); !usable {
			return false
		}
	}
	return found
}

// quotaCooldown reports a cooldown alert whose recovery time lies beyond the
// longest backoff CPA applies by itself.
func quotaCooldown(a domain.Alert) bool {
	if a.Kind != domain.AlertRateLimited {
		return false
	}
	at, err := time.Parse(time.RFC3339, alertFacts(a)[domain.FactPrefixRecovery])
	return err == nil && at.Sub(a.OccurredAt) > 30*time.Minute
}

// alertPctColor colours an alert's remaining share by what happened.
func alertPctColor(a domain.Alert, used float64) string {
	switch {
	case a.Kind == domain.AlertQuotaExhausted || used >= 100:
		return "red"
	case a.Kind == domain.AlertQuotaThreshold || used >= 90:
		return "orange"
	default:
		return "green"
	}
}

// alertFacts reads the cooldown watcher's "prefix value" facts.
func alertFacts(a domain.Alert) map[string]string {
	out := map[string]string{}
	for _, f := range a.Facts {
		for _, p := range []string{domain.FactPrefixStatus, domain.FactPrefixRecovery, domain.FactPrefixDuration} {
			if strings.HasPrefix(f, p) {
				out[p] = strings.TrimSpace(strings.TrimPrefix(f, p))
			}
		}
	}
	return out
}

// durationFact turns "12m" into "12 分钟".
func durationFact(v string) string {
	if v == "" {
		return ""
	}
	if d, err := time.ParseDuration(v); err == nil {
		return humanDuration(d)
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(v, "m")); err == nil {
		return humanDuration(time.Duration(n) * time.Minute)
	}
	return v
}

// alertPhrase is the status tag of a change. A suspected diagnosis is always
// labelled as suspected: a flat "凭证失效" would overstate the evidence.
func alertPhrase(a domain.Alert) (color, phrase string) {
	color, phrase = alertKindPhrase(a)
	if quotaCooldown(a) {
		// CPA caps its own backoff at 30 minutes; a cooldown running past
		// that follows the upstream's reset time, i.e. the account is used up.
		return "red", "额度用满"
	}
	if a.Evidence == domain.EvidenceSuspected && a.Kind != domain.AlertSuspect {
		return "orange", "疑似" + phrase
	}
	return color, phrase
}

func alertKindPhrase(a domain.Alert) (color, phrase string) {
	switch a.Kind {
	case domain.AlertQuotaReset:
		return "green", "额度已刷新"
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
	case domain.AlertRateLimited:
		return "orange", "限流冷却中"
	case domain.AlertRateLimitCleared:
		return "green", "限流已解除"
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

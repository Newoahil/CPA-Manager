package render

import (
	"fmt"
	"strings"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// Card builds the Feishu interactive-card (schema 2.0) structure as a
// map[string]any ready to be JSON-marshalled into an "interactive" message.
//
// Layout: a one-line conclusion first, then compact per-provider status, then
// the abnormal items expanded with their evidence and the fixed advice that was
// already attached to the Alert. The advice is never authored here; the render
// layer only forwards Alert.Advice.
func (r *Renderer) Card(msg domain.Message) map[string]any {
	elements := []any{}
	if msg.Report != nil && msg.Report.Degraded {
		elements = append(elements, md("⚠️ **"+r.degradedLine(msg.Report)+"**"))
	}
	kicker := r.kicker(msg)
	if kicker != "" {
		elements = append(elements, md(kicker))
	}

	if msg.Report != nil && len(msg.Report.Providers) > 0 {
		elements = append(elements, md(r.cardProviders(msg.Report)))
	}
	if len(msg.Alerts) > 0 {
		if len(elements) > 0 {
			elements = append(elements, map[string]any{"tag": "hr"})
		}
		elements = append(elements, r.cardAlerts(msg.Alerts)...)
	}
	if len(elements) == 0 {
		elements = append(elements, md("暂无可展示的额度信息。"))
	}

	elements = append(elements, map[string]any{"tag": "hr"})
	elements = append(elements, map[string]any{
		"tag":  "button",
		"text": map[string]any{"tag": "plain_text", "content": "刷新额度"},
		"type": "primary",
		// value must be an object: Feishu rejects a bare string here.
		"behaviors": []any{
			map[string]any{"type": "callback", "value": refreshValue},
		},
	})

	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true, "wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": r.titleFor(msg)},
			"template": r.cardColor(msg),
		},
		"body": map[string]any{"elements": elements},
	}
}

// kicker is the first-screen conclusion: who to use more, who to ease off, who
// must re-authenticate.
func (r *Renderer) kicker(msg domain.Message) string {
	if msg.Report != nil && len(msg.Report.Recommendations) > 0 {
		c := r.conclusion(msg.Report)
		if c != "" {
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
				ease = append(ease, a.Credential.Label()+"（account）")
			} else {
				ease = append(ease, a.Credential.Label()+"（仅 "+string(a.Scope.Normalized())+":"+a.ScopeID+"）")
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

func (r *Renderer) cardProviders(rep *domain.Report) string {
	var b strings.Builder
	b.WriteString("**各渠道状态**\n")
	for _, p := range rep.Providers {
		src, fetched := r.sourcesAndFetched(p, rep.GeneratedAt)
		fmt.Fprintf(&b, "- **%s** %s（%d/%d）", r.displayName(p.Provider), stateLabel(p.WorstState), p.Healthy, p.Total)
		if flags := r.providerFlags(p); len(flags) > 0 {
			fmt.Fprintf(&b, "［%s］", strings.Join(flags, "·"))
		}
		b.WriteByte('\n')
		for _, w := range p.BestWindows {
			fmt.Fprintf(&b, "  - %s\n", windowLine(w))
		}
		for _, s := range p.Snapshots {
			for _, w := range s.Windows {
				fmt.Fprintf(&b, "  - %s：%s", s.Credential.Label(), windowLine(w))
				if s.Stale {
					b.WriteString("［旧值］")
				}
				b.WriteByte('\n')
			}
		}
		for _, rec := range rep.Recommendations {
			if rec.Provider == p.Provider {
				fmt.Fprintf(&b, "  - 建议：%s\n", rec.Reason)
			}
		}
		fmt.Fprintf(&b, "  - 来源 %s · 最后成功 %s\n", src, r.formatTime(fetched))
		if p.Error != "" {
			fmt.Fprintf(&b, "  - 渠道错误：%s\n", oneLine(p.Error))
		}
		if last := latestSuccess(p); !last.IsZero() {
			for _, s := range p.Snapshots {
				if s.Stale || !s.OK {
					fmt.Fprintf(&b, "  - %s：数据已过期（最后成功：%s）\n", s.Credential.Label(), r.formatTime(last))
					break
				}
			}
		}
	}
	return b.String()
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
			switch p.WorstState {
			case domain.StateExhausted, domain.StateInvalid:
				if severityRank(domain.SeverityUrgent) > severityRank(worst) {
					worst = domain.SeverityUrgent
				}
			case domain.StateWarning, domain.StateLimited:
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

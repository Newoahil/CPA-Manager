package render

import (
	"fmt"
	"strings"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// Text renders the plain-text form used by the webhook and logs. It keeps the
// exact evidence and never emits credentials, tokens or raw payloads.
func (r *Renderer) Text(msg domain.Message) string {
	var b strings.Builder
	title := r.titleFor(msg)
	fmt.Fprintf(&b, "【%s】\n", title)
	if msg.Report != nil {
		fmt.Fprintf(&b, "生成时间：%s\n", r.formatTime(msg.Report.GeneratedAt))
		if msg.Report.Degraded {
			fmt.Fprintf(&b, "%s\n", r.degradedLine(msg.Report))
		}
	}
	if msg.Body != "" {
		b.WriteString(msg.Body)
		b.WriteByte('\n')
	}

	if msg.Report != nil && len(msg.Report.Providers) > 0 {
		b.WriteString(r.textProviders(msg.Report))
	}
	if len(msg.Alerts) > 0 {
		b.WriteString(r.textAlerts(msg.Alerts))
	}
	if strings.TrimSpace(b.String()) == fmt.Sprintf("【%s】\n", title) {
		b.WriteString("暂无可展示的额度信息。\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// degradedLine is the reader-facing warning that this picture is incomplete.
func (r *Renderer) degradedLine(rep *domain.Report) string {
	line := "⚠️ 本次数据不完整（降级）"
	if len(rep.Notes) > 0 {
		line += "：" + strings.Join(rep.Notes, "；")
	}
	return line
}

func (r *Renderer) textProviders(rep *domain.Report) string {
	var b strings.Builder
	conclusion := r.conclusion(rep)
	if conclusion != "" {
		fmt.Fprintf(&b, "结论：%s\n", conclusion)
	}
	b.WriteString("各渠道状态：\n")
	for _, p := range rep.Providers {
		src, fetched := r.sourcesAndFetched(p, rep.GeneratedAt)
		fmt.Fprintf(&b, "- %s：%s（%d/%d 个凭证）", r.displayName(p.Provider), stateLabel(p.WorstState), p.Healthy, p.Total)
		if flags := r.providerFlags(p); len(flags) > 0 {
			fmt.Fprintf(&b, "［%s］", strings.Join(flags, "·"))
		}
		if p.BestWindows != nil {
			parts := make([]string, 0, len(p.BestWindows))
			for _, w := range p.BestWindows {
				parts = append(parts, windowLine(w))
			}
			if len(parts) > 0 {
				fmt.Fprintf(&b, "｜ %s", strings.Join(parts, "；"))
			}
		}
		fmt.Fprintf(&b, "｜ 来源 %s｜ 最后成功 %s\n", src, r.formatTime(fetched))
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
		if p.Error != "" {
			fmt.Fprintf(&b, "  - 渠道错误：%s\n", oneLine(p.Error))
		}
		if last := latestSuccess(p); !last.IsZero() {
			// Freshness is stated explicitly because a failed cycle must never
			// make carried-over numbers look current.
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

func (r *Renderer) textAlerts(alerts []domain.Alert) string {
	var b strings.Builder
	b.WriteString("告警：\n")
	for _, a := range alerts {
		fmt.Fprintf(&b, "[%s·%s] %s 额度告警\n", severityLabel(a.Severity), evidenceLabel(a.Evidence), a.Credential.Label())
		if a.Detail != "" {
			fmt.Fprintf(&b, "  %s\n", a.Detail)
		}
		for _, f := range a.Facts {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
		if a.Advice != "" {
			fmt.Fprintf(&b, "  建议：%s\n", a.Advice)
		}
	}
	if u := r.cpaPageURL(); u != "" {
		fmt.Fprintf(&b, "管理页：%s\n", u)
	}
	return b.String()
}

// conclusion is the one-line "who to use" summary. It is derived only from
// domain.Recommendation and credential states, never invented.
func (r *Renderer) conclusion(rep *domain.Report) string {
	var use, ease []string
	var reauth []string
	for _, rec := range rep.Recommendations {
		switch rec.Direction {
		case domain.DirectionUseMore:
			use = append(use, r.displayName(rec.Provider))
		case domain.DirectionEaseOff:
			ease = append(ease, r.displayName(rec.Provider))
		case domain.DirectionReauth:
			reauth = append(reauth, r.displayName(rec.Provider))
		}
	}
	var clauses []string
	if len(ease) > 0 {
		if r.formal() {
			clauses = append(clauses, strings.Join(ease, "、")+" 所选凭证用量偏高，请控制该凭证的使用")
		} else {
			clauses = append(clauses, strings.Join(ease, "、")+" 所选凭证先省着点用")
		}
	}
	if len(use) > 0 {
		if r.formal() {
			clauses = append(clauses, strings.Join(use, "、")+" 所选凭证已报告窗口余量充足，可正常使用该凭证")
		} else {
			clauses = append(clauses, strings.Join(use, "、")+" 所选凭证已报告窗口余量充足，可以多用这个凭证")
		}
	}
	if len(reauth) > 0 {
		clauses = append(clauses, strings.Join(reauth, "、")+" 需要重新登录")
	}
	if len(clauses) == 0 {
		for _, p := range rep.Providers {
			if p.WorstState == domain.StateInvalid && p.Total > 0 {
				reauth = append(reauth, r.displayName(p.Provider))
			}
		}
		if len(reauth) > 0 {
			clauses = append(clauses, strings.Join(reauth, "、")+" 需要重新登录")
		}
	}
	return strings.Join(clauses, "；")
}

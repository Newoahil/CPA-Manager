package evaluate

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

// A tuple encoding avoids collisions between scope IDs and window names.
func scopeKey(w domain.QuotaWindow) string {
	id := w.ScopeID
	if w.Scope == domain.ScopeAccount {
		id = ""
	}
	// Unknown windows cannot be assumed to constrain the same resource.
	if w.Scope == domain.ScopeUnknown {
		id = w.ScopeID + "\x00" + w.Name
	}
	b, _ := json.Marshal([]string{string(w.Scope), id})
	return string(b)
}

func normalizedWindows(in []domain.QuotaWindow) []domain.QuotaWindow {
	out := append([]domain.QuotaWindow(nil), in...)
	for i := range out {
		out[i].Scope = out[i].Scope.Normalized()
		if (out[i].Scope == domain.ScopeModel || out[i].Scope == domain.ScopeGroup) && out[i].ScopeID == "" {
			out[i].Scope = domain.ScopeUnknown
		}
		if out[i].Scope == domain.ScopeAccount {
			out[i].ScopeID = ""
		}
	}
	return out
}

// evaluateScopes keeps every measurement. Only account evidence projects onto
// credential quota health. Alerts and resets are independently deduped per window.
func (e *Engine) evaluateScopes(out *state.CredentialRecord, rec state.CredentialRecord, snap domain.QuotaSnapshot, now time.Time) (state.QuotaLevel, []domain.Alert, bool) {
	th := e.thresholdsFor(snap.Credential.Provider)
	out.Scopes = map[string]state.ScopeRecord{}
	var alerts []domain.Alert
	resetFired := false
	var account []domain.QuotaWindow
	for _, w := range snap.Windows {
		key := scopeKey(w)
		sr, exists := out.Scopes[key]
		if !exists {
			sr = state.ScopeRecord{Scope: w.Scope, ScopeID: w.ScopeID, Level: state.QuotaUnknown, Windows: map[string]state.ScopeWindowRecord{}}
		}
		level, _ := quotaLevelFromWindows([]domain.QuotaWindow{w}, th)
		old, known := rec.Scopes[key].Windows[w.Name]
		// Old files have measurements but no applicability. They may seed reset
		// detection for this named window, never an account-wide verdict.
		if !known && len(rec.Scopes) == 0 {
			if v, ok := rec.LastUsedPercent[w.Name]; ok {
				old.Window = domain.QuotaWindow{Name: w.Name, UsedPercent: &v, Scope: domain.ScopeUnknown}
				if rt, ok := rec.LastResetAt[w.Name]; ok {
					old.Window.ResetAt = &rt
				}
				// Legacy values can seed reset comparisons, but not suppress the
				// first explicitly scoped threshold transition.
				old.ReachedNotice = rec.ReachedNotice
				old.ObservedAt = rec.LastSuccessAt
				known = true
			}
		}
		reset := false
		if w.UsedPercent != nil && known {
			reset = old.Window.UsedPercent != nil && *old.Window.UsedPercent-*w.UsedPercent > dropResetThreshold
			if rt := old.Window.ResetAt; rt != nil && !now.Before(*rt) && old.ObservedAt.Before(*rt) {
				reset = true
			}
		}
		if reset && old.ReachedNotice {
			a := resetAlert(snap.Credential, w, now, e.loc())
			a.Scope, a.ScopeID = w.Scope, w.ScopeID
			a.Detail = snap.Credential.Label() + " 的 " + w.ScopeText() + " 窗口 " + w.DisplayLabel() + " 已刷新；仅表示该窗口用量变化。"
			alerts = append(alerts, a)
			resetFired = true
		}
		if quotaRank(level) >= quotaRank(state.QuotaNotice) && quotaRank(level) > quotaRank(old.Level) {
			one := snap
			one.Windows = []domain.QuotaWindow{w}
			a := e.thresholdAlert(snap.Credential, one, level, now)
			if level == state.QuotaExhausted && w.Scope != domain.ScopeUnknown {
				a = e.exhaustedAlert(snap.Credential, one, "", now)
			}
			a.Scope, a.ScopeID = w.Scope, w.ScopeID
			// Name the window so the alert card can show its remaining share
			// and refresh time instead of listing every window.
			a.Facts = append([]string{"窗口: " + w.DisplayLabel()}, a.Facts...)
			a.Detail = snap.Credential.Label() + " 的 " + w.ScopeText() + " 窗口 " + w.DisplayLabel() + " 已用 " + pct(*w.UsedPercent) + "。"
			if w.Scope != domain.ScopeAccount {
				a.Advice = "仅针对该 scope 检查或调整使用，不建议停用整凭证；无自动操作。"
				if w.Scope == domain.ScopeUnknown {
					a.Evidence = domain.EvidenceUnknown
					a.Title = "额度窗口告警（scope unknown）"
					a.Advice = "窗口适用范围未知，不能确认整凭证耗尽或建议停用；无自动操作。"
				}
			}
			alerts = append(alerts, a)
		}
		if quotaRank(level) > quotaRank(sr.Level) {
			sr.Level = level
		}
		sr.Windows[w.Name] = state.ScopeWindowRecord{Window: w, Level: level, ReachedNotice: old.ReachedNotice || quotaRank(level) >= quotaRank(state.QuotaNotice), ObservedAt: now}
		out.Scopes[key] = sr
		if w.Scope == domain.ScopeAccount {
			account = append(account, w)
		}
	}
	// Retain the old fields for file readers, but replace rather than merge so
	// disappeared windows cannot reappear as current evidence on a later failure.
	out.LastUsedPercent = map[string]float64{}
	out.LastResetAt = map[string]time.Time{}
	for _, w := range snap.Windows {
		if w.UsedPercent != nil {
			out.LastUsedPercent[w.Name] = *w.UsedPercent
		}
		if w.ResetAt != nil {
			out.LastResetAt[w.Name] = *w.ResetAt
		}
	}
	level, _ := quotaLevelFromWindows(account, th)
	return level, alerts, resetFired
}

// scopedSummary states what the measured scopes do and do not prove.
//
// It reports counts rather than enumerating every scope identity: the previous
// form spelled out each internal ScopeID (e.g.
// "group:antigravity/groups/Gemini%20Models#1=healthy"), which leaked an
// internal path into chat and made the sentence unreadable. The precise
// per-window numbers remain in the snapshots, which is where a reader looks
// for them.
func scopedSummary(scopes map[string]state.ScopeRecord) string {
	var available, exhausted, unknown int
	accountExhausted := false
	for _, s := range scopes {
		if s.Scope == domain.ScopeUnknown {
			unknown++
			continue
		}
		if s.Scope == domain.ScopeAccount && s.Level == state.QuotaExhausted {
			accountExhausted = true
		}
		if s.Level == state.QuotaExhausted {
			exhausted++
		} else if s.Level != state.QuotaUnknown {
			available++
		}
	}
	prefix := ""
	if available > 0 {
		prefix = "仍有可用 scope（" + strconv.Itoa(available) + " 个），不代表整个 provider 充足"
	}
	if exhausted > 0 && available == 0 {
		prefix = "已观测模型组均耗尽（" + strconv.Itoa(exhausted) + " 个），覆盖范围未证实，不代表全账号耗尽；无自动操作"
	}
	if unknown > 0 {
		prefix += "；存在 " + strconv.Itoa(unknown) + " 个 scope unknown 的窗口，保留数值，不能判断全账号容量"
	}
	if accountExhausted {
		prefix = "account scope 已确认耗尽，建议暂停该单个凭证直到刷新；局部窗口余量不解除 account 限制"
	}
	return strings.TrimPrefix(prefix, "；")
}

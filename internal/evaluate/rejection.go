package evaluate

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

// Rejections use a scope tuple, not a window or account quota level. There is
// no fabricated percentage and no raw request/error/account context in state.
func rejectionIdentity(scope domain.QuotaScope, id string) (string, state.QuotaRejection) {
	scope = scope.Normalized()
	if scope == domain.ScopeAccount {
		id = ""
	}
	if (scope == domain.ScopeModel || scope == domain.ScopeGroup) && id == "" {
		scope = domain.ScopeUnknown
	}
	b, _ := json.Marshal([]string{string(scope), id})
	return string(b), state.QuotaRejection{Scope: scope, ScopeID: id}
}

// rejectionLabel is reader-facing: it must not spell out an internal ScopeID
// path. The machine-facing tuple stays in state under the rejection key.
func rejectionLabel(r state.QuotaRejection) string {
	return domain.ScopeText(r.Scope, r.ScopeID)
}

func sortedRejections(rec state.CredentialRecord) []state.QuotaRejection {
	keys := make([]string, 0, len(rec.Rejections))
	for k := range rec.Rejections {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]state.QuotaRejection, 0, len(keys))
	for _, k := range keys {
		out = append(out, rec.Rejections[k])
	}
	return out
}

func hasAccountRejection(rec state.CredentialRecord) bool {
	for _, r := range rec.Rejections {
		if r.Scope == domain.ScopeAccount {
			return true
		}
	}
	return false
}

func (e *Engine) recordRejection(out *state.CredentialRecord, snap domain.QuotaSnapshot, evidence string, now time.Time) []domain.Alert {
	key, r := rejectionIdentity(snap.FailureScope, snap.FailureScopeID)
	if _, exists := out.Rejections[key]; exists {
		return nil
	}
	if out.Rejections == nil {
		out.Rejections = map[string]state.QuotaRejection{}
	}
	r.ObservedAt = now
	out.Rejections[key] = r
	a := domain.Alert{
		Kind: domain.AlertQuotaThreshold, Severity: domain.SeverityWarn, Evidence: domain.EvidenceUnknown,
		Credential: snap.Credential, Scope: r.Scope, ScopeID: r.ScopeID,
		Title:  "额度请求被拒绝（范围受限）",
		Detail: "上游报告 " + rejectionLabel(r) + " 额度拒绝；未确认 account scope 耗尽。",
		Advice: "检查对应请求范围，不建议停用整凭证；无自动操作。", OccurredAt: now,
	}
	if r.Scope == domain.ScopeAccount {
		// Error evidence is distinct from any old measured windows.
		a = e.exhaustedAlert(snap.Credential, domain.QuotaSnapshot{}, evidence, now)
		a.Detail = "上游明确报告 account scope 额度拒绝，适用于该单个凭证。"
	} else if r.Scope != domain.ScopeUnknown {
		a.Evidence = domain.EvidenceConfirmed
	}
	return []domain.Alert{a}
}

func recoverRejections(out *state.CredentialRecord, snap domain.QuotaSnapshot, now time.Time) []domain.Alert {
	var alerts []domain.Alert
	for _, r := range sortedRejections(*out) {
		observed := false
		for _, w := range snap.Windows {
			if w.UsedPercent == nil {
				continue
			}
			// An unscoped rejection cannot establish scope equivalence. We only
			// announce that quota collection succeeded, not that requests work.
			if r.Scope == domain.ScopeUnknown || (w.Scope == r.Scope && w.ScopeID == r.ScopeID) {
				observed = true
			}
		}
		if !observed {
			continue
		}
		key, _ := rejectionIdentity(r.Scope, r.ScopeID)
		delete(out.Rejections, key)
		alerts = append(alerts, domain.Alert{
			Kind: domain.AlertRecovered, Severity: domain.SeverityInfo, Evidence: domain.EvidenceConfirmed,
			Credential: snap.Credential, Scope: r.Scope, ScopeID: r.ScopeID,
			Title:  "拒绝记录后的额度查询已成功",
			Detail: rejectionLabel(r) + " 拒绝记录后的额度查询已成功；仅恢复查询观测，不代表请求限制解除或全局额度回落。",
			Advice: "按当前各 scope 测量判断容量，不据此自动操作。", OccurredAt: now,
		})
	}
	return alerts
}

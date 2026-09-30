package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestScopedReportAndAlertsRemainScopedInBothFormats(t *testing.T) {
	u := 100.0
	cred := domain.Credential{Provider: domain.ProviderGeminiCLI, Alias: "fixture"}
	w := domain.QuotaWindow{Name: "day", Scope: domain.ScopeModel, ScopeID: "model-a", UsedPercent: &u}
	msg := domain.Message{Report: &domain.Report{Providers: []domain.ProviderReport{{Provider: domain.ProviderGeminiCLI, Snapshots: []domain.QuotaSnapshot{{Credential: cred, OK: true, Windows: []domain.QuotaWindow{w, {Name: "legacy", UsedPercent: &u}}}}}}, Recommendations: []domain.Recommendation{{Provider: domain.ProviderGeminiCLI, Direction: domain.DirectionSteady, Reason: "仍有可用 scope"}}}, Alerts: []domain.Alert{{Kind: domain.AlertQuotaExhausted, Scope: domain.ScopeModel, ScopeID: "model-a", Credential: cred}}}
	r := New("formal")
	b, err := json.Marshal(r.Card(msg))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{r.Text(msg), string(b)} {
		// Scope survives the compact rendering, but as readable applicability
		// ("模型 model-a") rather than the internal tuple. Unknown applicability
		// stays visible at provider level even when its window is folded away.
		for _, want := range []string{"Gemini CLI", "模型 model-a", "范围未知", "0.0%", "仍有可用 scope"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %s: %s", want, text)
			}
		}
		if strings.Contains(text, "额度耗尽，请先别用") || strings.Contains(text, "余量充足") {
			t.Fatal(text)
		}
	}
}

// TestFoldedWindowsAreCountedNotDropped: the compact view shows the tightest
// window per credential, but it must say how many it deferred. Silently
// dropping windows would lose evidence to save space.
func TestFoldedWindowsAreCountedNotDropped(t *testing.T) {
	high, low := 90.0, 10.0
	cred := domain.Credential{Key: "k", Provider: domain.ProviderCodex, Alias: "acct"}
	p := domain.ProviderReport{
		Provider: domain.ProviderCodex,
		Total:    1,
		States:   map[string]domain.CredentialState{"k": domain.StateWarning},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: cred, OK: true, Source: domain.SourceCPA, Confidence: domain.ConfidenceReported,
			Windows: []domain.QuotaWindow{
				{Name: "codex/rate_limit/primary_window", Label: "账号 · 主额度窗口", Scope: domain.ScopeAccount, UsedPercent: &high},
				{Name: "codex/rate_limit/secondary_window", Label: "账号 · 次额度窗口", Scope: domain.ScopeAccount, UsedPercent: &low},
			},
		}},
	}
	msg := domain.Message{Report: &domain.Report{Providers: []domain.ProviderReport{p}}}
	r := New("casual")

	compact := r.Text(msg)
	if !strings.Contains(compact, "90.0%") || !strings.Contains(compact, "另 1 个窗口") {
		t.Fatalf("compact view lost the folded-window evidence:\n%s", compact)
	}
	if strings.Contains(compact, "10.0%") {
		t.Fatalf("compact view should defer the non-tightest window:\n%s", compact)
	}

	msg.Detailed = true
	full := r.Text(msg)
	for _, want := range []string{"90.0%", "10.0%", "账号 · 主额度窗口", "账号 · 次额度窗口"} {
		if !strings.Contains(full, want) {
			t.Fatalf("single-channel view missing %q:\n%s", want, full)
		}
	}
}

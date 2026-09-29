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
		for _, want := range []string{"Gemini CLI", "model:model-a", "unknown", "100.0%", "仍有可用 scope"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %s: %s", want, text)
			}
		}
		if strings.Contains(text, "额度耗尽，请先别用") || strings.Contains(text, "余量充足") {
			t.Fatal(text)
		}
	}
}

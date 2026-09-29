package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestLimitedStateAndScopedRecoveryRendering(t *testing.T) {
	r := New("formal")
	msg := domain.Message{Report: &domain.Report{Providers: []domain.ProviderReport{{Provider: domain.ProviderGeminiCLI, WorstState: domain.StateLimited, Total: 1}}}, Alerts: []domain.Alert{{Kind: domain.AlertRecovered, Severity: domain.SeverityInfo, Scope: domain.ScopeGroup, ScopeID: "model-a", Detail: "group:model-a 额度查询已成功；不代表请求限制解除或全局额度回落。"}}}
	card := r.Card(msg)
	if r.cardColor(msg) != "orange" {
		t.Fatal("limited scope rendered as normal")
	}
	b, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{r.Text(msg), string(b)} {
		if !strings.Contains(text, "范围受限（局部额度）") || !strings.Contains(text, "group:model-a") || !strings.Contains(text, "不代表请求限制解除或全局额度回落") {
			t.Fatal(text)
		}
	}
}

package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestLimitedStateAndScopedRecoveryRendering(t *testing.T) {
	r := New("formal")
	msg := domain.Message{Report: &domain.Report{Providers: []domain.ProviderReport{{Provider: domain.ProviderGeminiCLI, WorstState: domain.StateLimited, Total: 1}}}, Alerts: []domain.Alert{{Kind: domain.AlertRecovered, Severity: domain.SeverityInfo, Scope: domain.ScopeGroup, ScopeID: "model-a", Detail: "分组 model-a 额度查询已成功；不代表请求限制解除或全局额度回落。"}}}
	card := r.Card(msg)
	if r.cardColor(msg) != "orange" {
		t.Fatal("limited scope rendered as normal")
	}
	b, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{r.Text(msg), string(b)} {
		// The folded line must still say the coverage is partial: a limited
		// credential is never allowed to render as a plain healthy count.
		if !strings.Contains(text, "局部额度") || !strings.Contains(text, "分组 model-a") || !strings.Contains(text, "不代表请求限制解除或全局额度回落") {
			t.Fatal(text)
		}
	}
	if strings.Contains(r.Text(msg), "正常") {
		t.Fatalf("limited coverage claimed as healthy:\n%s", r.Text(msg))
	}
}

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
	// An alert card's header colour follows the alert kind (a recovery is
	// green), not the severity and not the unrelated provider in the report.
	if r.cardColor(msg) != "green" {
		t.Fatal("a scoped recovery alert should render the header green")
	}
	b, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	// The text keeps the full wording: the folded line must say the coverage
	// is partial, and a scoped recovery must not read as account-wide.
	text := r.Text(msg)
	if !strings.Contains(text, "局部额度") || !strings.Contains(text, "分组 model-a") || !strings.Contains(text, "不代表请求限制解除或全局额度回落") {
		t.Fatal(text)
	}
	// The alert card states the same limit in one line.
	if card := string(b); !strings.Contains(card, "分组 model-a") || !strings.Contains(card, "不代表全账号恢复") {
		t.Fatal(card)
	}
	if strings.Contains(r.Text(msg), "正常") {
		t.Fatalf("limited coverage claimed as healthy:\n%s", r.Text(msg))
	}
}

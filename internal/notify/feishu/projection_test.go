package feishu

import (
	"strings"
	"testing"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestLimitedScopesRemainInAttentionFilter(t *testing.T) {
	r := domain.Report{Providers: []domain.ProviderReport{{Provider: domain.ProviderGeminiCLI, WorstState: domain.StateLimited}, {Provider: domain.ProviderCodex, WorstState: domain.StateHealthy}}}
	got, note := filterReport(r, intent{kind: intentAbnormal})
	if note != "" || len(got.Providers) != 1 || got.Providers[0].WorstState != domain.StateLimited {
		t.Fatalf("limited scope hidden: %+v %s", got, note)
	}
}

func TestHelpDescribesOAuthRefreshSideEffect(t *testing.T) {
	if strings.Contains(helpText, "只读") || !strings.Contains(helpText, "不主动改启停/策略；查询可能触发CPA自动OAuth续期") {
		t.Fatal(helpText)
	}
}

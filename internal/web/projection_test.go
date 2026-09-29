package web

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestLimitedStateLabelsUseExistingBadgeStyle(t *testing.T) {
	u := 100.0
	r := domain.Report{Providers: []domain.ProviderReport{{Provider: domain.ProviderGeminiCLI, Total: 1, WorstState: domain.StateLimited, States: map[string]domain.CredentialState{"fixture": domain.StateLimited}, Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "fixture"}, OK: true, Windows: []domain.QuotaWindow{{Name: "day", Scope: domain.ScopeModel, ScopeID: "model-a", UsedPercent: &u}}}}}}}
	dto := buildStatus(r)
	p := dto.Providers[0]
	if p.WorstState != "limited" || p.Healthy != 0 || p.StateClass != "notice" || !strings.Contains(p.WorstStateLabel, "局部额度") || p.Credentials[0].State != "limited" {
		t.Fatalf("wrong DTO: %+v", p)
	}
	var b bytes.Buffer
	if err := indexTemplate.Execute(&b, pageData{Status: dto}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `class="state notice">limited · 范围受限（局部额度）`) || strings.Contains(b.String(), `class="state healthy"`) {
		t.Fatal(b.String())
	}
}

func TestUnsupportedDTOIsNotFreshOrFalseStale(t *testing.T) {
	d := buildStatus(domain.Report{Providers: []domain.ProviderReport{{Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "fixture"}, Failure: domain.FailureUnsupported}}, States: map[string]domain.CredentialState{"fixture": domain.StateInvalid}}}})
	c := d.Providers[0].Credentials[0]
	if c.Freshness != "unsupported" || c.Stale || c.State != "invalid" || !strings.Contains(c.StateLabel, "unsupported") {
		t.Fatalf("wrong unsupported DTO: %+v", c)
	}
}

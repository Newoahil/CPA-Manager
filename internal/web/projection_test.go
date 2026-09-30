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

// TestStatusPageShowsReadableWindowNamesAndClassCounts: the status page is
// screenshotted and pasted into chat, so it must not print internal window
// paths, and its headline count must not call three scoped-but-usable
// credentials "0 healthy".
func TestStatusPageShowsReadableWindowNamesAndClassCounts(t *testing.T) {
	u := 2.4
	w := domain.QuotaWindow{
		Name:  "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1",
		Label: "Gemini Models · 周", Scope: domain.ScopeGroup,
		ScopeID: "antigravity/groups/Gemini%20Models#1", UsedPercent: &u,
	}
	r := domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderAntigravity, Total: 3, Healthy: 0, Limited: 3,
		WorstState: domain.StateLimited,
		States:     map[string]domain.CredentialState{"k": domain.StateLimited},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "k", Alias: "hongwane3", ShortID: "ade0"},
			OK:         true, Windows: []domain.QuotaWindow{w},
		}},
		BestWindows: []domain.QuotaWindow{w},
	}}}
	dto := buildStatus(r)
	p := dto.Providers[0]
	if p.Normal != 0 || p.Limited != 3 || p.Abnormal != 0 || p.Total != 3 {
		t.Fatalf("class counts = %d/%d/%d of %d", p.Normal, p.Limited, p.Abnormal, p.Total)
	}
	win := p.Credentials[0].Windows[0]
	if win.Label != "Gemini Models · 周" || win.ScopeText != "分组 Gemini Models" {
		t.Fatalf("window not projected readably: %+v", win)
	}
	if win.Name == "" {
		t.Fatal("machine-readable Name must still be in the JSON contract")
	}

	var b bytes.Buffer
	if err := indexTemplate.Execute(&b, pageData{Status: dto}); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	for _, banned := range []string{"antigravity/groups", "%20Models", "/buckets/", "健康 0 / 3"} {
		if strings.Contains(page, banned) {
			t.Errorf("page leaked %q", banned)
		}
	}
	for _, want := range []string{"Gemini Models · 周", "分组 Gemini Models", "受限 3"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

// TestStatusPagePassesThroughOptionalUpstreamFields: plan, extra usage, window
// length, the limit marker and remaining counts all come from the same
// response. Present means shown; absent means absent, with no placeholder.
func TestStatusPagePassesThroughOptionalUpstreamFields(t *testing.T) {
	used, credits, limit, util := 42.0, 12.5, 100.0, 12.5
	r := domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: 1, Normal: 1,
		WorstState: domain.StateHealthy,
		States:     map[string]domain.CredentialState{"k": domain.StateHealthy},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "k", Alias: "acct"}, OK: true,
			Plan:       "plan_max",
			ExtraUsage: &domain.ExtraUsage{Enabled: true, UsedCredits: &credits, MonthlyLimit: &limit, UsedPercent: &util},
			Windows: []domain.QuotaWindow{{
				Name: "codex/rate_limit/primary_window", Label: "账号 · 周窗口",
				Scope: domain.ScopeAccount, UsedPercent: &used,
				WindowSeconds: 604800, LimitReached: true, RemainingAmount: "100",
			}},
		}},
	}}}
	dto := buildStatus(r)
	c := dto.Providers[0].Credentials[0]
	if c.Plan != "plan_max" || c.ExtraUsage == nil || !strings.Contains(c.ExtraUsage.Text, "12.5 / 上限 100 credits") {
		t.Fatalf("optional fields not projected: %+v / %+v", c.Plan, c.ExtraUsage)
	}
	w := c.Windows[0]
	if w.WindowSeconds != 604800 || !w.LimitReached || w.RemainingAmount != "100" {
		t.Fatalf("window extras not projected: %+v", w)
	}

	var b bytes.Buffer
	if err := indexTemplate.Execute(&b, pageData{Status: dto}); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	for _, want := range []string{"套餐 plan_max", "额外用量", "剩余 100", "上游标记已达上限", "账号 · 周窗口"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	for _, banned := range []string{"$", "USD", "未知套餐"} {
		if strings.Contains(page, banned) {
			t.Errorf("page invented %q", banned)
		}
	}

	// Absent optional fields leave no trace.
	bare := domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: 1, Normal: 1,
		States: map[string]domain.CredentialState{"k": domain.StateHealthy},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "k", Alias: "acct"}, OK: true,
			Windows: []domain.QuotaWindow{{Name: "w", Scope: domain.ScopeAccount, UsedPercent: &used}},
		}},
	}}}
	bc := buildStatus(bare).Providers[0].Credentials[0]
	if bc.Plan != "" || bc.ExtraUsage != nil {
		t.Errorf("invented optional fields: %+v / %+v", bc.Plan, bc.ExtraUsage)
	}
	if bc.Windows[0].WindowSeconds != 0 || bc.Windows[0].LimitReached || bc.Windows[0].RemainingAmount != "" {
		t.Errorf("invented window extras: %+v", bc.Windows[0])
	}
}

func TestUnsupportedDTOIsNotFreshOrFalseStale(t *testing.T) {
	d := buildStatus(domain.Report{Providers: []domain.ProviderReport{{Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "fixture"}, Failure: domain.FailureUnsupported}}, States: map[string]domain.CredentialState{"fixture": domain.StateInvalid}}}})
	c := d.Providers[0].Credentials[0]
	if c.Freshness != "unsupported" || c.Stale || c.State != "invalid" || !strings.Contains(c.StateLabel, "unsupported") {
		t.Fatalf("wrong unsupported DTO: %+v", c)
	}
}

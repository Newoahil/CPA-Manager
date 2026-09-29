package quota

import (
	"testing"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestProviderScopeContracts(t *testing.T) {
	for _, tc := range []struct {
		provider, profile, body string
		scopes                  []domain.QuotaScope
		ids                     []string
	}{
		{"codex", "", `{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":20}},"code_review_rate_limit":{"primary_window":{"used_percent":100}},"additional_rate_limits":[{"limit_name":"spark","rate_limit":{"primary_window":{"used_percent":95},"secondary_window":{"used_percent":30}}}]}`, []domain.QuotaScope{domain.ScopeAccount, domain.ScopeAccount, domain.ScopeGroup, domain.ScopeGroup, domain.ScopeGroup}, []string{"", "", "code_review", "codex/additional/spark#1", "codex/additional/spark#1"}},
		{"claude", "", `{"five_hour":{"utilization":10},"seven_day":{"utilization":20},"seven_day_opus":{"utilization":100},"seven_day_oauth_apps":{"utilization":95},"limits":[{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}},"percent":30}]}`, []domain.QuotaScope{domain.ScopeAccount, domain.ScopeAccount, domain.ScopeGroup, domain.ScopeGroup, domain.ScopeModel}, []string{"", "", "seven_day_oauth_apps", "opus", "fable"}},
		{"gemini-cli", "", `{"buckets":[{"modelId":"a","tokenType":"input","remainingFraction":0},{"modelId":"a","tokenType":"output","remainingFraction":0.5},{"modelId":"b","remainingFraction":0.9}]}`, []domain.QuotaScope{domain.ScopeModel, domain.ScopeModel, domain.ScopeModel}, []string{"a", "a", "b"}},
		{"antigravity", "current", `{"groups":[{"displayName":"pro","buckets":[{"bucketId":"a","window":"day","remainingFraction":0},{"bucketId":"b","window":"week","remainingFraction":0.9}]}]}`, []domain.QuotaScope{domain.ScopeGroup, domain.ScopeGroup}, []string{"antigravity/groups/pro#1", "antigravity/groups/pro#1"}},
		{"antigravity", "legacy", `{"models":{"a":{"quotaInfo":{"remainingFraction":0}},"b":{"quotaInfo":{"remainingFraction":0.9}}}}`, []domain.QuotaScope{domain.ScopeModel, domain.ScopeModel}, []string{"a", "b"}},
	} {
		t.Run(tc.provider+tc.profile, func(t *testing.T) {
			c := contextFor(tc.provider)
			c.Profile = tc.profile
			r := Parse(c, 200, []byte(tc.body), testNow)
			if len(r.Windows) != len(tc.scopes) {
				t.Fatalf("result: %+v", r)
			}
			for i, w := range r.Windows {
				if w.Scope != tc.scopes[i] || w.ScopeID != tc.ids[i] {
					t.Fatalf("window %d: %+v", i, w)
				}
				if w.ScopeID == c.AccountID || w.ScopeID == c.ProjectID {
					t.Fatal("scope leaked request context")
				}
			}
		})
	}
}

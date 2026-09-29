package state

import (
	"testing"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestLimitedProjectionPriority(t *testing.T) {
	u := 100.0
	for _, tc := range []struct {
		quota QuotaLevel
		cred  CredentialHealth
		fresh Freshness
		want  domain.CredentialState
	}{
		{QuotaHealthy, CredValid, Fresh, domain.StateLimited},
		{QuotaUnknown, CredValid, Fresh, domain.StateLimited},
		{QuotaNotice, CredValid, Fresh, domain.StateNotice},
		{QuotaWarning, CredValid, Fresh, domain.StateWarning},
		{QuotaExhausted, CredValid, Fresh, domain.StateExhausted},
		{QuotaHealthy, CredValid, Stale, domain.StateStale},
		{QuotaHealthy, CredSuspect, Stale, domain.StateSuspect},
		{QuotaExhausted, CredInvalid, Stale, domain.StateInvalid},
	} {
		r := CredentialRecord{Quota: tc.quota, Credential: tc.cred, Freshness: tc.fresh, Scopes: map[string]ScopeRecord{"local": {Scope: domain.ScopeModel, ScopeID: "model-a", Level: QuotaExhausted, Windows: map[string]ScopeWindowRecord{"day": {Window: domain.QuotaWindow{Scope: domain.ScopeModel, UsedPercent: &u}}}}}}
		if got := r.Project(); got != tc.want {
			t.Fatalf("q=%s c=%s f=%s: %s != %s", tc.quota, tc.cred, tc.fresh, got, tc.want)
		}
	}
}

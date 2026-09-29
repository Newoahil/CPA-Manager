package web

import (
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"testing"
)

func TestScopeDTOPassthrough(t *testing.T) {
	u := 98.0
	for _, scope := range []domain.QuotaScope{domain.ScopeAccount, domain.ScopeModel, domain.ScopeGroup, domain.ScopeUnknown, ""} {
		w := domain.QuotaWindow{Name: "week", Scope: scope, ScopeID: "model-a", UsedPercent: &u}
		d := toWindowDTO(w, false)
		if d.Scope != scope.Normalized() || d.ScopeID != "model-a" || d.UsedPercent == nil || *d.UsedPercent != 98 {
			t.Fatalf("scope DTO: %+v", d)
		}
	}
}

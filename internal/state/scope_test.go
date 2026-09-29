package state

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestScopeStateFileRoundTrip(t *testing.T) {
	u := 100.0
	now := time.Now().UTC().Truncate(time.Second)
	s := Empty()
	s.Credentials["opaque-fixture"] = CredentialRecord{Scopes: map[string]ScopeRecord{
		"model-a": {Scope: domain.ScopeModel, ScopeID: "model-a", Level: QuotaExhausted, Windows: map[string]ScopeWindowRecord{
			"day": {Window: domain.QuotaWindow{Name: "day", Scope: domain.ScopeModel, ScopeID: "model-a", UsedPercent: &u}, Level: QuotaExhausted, ReachedNotice: true, ObservedAt: now},
		}},
	}}
	store := NewFileStore(filepath.Join(t.TempDir(), "scope-state.json"))
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	w := loaded.Credentials["opaque-fixture"].Scopes["model-a"].Windows["day"]
	if w.Window.Scope != domain.ScopeModel || w.Window.ScopeID != "model-a" || w.Window.UsedPercent == nil || *w.Window.UsedPercent != 100 || !w.ReachedNotice || !w.ObservedAt.Equal(now) {
		t.Fatalf("lost scope history: %+v", w)
	}
}

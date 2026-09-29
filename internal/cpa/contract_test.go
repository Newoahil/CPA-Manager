package cpa

import (
	"strings"
	"testing"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestContractExactFractionsAndGroupIdentity(t *testing.T) {
	body := []byte(`{"subscription":{"plan":"fixture-plan"},"summary":[{"key":"credit_balance","value":12.5,"currency":"USD"}],"groups":[{"displayName":"A","buckets":[{"window":"weekly","remainingFraction":0},{"window":"weekly","remainingFraction":1}]},{"displayName":"B","buckets":[{"window":"weekly","remainingFraction":0.95,"resetTime":"2030-01-01T00:00:00Z"}]}]}`)
	w, plan, balance, clamped, shaped := parseContract(body)
	if !shaped || clamped || len(w) != 3 || plan != "fixture-plan" || balance != "12.5 USD" {
		t.Fatalf("parse: %+v %s %s", w, plan, balance)
	}
	for i, want := range []float64{100, 0, 5} {
		if w[i].UsedPercent == nil || *w[i].UsedPercent != want || w[i].Scope != domain.ScopeUnknown || w[i].ScopeID != "" {
			t.Fatalf("window %d: %+v", i, w[i])
		}
	}
	if w[0].Name == w[1].Name || w[0].Name == w[2].Name || !strings.Contains(w[2].Name, "B") || w[2].ResetAt == nil {
		t.Fatal("group/window identity or reset lost")
	}
}

func TestContractInvalidMeasurementRejectsWholeResponse(t *testing.T) {
	for _, value := range []string{`-0.1`, `1.1`, `true`, `"0.5"`, `{}`, `1e999`} {
		body := `{"groups":[{"buckets":[{"remainingFraction":0.5},{"remainingFraction":` + value + `}]}]}`
		w, plan, balance, clamped, _ := parseContract([]byte(body))
		if len(w) != 0 || plan != "" || balance != "" || clamped {
			t.Fatalf("accepted %s", value)
		}
	}
	for _, body := range []string{`{}`, `null`, `{"groups":[]}`, `{"groups":[{"buckets":[{}]}]}`, `{"groups":[{"buckets":[{"remainingFraction":null}]}]}`} {
		w, _, _, _, _ := parseContract([]byte(body))
		if len(w) != 0 {
			t.Fatalf("invented number from %s", body)
		}
	}
}

func TestContractMissingBalanceNotInvented(t *testing.T) {
	_, _, balance, _, _ := parseContract([]byte(`{"summary":[{"key":"balance","currency":"USD"}],"groups":[{"buckets":[{"remainingFraction":0}]}]}`))
	if balance != "" {
		t.Fatal("missing balance became zero")
	}
}

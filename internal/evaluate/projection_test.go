package evaluate

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

func TestLimitedScopeProjectionAndBootstrap(t *testing.T) {
	for _, scope := range []domain.QuotaScope{domain.ScopeModel, domain.ScopeGroup, domain.ScopeUnknown} {
		for _, used := range []float64{10, 90, 95, 100} {
			for _, mixed := range []bool{false, true} {
				windows := []domain.QuotaWindow{scopedWin(scope, "local", "day", used)}
				if mixed {
					windows = append(windows, win("account", 10))
				}
				s := okSnap("k", domain.ProviderClaude, windows...)
				r, alerts, next := newEngine(t).Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, time.Now())
				if next.Credentials["k"].State != domain.StateLimited || r.Providers[0].WorstState != domain.StateLimited || r.Providers[0].Healthy != 0 {
					t.Fatalf("scope=%s used=%v mixed=%v: %+v", scope, used, mixed, r.Providers[0])
				}
				for _, a := range alerts {
					if a.Advice == adviceExhaust {
						t.Fatalf("local scope received account advice: %+v", a)
					}
				}
				_, initial, _ := newEngine(t).Evaluate(state.Empty(), []domain.QuotaSnapshot{s}, time.Now())
				if len(initial) != 1 || !strings.Contains(initial[0].Detail, "范围受限") || strings.Contains(initial[0].Detail, "数据异常") {
					t.Fatalf("bootstrap: %+v", initial)
				}
			}
		}
	}
}

func TestUnsupportedRetainsEvidenceUntilSuccessfulMeasurement(t *testing.T) {
	e := newEngine(t)
	now := time.Now()
	for _, initial := range []domain.FailureKind{domain.FailureAuth, domain.FailureTransport, domain.FailureQuota} {
		prev := bootstrapped()
		for i := 0; i < 3; i++ {
			_, _, prev = e.Evaluate(prev, []domain.QuotaSnapshot{failSnapKind("k", domain.ProviderCodex, "fixture", initial)}, now)
		}
		before := prev.Credentials["k"]
		for i := 0; i < 10; i++ {
			s := failSnapKind("k", domain.ProviderCodex, "unsupported", domain.FailureUnsupported)
			r, alerts, next := e.Evaluate(prev, []domain.QuotaSnapshot{s}, now.Add(time.Minute))
			rec := next.Credentials["k"]
			if rec.Credential != before.Credential || rec.Quota != before.Quota || !rec.CapabilityUnavailable || rec.ConsecutiveFailures != 0 || len(alerts) != 0 {
				t.Fatalf("%s lost evidence: %+v %+v", initial, rec, alerts)
			}
			if initial == domain.FailureQuota && (!r.Providers[0].Snapshots[0].Stale || rec.Freshness != state.Stale) {
				t.Fatal("prior account evidence presented as fresh")
			}
			prev = next
		}
		_, alerts, next := e.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("day", 10))}, now.Add(2*time.Minute))
		if next.Credentials["k"].Credential != state.CredValid || next.Credentials["k"].CapabilityUnavailable {
			t.Fatalf("success did not restore: %+v", next.Credentials["k"])
		}
		if initial != domain.FailureQuota && countKind(alerts, domain.AlertRecovered) != 1 {
			t.Fatalf("%s recovery count: %+v", initial, alerts)
		}
		_, alerts, _ = e.Evaluate(next, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("day", 10))}, now.Add(3*time.Minute))
		if countKind(alerts, domain.AlertRecovered) != 0 {
			t.Fatal("recovery repeated")
		}
	}
}

func TestOnlyConsecutiveNetworkFailuresCount(t *testing.T) {
	e := newEngine(t)
	for _, kind := range []domain.FailureKind{domain.FailureControlPlane, domain.FailureAuth, domain.FailureQuota, domain.FailureUnsupported} {
		for _, precedingNetwork := range []bool{false, true} {
			prev := bootstrapped()
			if precedingNetwork {
				for i := 0; i < 2; i++ {
					_, _, prev = e.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "timeout")}, time.Now())
				}
			}
			for i := 0; i < 2; i++ {
				_, _, prev = e.Evaluate(prev, []domain.QuotaSnapshot{failSnapKind("k", domain.ProviderCodex, "fixture", kind)}, time.Now())
				if prev.Credentials["k"].ConsecutiveFailures != 0 {
					t.Fatalf("%s counted as network failure", kind)
				}
			}
			_, alerts, next := e.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "timeout")}, time.Now())
			if next.Credentials["k"].ConsecutiveFailures != 1 || next.Credentials["k"].Credential == state.CredSuspect || countKind(alerts, domain.AlertSuspect) != 0 {
				t.Fatalf("%s contaminated network sequence: %+v %+v", kind, next, alerts)
			}
		}
	}
}

func TestRejectionsIndependentOfAccountQuotaAndDurable(t *testing.T) {
	e := newEngine(t)
	now := time.Now()
	_, _, prev := e.Evaluate(bootstrapped(), []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("account", 96))}, now)
	store := state.NewFileStore(filepath.Join(t.TempDir(), "scope-state.json"))
	for _, local := range []struct {
		scope domain.QuotaScope
		id    string
	}{{domain.ScopeUnknown, ""}, {domain.ScopeGroup, "a"}, {domain.ScopeGroup, "b"}} {
		s := failSnapKind("k", domain.ProviderCodex, "synthetic-error-not-for-storage", domain.FailureQuota)
		s.Credential.AuthIndex = "synthetic-live-handle"
		s.Credential.Status = "synthetic-account-context"
		s.FailureScope, s.FailureScopeID = local.scope, local.id
		_, alerts, next := e.Evaluate(prev, []domain.QuotaSnapshot{s}, now)
		if len(alerts) != 1 || alerts[0].Scope != local.scope || alerts[0].ScopeID != local.id || alerts[0].Advice == adviceExhaust || next.Credentials["k"].Quota != state.QuotaWarning {
			t.Fatalf("new rejection hidden by account warning: %+v %+v", alerts, next)
		}
		if err := store.Save(next); err != nil {
			t.Fatal(err)
		}
		loaded, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(loaded)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"synthetic-live-handle", "synthetic-account-context", "synthetic-error-not-for-storage", "auth_index"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("state persisted forbidden context %s", forbidden)
			}
		}
		_, alerts, prev = e.Evaluate(loaded, []domain.QuotaSnapshot{s}, now)
		if len(alerts) != 0 {
			t.Fatalf("restart lost rejection dedup: %+v", alerts)
		}
	}
	if len(prev.Credentials["k"].Rejections) != 3 {
		t.Fatal("scope identities collapsed")
	}
	// Account success clears only the unknown rejection's collection barrier.
	s := okSnap("k", domain.ProviderCodex, win("account", 96))
	_, alerts, next := e.Evaluate(prev, []domain.QuotaSnapshot{s}, now)
	if countKind(alerts, domain.AlertRecovered) != 1 || len(next.Credentials["k"].Rejections) != 2 {
		t.Fatalf("unsafe recovery: %+v %+v", alerts, next)
	}
	a, _ := findAlert(alerts, domain.AlertRecovered)
	if a.Scope != domain.ScopeUnknown || !strings.Contains(a.Detail, "不代表请求限制解除或全局额度回落") || strings.Contains(a.Detail, "额度用量已回落") {
		t.Fatalf("global recovery invented: %+v", a)
	}
	// Unmeasured local rejections remain and block broad capacity advice.
	s.Windows = []domain.QuotaWindow{win("account", 10)}
	r, _, next := e.Evaluate(next, []domain.QuotaSnapshot{s}, now)
	if next.Credentials["k"].State != domain.StateLimited || r.Providers[0].Healthy != 0 || len(r.Providers[0].BestWindows) != 0 || r.Recommendations[0].Direction != domain.DirectionSteady {
		t.Fatalf("unresolved local rejection hidden: %+v", r)
	}
	s.Windows = append(s.Windows, scopedWin(domain.ScopeGroup, "a", "day", 10))
	_, alerts, next = e.Evaluate(next, []domain.QuotaSnapshot{s}, now)
	if countKind(alerts, domain.AlertRecovered) != 1 || len(next.Credentials["k"].Rejections) != 1 {
		t.Fatal("local recovery cleared another scope")
	}
	if err := store.Save(next); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, alerts, _ = e.Evaluate(loaded, []domain.QuotaSnapshot{s}, now)
	if len(alerts) != 0 || len(loaded.Credentials["k"].Scopes) != 2 {
		t.Fatalf("measurement/rejection roundtrip lost state: %+v", alerts)
	}
}

func TestUnscopedRejectionNeverChangesAccountDimension(t *testing.T) {
	s := failSnapKind("k", domain.ProviderCodex, "quota rejection", domain.FailureQuota)
	s.FailureScope = domain.ScopeUnknown
	e := newEngine(t)
	_, alerts, prev := e.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, time.Now())
	if len(alerts) != 1 || prev.Credentials["k"].Quota != state.QuotaUnknown || prev.Credentials["k"].State != domain.StateLimited {
		t.Fatalf("rejection became account measurement: %+v", prev)
	}
	_, alerts, _ = e.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("day", 10))}, time.Now())
	if len(alerts) != 1 || alerts[0].Scope != domain.ScopeUnknown || strings.Contains(alerts[0].Detail, "额度用量已回落") {
		t.Fatalf("fabricated account recovery: %+v", alerts)
	}
}

func TestAccountRejectionRequiresAccountMeasurementToClear(t *testing.T) {
	e := newEngine(t)
	s := failSnapKind("k", domain.ProviderCodex, "synthetic-error", domain.FailureQuota)
	s.FailureScopeID = "synthetic-account-handle"
	_, _, prev := e.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, time.Now())
	encoded, err := json.Marshal(prev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "synthetic-account-handle") || strings.Contains(string(encoded), "synthetic-error") {
		t.Fatal("account context persisted")
	}
	local := okSnap("k", domain.ProviderCodex, scopedWin(domain.ScopeModel, "model-a", "day", 10))
	_, alerts, next := e.Evaluate(prev, []domain.QuotaSnapshot{local}, time.Now())
	if next.Credentials["k"].Quota != state.QuotaExhausted || next.Credentials["k"].State != domain.StateExhausted || !hasAccountRejection(next.Credentials["k"]) || countKind(alerts, domain.AlertRecovered) != 0 {
		t.Fatalf("local success erased account rejection: %+v %+v", next, alerts)
	}
}

func TestEmptySuccessCannotRecoverCredential(t *testing.T) {
	e := newEngine(t)
	_, _, prev := e.Evaluate(bootstrapped(), []domain.QuotaSnapshot{failSnapKind("k", domain.ProviderCodex, "auth", domain.FailureAuth)}, time.Now())
	_, alerts, next := e.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, domain.QuotaWindow{Name: "day"})}, time.Now())
	if next.Credentials["k"].Credential != state.CredInvalid || countKind(alerts, domain.AlertRecovered) != 0 {
		t.Fatal("empty envelope restored valid")
	}
}

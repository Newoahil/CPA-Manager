package evaluate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

func scopedWin(scope domain.QuotaScope, id, name string, used float64) domain.QuotaWindow {
	w := win(name, used)
	w.Scope, w.ScopeID = scope, id
	return w
}

func TestUnsupportedNeverAccumulatesNetworkFailure(t *testing.T) {
	e := newEngine(t)
	now := time.Now()
	prev := bootstrapped()
	// Include a real previous timeout and anomalous request samples. Neither
	// may turn absent quota capability into a new network/auth/quota verdict.
	_, _, prev = e.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderGeminiCLI, "timeout")}, now)
	s := failSnapKind("k", domain.ProviderGeminiCLI, "401 quota exhausted", domain.FailureUnsupported)
	s.Credential.Status = "expired"
	for i := 0; i < 10; i++ {
		report, alerts, next := e.EvaluateWithSamples(prev, []domain.QuotaSnapshot{s}, map[string]RequestStats{"k": {Total: 20, Failed: 20}}, now.Add(time.Duration(i)*time.Minute))
		r := next.Credentials["k"]
		if r.State != domain.StateUnknown || r.ConsecutiveFailures != 0 || len(alerts) != 0 {
			t.Fatalf("round %d: record=%+v alerts=%+v", i, r, alerts)
		}
		if !report.Degraded || !strings.Contains(strings.Join(report.Notes, " "), "unsupported") {
			t.Fatal("missing capability explanation")
		}
		prev = next
	}
	_, alerts, next := e.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderGeminiCLI, "timeout")}, now)
	if next.Credentials["k"].ConsecutiveFailures != 1 || countKind(alerts, domain.AlertSuspect) != 0 {
		t.Fatal("unsupported bridged consecutive network failures")
	}
}

func TestSuccessfulSnapshotOverridesStaleCredentialText(t *testing.T) {
	e := newEngine(t)
	for _, status := range []string{"unauthorized", "expired", "forbidden", "rate limited", "quota_exceeded"} {
		s := okSnap("k", domain.ProviderCodex, win("5h", 20))
		s.Credential.Status, s.Err = status, "401 quota exhausted"
		prev := bootstrapped()
		for i := 0; i < 4; i++ {
			_, alerts, next := e.Evaluate(prev, []domain.QuotaSnapshot{s}, time.Now())
			if next.Credentials["k"].State != domain.StateHealthy || countKind(alerts, domain.AlertCredential)+countKind(alerts, domain.AlertQuotaExhausted) != 0 {
				t.Fatalf("%s: %+v", status, next)
			}
			prev = next
		}
	}
}

func TestForbiddenAndRateLimitAreNotAuthOrQuotaEvidence(t *testing.T) {
	for _, text := range []string{"forbidden", "403 Forbidden", "rate limited", "429 rate limit exceeded"} {
		s := failSnapKind("k", domain.ProviderCodex, text, domain.FailureNone)
		s.Credential.Status = text
		_, alerts, next := newEngine(t).Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, time.Now())
		if next.Credentials["k"].State == domain.StateInvalid || next.Credentials["k"].State == domain.StateExhausted || countKind(alerts, domain.AlertCredential)+countKind(alerts, domain.AlertQuotaExhausted) != 0 {
			t.Fatalf("%s: %+v", text, alerts)
		}
	}
}

func TestScopedThresholdsPersistAndDoNotMaskOtherWindows(t *testing.T) {
	e := newEngine(t)
	now := time.Now()
	prev := bootstrapped()
	for round, input := range []struct {
		a, b, aWeek float64
		alerts      int
	}{
		{100, 20, 10, 1}, {100, 90, 10, 1}, {100, 95, 10, 1}, {100, 100, 10, 1},
		{100, 100, 95, 1}, {100, 100, 95, 0},
	} {
		s := okSnap("k", domain.ProviderGeminiCLI,
			scopedWin(domain.ScopeModel, "model-a", "day", input.a),
			scopedWin(domain.ScopeModel, "model-b", "day", input.b),
			scopedWin(domain.ScopeModel, "model-a", "week", input.aWeek))
		report, alerts, next := e.Evaluate(prev, []domain.QuotaSnapshot{s}, now.Add(time.Duration(round)*time.Minute))
		if len(alerts) != input.alerts {
			t.Fatalf("round %d alerts=%+v", round, alerts)
		}
		if next.Credentials["k"].State == domain.StateExhausted || len(report.Providers[0].BestWindows) != 0 || report.Recommendations[0].Direction != domain.DirectionSteady {
			t.Fatalf("partial scopes became whole credential/provider: %+v", report)
		}
		for _, a := range alerts {
			if a.Scope != domain.ScopeModel || a.ScopeID == "" || a.Advice == adviceExhaust {
				t.Fatalf("unscoped advice: %+v", a)
			}
		}
		if round == 0 && !strings.Contains(report.Recommendations[0].Reason, "仍有可用 scope") {
			t.Fatal(report.Recommendations[0].Reason)
		}
		if round == 3 && !strings.Contains(report.Recommendations[0].Reason, "已观测模型组均耗尽") {
			t.Fatal(report.Recommendations[0].Reason)
		}
		// Simulate restart between every observation; dedup is durable.
		b, err := json.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}
		prev = &state.State{}
		if err := json.Unmarshal(b, prev); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAccountAndUnknownScopes(t *testing.T) {
	e := newEngine(t)
	for _, scope := range []domain.QuotaScope{domain.ScopeAccount, domain.ScopeGroup, domain.ScopeUnknown, "", "future"} {
		s := okSnap("k", domain.ProviderClaude, scopedWin(scope, "opus", "five_hour", 100), scopedWin(scope, "opus", "seven_day", 10))
		report, alerts, next := e.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, time.Now())
		if len(alerts) != 1 {
			t.Fatalf("%s: %+v", scope, alerts)
		}
		if scope == domain.ScopeAccount {
			if next.Credentials["k"].State != domain.StateExhausted || alerts[0].Advice != adviceExhaust {
				t.Fatalf("account: %+v", alerts)
			}
		} else {
			if next.Credentials["k"].State == domain.StateExhausted || alerts[0].Advice == adviceExhaust {
				t.Fatalf("scope %s became account", scope)
			}
			if scope.Normalized() == domain.ScopeUnknown && (alerts[0].Kind != domain.AlertQuotaThreshold || alerts[0].Evidence == domain.EvidenceConfirmed) {
				t.Fatalf("unknown confirmed: %+v", alerts)
			}
		}
		if got := report.Providers[0].Snapshots[0].Windows[0]; got.UsedPercent == nil || *got.UsedPercent != 100 || got.Scope != scope.Normalized() {
			t.Fatalf("lost measurement: %+v", got)
		}
	}
}

func TestLegacyExhaustionHasUnknownScope(t *testing.T) {
	prev := bootstrapped()
	var old state.CredentialRecord
	if err := json.Unmarshal([]byte(`{"state":"exhausted","quota":"exhausted","last_used_percent":{"account/global":100}}`), &old); err != nil {
		t.Fatal(err)
	}
	prev.Credentials["k"] = old
	report, alerts, next := newEngine(t).Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "timeout")}, time.Now())
	w := report.Providers[0].Snapshots[0].Windows[0]
	if next.Credentials["k"].State == domain.StateExhausted || w.Scope != domain.ScopeUnknown || *w.UsedPercent != 100 || len(alerts) != 0 {
		t.Fatalf("legacy invented account scope: %+v %+v", next, w)
	}
}

func TestStalePreservesScopeAndIndependentResetHistory(t *testing.T) {
	e := newEngine(t)
	now := time.Now()
	s := okSnap("k", domain.ProviderGeminiCLI, scopedWin(domain.ScopeModel, "a", "day", 100), scopedWin(domain.ScopeModel, "b", "day", 40))
	_, _, prev := e.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, now)
	report, _, prev := e.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderGeminiCLI, "timeout")}, now.Add(time.Minute))
	windows := report.Providers[0].Snapshots[0].Windows
	if len(windows) != 2 || windows[0].ScopeID == windows[1].ScopeID {
		t.Fatalf("lost scope identity: %+v", windows)
	}
	s.Windows[0].UsedPercent = fp(10)
	s.Windows[1].UsedPercent = fp(5)
	_, alerts, _ := e.Evaluate(prev, []domain.QuotaSnapshot{s}, now.Add(2*time.Minute))
	if countKind(alerts, domain.AlertQuotaReset) != 1 {
		t.Fatalf("one model's threshold history leaked: %+v", alerts)
	}
	a, _ := findAlert(alerts, domain.AlertQuotaReset)
	if a.ScopeID != "a" {
		t.Fatalf("wrong reset: %+v", a)
	}
}

func TestIncompleteOrMixedScopeWindowsNeverDriveBroadAdvice(t *testing.T) {
	for _, extra := range []domain.QuotaWindow{
		{Name: "week", Scope: domain.ScopeAccount},
		scopedWin(domain.ScopeModel, "a", "week", 100),
		scopedWin(domain.ScopeUnknown, "", "week", 10),
	} {
		s := okSnap("k", domain.ProviderCodex, win("day", 10), extra)
		r, _, _ := newEngine(t).Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, time.Now())
		if len(r.Providers[0].BestWindows) != 0 || r.Recommendations[0].Direction != domain.DirectionSteady {
			t.Fatalf("unsafe recommendation: %+v", r)
		}
	}
}

func TestQuotaRejectionRequiresExplicitAccountScope(t *testing.T) {
	s := failSnapKind("k", domain.ProviderCodex, "quota exceeded", domain.FailureQuota)
	s.FailureScope = ""
	e := newEngine(t)
	prev := bootstrapped()
	for i := 0; i < 4; i++ {
		_, alerts, next := e.Evaluate(prev, []domain.QuotaSnapshot{s}, time.Now())
		if next.Credentials["k"].State == domain.StateExhausted || countKind(alerts, domain.AlertQuotaExhausted) > 0 {
			t.Fatal("unscoped rejection confirmed account exhaustion")
		}
		if i > 0 && len(alerts) > 0 {
			t.Fatalf("repeated unscoped alert: %+v", alerts)
		}
		prev = next
	}
}

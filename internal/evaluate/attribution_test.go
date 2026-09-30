package evaluate

import (
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// credential builds a named credential so a multi-credential report can be
// read back by label.
func credSnapOK(key, alias, short string, p domain.ProviderKind, windows ...domain.QuotaWindow) domain.QuotaSnapshot {
	s := okSnap(key, p, windows...)
	s.Credential.Alias, s.Credential.ShortID = alias, short
	return s
}

func credSnapFail(key, alias, short string, p domain.ProviderKind, kind domain.FailureKind) domain.QuotaSnapshot {
	s := failSnapKind(key, p, "upstream rejected", kind)
	s.Credential.Alias, s.Credential.ShortID = alias, short
	s.FailureScope = domain.ScopeUnknown
	return s
}

// TestExhaustedIsNotInvalidAndInvalidIsAttributed reproduces the production
// Codex report: two credentials at 100% on an account-scoped window plus one
// credential the collector rejected for auth.
//
// The three things this pins down:
//
//  1. 100% used projects to StateExhausted, never StateInvalid. Being out of
//     quota says nothing about whether the credential authenticates.
//  2. StateInvalid is reached only through a confirmed auth failure, and the
//     report says WHICH credential it came from. A provider-level "失效" that
//     does not name the credential reads as if all three were dead.
//  3. WorstState=Invalid with Healthy=0 is self-consistent: worst-state is the
//     maximum over credentials, and none of these three is healthy, so the two
//     numbers are not in conflict — but Normal/Limited/Abnormal is what the
//     reader gets, because "0 健康" alone hides that two of them have real,
//     precise numbers.
func TestExhaustedIsNotInvalidAndInvalidIsAttributed(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	snaps := []domain.QuotaSnapshot{
		credSnapOK("spent-a", "codex-vinsprite78", "cb31", domain.ProviderCodex, win("codex/rate_limit/primary_window", 100)),
		credSnapOK("spent-b", "codex-vintechg1", "e1ae", domain.ProviderCodex, win("codex/rate_limit/primary_window", 100)),
		credSnapFail("dead", "codex-design", "ad3d", domain.ProviderCodex, domain.FailureAuth),
	}
	report, _, next := eng.Evaluate(bootstrapped(), snaps, now)

	for _, key := range []string{"spent-a", "spent-b"} {
		if got := next.Credentials[key].State; got != domain.StateExhausted {
			t.Errorf("%s: 100%% account usage projected to %q, want exhausted", key, got)
		}
	}
	if got := next.Credentials["dead"].State; got != domain.StateInvalid {
		t.Errorf("auth-rejected credential projected to %q, want invalid", got)
	}

	pr := report.Providers[0]
	if pr.WorstState != domain.StateInvalid {
		t.Errorf("WorstState = %q, want invalid (the maximum over credentials)", pr.WorstState)
	}
	if pr.Healthy != 0 || pr.Total != 3 {
		t.Errorf("Healthy/Total = %d/%d, want 0/3", pr.Healthy, pr.Total)
	}
	if pr.Normal != 0 || pr.Limited != 0 || pr.Abnormal != 3 {
		t.Errorf("class counts = %d/%d/%d, want 0 normal, 0 limited, 3 abnormal", pr.Normal, pr.Limited, pr.Abnormal)
	}

	rec := report.Recommendations[0]
	if rec.Direction != domain.DirectionReauth {
		t.Fatalf("direction = %q, want reauth", rec.Direction)
	}
	if !strings.Contains(rec.Reason, "codex-design · ad3d") {
		t.Errorf("advice does not name the failing credential: %q", rec.Reason)
	}
	for _, other := range []string{"codex-vinsprite78", "codex-vintechg1"} {
		if strings.Contains(rec.Reason, other) {
			t.Errorf("advice blamed an exhausted-but-working credential %q: %q", other, rec.Reason)
		}
	}
	if !strings.Contains(rec.Reason, "认证被上游拒绝") {
		t.Errorf("advice does not state the evidence: %q", rec.Reason)
	}
}

// TestStaleAndTransportNeverProduceInvalid guards the other half of the
// attribution question: missing data is not a verdict on the credential, no
// matter how many cycles it persists.
func TestStaleAndTransportNeverProduceInvalid(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	prev := bootstrapped()
	for round := 0; round < 5; round++ {
		snaps := []domain.QuotaSnapshot{credSnapFail("k", "acct", "x1", domain.ProviderCodex, domain.FailureTransport)}
		report, _, next := eng.Evaluate(prev, snaps, now.Add(time.Duration(round)*time.Minute))
		prev = next
		if got := next.Credentials["k"].State; got == domain.StateInvalid {
			t.Fatalf("round %d: repeated transport failure produced invalid", round)
		}
		if report.Providers[0].WorstState == domain.StateInvalid {
			t.Fatalf("round %d: provider WorstState reached invalid without auth evidence", round)
		}
	}
}

// TestReauthAdviceIsProviderSpecific: the __Secure-session cookie is an Ollama
// implementation detail. Telling a Codex or Claude reader to update it is both
// wrong and unactionable, and it appeared in production because the two
// remediations were concatenated into one sentence.
func TestReauthAdviceIsProviderSpecific(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	cases := []struct {
		provider domain.ProviderKind
		want     string
		banned   string
	}{
		{domain.ProviderCodex, adviceOAuth, "__Secure-session"},
		{domain.ProviderClaude, adviceOAuth, "__Secure-session"},
		{domain.ProviderAntigravity, adviceOAuth, "__Secure-session"},
		{domain.ProviderGeminiCLI, adviceOAuth, "__Secure-session"},
		{domain.ProviderOllama, adviceOllama, "OAuth"},
	}
	for _, tc := range cases {
		t.Run(string(tc.provider), func(t *testing.T) {
			snaps := []domain.QuotaSnapshot{credSnapFail("k", "acct", "x1", tc.provider, domain.FailureAuth)}
			report, alerts, _ := eng.Evaluate(bootstrapped(), snaps, now)

			rec := report.Recommendations[0]
			if !strings.Contains(rec.Reason, tc.want) {
				t.Errorf("recommendation missing %q: %q", tc.want, rec.Reason)
			}
			if strings.Contains(rec.Reason, tc.banned) {
				t.Errorf("recommendation leaked unrelated channel advice %q: %q", tc.banned, rec.Reason)
			}
			a, ok := findAlert(alerts, domain.AlertCredential)
			if !ok {
				t.Fatalf("no credential alert: %v", kinds(alerts))
			}
			if a.Advice != tc.want {
				t.Errorf("alert advice = %q, want %q", a.Advice, tc.want)
			}
		})
	}
}

// TestScopedProviderIsNotCountedAsAllBroken is the counting regression: three
// Antigravity credentials with 0–35% group-scoped usage rendered as "0/3 个凭证"
// because a group scope never projects to healthy. They are limited, not
// abnormal, and the class counts have to say so.
func TestScopedProviderIsNotCountedAsAllBroken(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	group := func(key string, used float64) domain.QuotaSnapshot {
		return credSnapOK(key, key, "xx", domain.ProviderAntigravity, domain.QuotaWindow{
			Name: "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1",
			// The parser assigns this; the evaluator only carries it through.
			Label: "Gemini Models · 周", Scope: domain.ScopeGroup,
			ScopeID: "antigravity/groups/Gemini%20Models#1", UsedPercent: fp(used),
		})
	}
	report, _, _ := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{
		group("a", 2.4), group("b", 35), group("c", 0),
	}, now)

	pr := report.Providers[0]
	if pr.Total != 3 || pr.Limited != 3 {
		t.Errorf("class counts = normal %d / limited %d / abnormal %d of %d, want all 3 limited",
			pr.Normal, pr.Limited, pr.Abnormal, pr.Total)
	}
	if pr.Abnormal != 0 {
		t.Errorf("credentials with real headroom counted as abnormal: %d", pr.Abnormal)
	}
	if pr.Normal != 0 {
		t.Errorf("group-scoped evidence claimed as account-wide health: normal=%d", pr.Normal)
	}
	if pr.Healthy != 0 {
		t.Errorf("Healthy must stay 0 for scoped-only evidence, got %d", pr.Healthy)
	}
}

// TestRepeatedAdviceIsGroupedNotRepeated: three credentials reaching the same
// conclusion produced the same long sentence three times.
func TestRepeatedAdviceIsGroupedNotRepeated(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	group := func(key string) domain.QuotaSnapshot {
		return credSnapOK(key, key, "xx", domain.ProviderAntigravity, domain.QuotaWindow{
			Name:  "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1",
			Label: "Gemini Models · 周", Scope: domain.ScopeGroup,
			ScopeID: "antigravity/groups/Gemini%20Models#1", UsedPercent: fp(10),
		})
	}
	report, _, _ := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{
		group("a"), group("b"), group("c"),
	}, now)

	reason := report.Recommendations[0].Reason
	if n := strings.Count(reason, "仍有可用 scope"); n != 1 {
		t.Errorf("identical advice repeated %d times:\n%s", n, reason)
	}
	for _, label := range []string{"a · xx", "b · xx", "c · xx"} {
		if !strings.Contains(reason, label) {
			t.Errorf("grouped advice dropped credential %q:\n%s", label, reason)
		}
	}
}

// TestUserFacingTextCarriesNoInternalPaths: alert details, facts and advice all
// reach a chat window. The internal window/scope identifiers are dedup and
// persistence keys and must not appear there.
func TestUserFacingTextCarriesNoInternalPaths(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	snap := credSnapOK("k", "acct", "x1", domain.ProviderAntigravity, domain.QuotaWindow{
		Name:  "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1",
		Label: "Gemini Models · 周", Scope: domain.ScopeGroup,
		ScopeID: "antigravity/groups/Gemini%20Models#1", UsedPercent: fp(99),
	})
	report, alerts, _ := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{snap}, now)

	texts := []string{report.Recommendations[0].Reason}
	texts = append(texts, report.Notes...)
	for _, a := range alerts {
		texts = append(texts, a.Detail, a.Advice)
		texts = append(texts, a.Facts...)
	}
	for _, text := range texts {
		for _, banned := range []string{"antigravity/groups", "%20", "#1", "/buckets/"} {
			if strings.Contains(text, banned) {
				t.Errorf("internal identifier %q leaked into user-facing text: %q", banned, text)
			}
		}
	}
}

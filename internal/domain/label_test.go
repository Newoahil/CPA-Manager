package domain

import "testing"

// TestDisplayLabelPrefersParserLabel: the parser knows the upstream's own
// vocabulary, so its Label wins. Name stays untouched because it is the dedup
// and persistence key.
func TestDisplayLabelPrefersParserLabel(t *testing.T) {
	w := QuotaWindow{
		Name:  "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1",
		Label: "Gemini Models · 周",
	}
	if got := w.DisplayLabel(); got != "Gemini Models · 周" {
		t.Errorf("DisplayLabel() = %q, want the parser label", got)
	}
	if w.Name != "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1" {
		t.Error("DisplayLabel must not rewrite Name: it is the dedup key")
	}
}

// TestDisplayLabelDerivesReadableFallback covers windows written before Label
// existed, or by a parser path that does not set one. The derivation must
// decode, drop the "#n" occurrence suffix and drop structural path segments.
func TestDisplayLabelDerivesReadableFallback(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1", "Gemini Models · gemini-weekly · weekly"},
		{"claude/limits/weekly_scoped/fable#1", "fable"},
		{"codex/rate_limit/primary_window", "rate limit · primary window"},
		{"gemini-cli/models/gemini-2.5-pro/tokens/input#1", "gemini-2.5-pro · input"},
		{"5h", "5h"},
		{"", "未命名窗口"},
	}
	for _, tc := range cases {
		if got := (QuotaWindow{Name: tc.name}).DisplayLabel(); got != tc.want {
			t.Errorf("DisplayLabel(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestDerivedLabelNeverLeaksInternalSyntax: whatever the derivation produces,
// it must not still look like an internal path.
func TestDerivedLabelNeverLeaksInternalSyntax(t *testing.T) {
	for _, name := range []string{
		"antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1",
		"codex/additional/code%20review#2/primary_window",
		"claude/limits/weekly_scoped/fable#1",
	} {
		got := (QuotaWindow{Name: name}).DisplayLabel()
		for _, banned := range []string{"/", "%", "#"} {
			if contains(got, banned) {
				t.Errorf("DisplayLabel(%q) = %q still contains %q", name, got, banned)
			}
		}
	}
}

// TestScopeTextIsReadableAndKeepsUnknownUnknown: applicability must survive
// rendering, and an unknown scope must never round up to account.
func TestScopeTextIsReadableAndKeepsUnknownUnknown(t *testing.T) {
	cases := []struct {
		scope   QuotaScope
		scopeID string
		want    string
	}{
		{ScopeAccount, "", "账号"},
		{ScopeModel, "fable", "模型 fable"},
		{ScopeGroup, "antigravity/groups/Gemini%20Models#1", "分组 Gemini Models"},
		{ScopeUnknown, "", "范围未知"},
		{"", "", "范围未知"},
		{"nonsense", "", "范围未知"},
	}
	for _, tc := range cases {
		if got := ScopeText(tc.scope, tc.scopeID); got != tc.want {
			t.Errorf("ScopeText(%q,%q) = %q, want %q", tc.scope, tc.scopeID, got, tc.want)
		}
	}
	// ScopeLabel keeps the machine tuple: dedup and state keys depend on it.
	w := QuotaWindow{Scope: ScopeGroup, ScopeID: "antigravity/groups/Gemini%20Models#1"}
	if w.ScopeLabel() != "group:antigravity/groups/Gemini%20Models#1" {
		t.Errorf("ScopeLabel changed shape: %q", w.ScopeLabel())
	}
}

// TestClassOfNeverCollapsesLimitedIntoHealthyOrBroken pins the counting
// semantics: scoped-only evidence is its own bucket.
func TestClassOfNeverCollapsesLimitedIntoHealthyOrBroken(t *testing.T) {
	cases := map[CredentialState]StateClass{
		StateHealthy:   ClassNormal,
		StateLimited:   ClassLimited,
		StateNotice:    ClassLimited,
		StateWarning:   ClassLimited,
		StateExhausted: ClassAbnormal,
		StateInvalid:   ClassAbnormal,
		StateSuspect:   ClassAbnormal,
		StateStale:     ClassAbnormal,
		StateUnknown:   ClassAbnormal,
		"":             ClassAbnormal,
	}
	for st, want := range cases {
		if got := ClassOf(st); got != want {
			t.Errorf("ClassOf(%q) = %q, want %q", st, got, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

package quota

import (
	"strings"
	"testing"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// TestParsersAssignReadableLabels checks that every parser attaches a
// human-readable Label without touching Name, ScopeID or any number.
//
// Name and ScopeID are dedup and persistence keys; changing them would
// invalidate stored state and reset detection. Label is display-only.
func TestParsersAssignReadableLabels(t *testing.T) {
	cases := []struct {
		name      string
		provider  string
		profile   string
		body      string
		wantLabel map[string]string
	}{
		{
			name: "codex", provider: "codex",
			body: `{"rate_limit":{"primary_window":{"used_percent":100,"reset_at":1790000000},
			        "secondary_window":{"used_percent":12}}}`,
			wantLabel: map[string]string{
				"codex/rate_limit/primary_window":   "账号 · 主额度窗口",
				"codex/rate_limit/secondary_window": "账号 · 次额度窗口",
			},
		},
		{
			name: "codex code review", provider: "codex",
			body: `{"rate_limit":{"primary_window":{"used_percent":1}},
			        "code_review_rate_limit":{"primary_window":{"used_percent":2}}}`,
			wantLabel: map[string]string{
				"codex/code_review_rate_limit/primary_window": "代码评审 · 主额度窗口",
			},
		},
		{
			name: "claude", provider: "claude",
			body: `{"five_hour":{"utilization":17},"seven_day":{"utilization":5},
			        "seven_day_opus":{"utilization":3},
			        "limits":[{"kind":"weekly_scoped","percent":7,"scope":{"model":{"display_name":"fable"}}}]}`,
			wantLabel: map[string]string{
				"claude/five_hour":                    "账号 · 5小时",
				"claude/seven_day":                    "账号 · 7天",
				"claude/seven_day_opus":               "Opus 模型 · 7天",
				"claude/limits/weekly_scoped/fable#1": "fable 模型 · 周",
			},
		},
		{
			name: "antigravity", provider: "antigravity", profile: "current",
			body: `{"groups":[{"displayName":"Gemini Models","buckets":[
			        {"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.976},
			        {"bucketId":"fast","window":"daily","remainingFraction":0.5}]}]}`,
			wantLabel: map[string]string{
				"antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1": "Gemini Models · 周",
				"antigravity/groups/Gemini%20Models#1/buckets/fast/daily#1":           "Gemini Models · fast · 日",
			},
		},
		{
			name: "gemini-cli", provider: "gemini-cli",
			body: `{"buckets":[{"modelId":"gemini-2.5-pro","tokenType":"input","remainingFraction":0.8}]}`,
			wantLabel: map[string]string{
				"gemini-cli/models/gemini-2.5-pro/tokens/input#1": "gemini-2.5-pro 模型 · input",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := contextFor(tc.provider)
			c.Profile = tc.profile
			r := Parse(c, 200, []byte(tc.body), testNow)
			if r.Failure != domain.FailureNone {
				t.Fatalf("parse failed: %s (%s)", r.Failure, r.Err)
			}
			got := map[string]string{}
			for _, w := range r.Windows {
				got[w.Name] = w.Label
				if strings.TrimSpace(w.Label) == "" {
					t.Errorf("window %q has no label", w.Name)
				}
				// A label is display text only; it must never look like a path.
				for _, banned := range []string{"/", "%20", "#"} {
					if strings.Contains(w.Label, banned) {
						t.Errorf("label %q for %q still looks like an internal identifier", w.Label, w.Name)
					}
				}
			}
			for name, want := range tc.wantLabel {
				if got[name] != want {
					t.Errorf("label for %q = %q, want %q (all: %v)", name, got[name], want, got)
				}
			}
		})
	}
}

// TestCodexWindowPeriodFromReportedLength: the upstream states the rolling
// window length, so the period is read rather than guessed. primary/secondary
// is an ordering, not a duration — the same account can report a weekly or a
// monthly secondary window.
func TestCodexWindowPeriodFromReportedLength(t *testing.T) {
	cases := []struct {
		name    string
		seconds string
		want    string
	}{
		{"five hour", "18000", "账号 · 5小时窗口"},
		{"week", "604800", "账号 · 周窗口"},
		{"month low bound", "2419200", "账号 · 月窗口"},
		{"month high bound", "2678400", "账号 · 月窗口"},
		{"three days", "259200", "账号 · 3天窗口"},
		{"two hours", "7200", "账号 · 2小时窗口"},
		{"ninety minutes", "5400", "账号 · 90分钟窗口"},
		{"odd seconds", "45", "账号 · 45秒窗口"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":` + tc.seconds + `}}}`
			r := Parse(contextFor("codex"), 200, []byte(body), testNow)
			if r.Failure != domain.FailureNone {
				t.Fatalf("parse failed: %s", r.Err)
			}
			if got := r.Windows[0].Label; got != tc.want {
				t.Errorf("label = %q, want %q", got, tc.want)
			}
			if r.Windows[0].WindowSeconds == 0 {
				t.Error("window length not preserved on the window")
			}
		})
	}
}

// TestCodexWindowPeriodFallsBackWhenAbsent: with no reported length we must
// not infer a period from the field's position. The neutral structural wording
// stays.
func TestCodexWindowPeriodFallsBackWhenAbsent(t *testing.T) {
	body := `{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":20}}}`
	r := Parse(contextFor("codex"), 200, []byte(body), testNow)
	if r.Failure != domain.FailureNone {
		t.Fatalf("parse failed: %s", r.Err)
	}
	want := []string{"账号 · 主额度窗口", "账号 · 次额度窗口"}
	for i, w := range r.Windows {
		if w.Label != want[i] {
			t.Errorf("label[%d] = %q, want %q", i, w.Label, want[i])
		}
		if w.WindowSeconds != 0 {
			t.Errorf("invented a window length: %d", w.WindowSeconds)
		}
		for _, banned := range []string{"5小时", "周", "月"} {
			if strings.Contains(w.Label, banned) {
				t.Errorf("guessed a period %q with no reported length: %q", banned, w.Label)
			}
		}
	}
}

// TestDeclaredButMalformedWindowLengthFails keeps the existing rule: a declared
// numeric field that is not a valid number fails the whole result rather than
// being silently dropped.
func TestDeclaredButMalformedWindowLengthFails(t *testing.T) {
	for _, bad := range []string{`"18000"`, "0", "-1", "1.5", "true", "{}", "[]", "99999999999"} {
		body := `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":` + bad + `}}}`
		r := Parse(contextFor("codex"), 200, []byte(body), testNow)
		if r.Failure != domain.FailureParse || len(r.Windows) != 0 {
			t.Errorf("limit_window_seconds=%s accepted: %#v", bad, r)
		}
	}
	// null means "not offered" and is not a failure.
	r := Parse(contextFor("codex"), 200, []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":null}}}`), testNow)
	if r.Failure != domain.FailureNone || r.Windows[0].WindowSeconds != 0 {
		t.Errorf("null window length should mean not offered: %#v", r)
	}
}

// TestCodexLimitReachedIsAFlagNotAMeasurement: allowed=false / limit_reached
// are markers. They are recorded, but they never synthesise a percentage — the
// pinned frontend does exactly that, and it would fabricate evidence the
// response does not contain.
func TestCodexLimitReachedIsAFlagNotAMeasurement(t *testing.T) {
	body := `{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":42}}}`
	r := Parse(contextFor("codex"), 200, []byte(body), testNow)
	if r.Failure != domain.FailureNone {
		t.Fatalf("parse failed: %s", r.Err)
	}
	w := r.Windows[0]
	if !w.LimitReached {
		t.Error("explicit upstream limit marker was dropped")
	}
	if w.UsedPercent == nil || *w.UsedPercent != 42 {
		t.Errorf("marker overwrote the reported measurement: %v", w.UsedPercent)
	}

	// A marker with no number stays a failure: there is nothing to report.
	noNumber := Parse(contextFor("codex"), 200, []byte(`{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"reset_at":1790686800}}}`), testNow)
	if noNumber.Failure != domain.FailureParse {
		t.Errorf("a limit marker was promoted into a measurement: %#v", noNumber)
	}

	// Non-boolean flags are a schema change, not a value we may reinterpret.
	for _, bad := range []string{`"false"`, "0", "1", "[]"} {
		bodyBad := `{"rate_limit":{"allowed":` + bad + `,"primary_window":{"used_percent":1}}}`
		if got := Parse(contextFor("codex"), 200, []byte(bodyBad), testNow); got.Failure != domain.FailureParse {
			t.Errorf("allowed=%s accepted: %#v", bad, got)
		}
	}
}

// TestClaudeExtraUsage covers the present and absent cases. The amounts are the
// upstream's own credits: no currency appears anywhere in the payload, so none
// is invented.
func TestClaudeExtraUsage(t *testing.T) {
	withExtra := `{"five_hour":{"utilization":10},"extra_usage":{"is_enabled":true,"used_credits":12.5,"monthly_limit":100,"utilization":12.5}}`
	r := Parse(contextFor("claude"), 200, []byte(withExtra), testNow)
	if r.Failure != domain.FailureNone {
		t.Fatalf("parse failed: %s", r.Err)
	}
	if r.ExtraUsage == nil || !r.ExtraUsage.Enabled {
		t.Fatalf("extra usage dropped: %#v", r.ExtraUsage)
	}
	if r.ExtraUsage.UsedCredits == nil || *r.ExtraUsage.UsedCredits != 12.5 ||
		r.ExtraUsage.MonthlyLimit == nil || *r.ExtraUsage.MonthlyLimit != 100 ||
		r.ExtraUsage.UsedPercent == nil || *r.ExtraUsage.UsedPercent != 12.5 {
		t.Errorf("extra usage amounts wrong: %#v", r.ExtraUsage)
	}
	if !r.ExtraUsage.Reportable() {
		t.Error("a populated, enabled budget should be reportable")
	}

	// Absent: nothing at all, not a zeroed placeholder.
	without := Parse(contextFor("claude"), 200, []byte(`{"five_hour":{"utilization":10}}`), testNow)
	if without.Failure != domain.FailureNone || without.ExtraUsage != nil {
		t.Errorf("absent extra usage invented: %#v", without.ExtraUsage)
	}

	// Disabled: present but not reportable, so nothing is rendered.
	disabled := Parse(contextFor("claude"), 200, []byte(`{"five_hour":{"utilization":10},"extra_usage":{"is_enabled":false,"used_credits":0,"monthly_limit":0}}`), testNow)
	if disabled.ExtraUsage == nil || disabled.ExtraUsage.Reportable() {
		t.Errorf("disabled budget should not be reportable: %#v", disabled.ExtraUsage)
	}

	// Declared but malformed fails the whole result.
	for _, bad := range []string{`{"is_enabled":true,"used_credits":"10"}`, `{"is_enabled":"yes"}`, `{"is_enabled":true,"utilization":101}`, `[]`} {
		body := `{"five_hour":{"utilization":10},"extra_usage":` + bad + `}`
		if got := Parse(contextFor("claude"), 200, []byte(body), testNow); got.Failure != domain.FailureParse {
			t.Errorf("extra_usage=%s accepted: %#v", bad, got)
		}
	}
}

// TestPlanOnlyFromTheSameResponse: Codex states plan_type inline. Claude's plan
// lives behind a separate profile request and Antigravity's behind a separate
// subscription request, so neither may appear here — and no placeholder is
// substituted for them.
func TestPlanOnlyFromTheSameResponse(t *testing.T) {
	codex := Parse(contextFor("codex"), 200, []byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":1}}}`), testNow)
	if codex.Plan != "pro" {
		t.Errorf("codex plan = %q, want pro", codex.Plan)
	}
	noPlan := Parse(contextFor("codex"), 200, []byte(`{"rate_limit":{"primary_window":{"used_percent":1}}}`), testNow)
	if noPlan.Plan != "" {
		t.Errorf("invented a codex plan: %q", noPlan.Plan)
	}
	claude := Parse(contextFor("claude"), 200, []byte(`{"five_hour":{"utilization":1}}`), testNow)
	if claude.Plan != "" {
		t.Errorf("claude plan must not be guessed from the usage response: %q", claude.Plan)
	}
	c := contextFor("antigravity")
	c.Profile = "current"
	ag := Parse(c, 200, []byte(`{"groups":[{"displayName":"G","buckets":[{"window":"weekly","remainingFraction":1}]}]}`), testNow)
	if ag.Plan != "" {
		t.Errorf("antigravity tier must not be guessed from the summary response: %q", ag.Plan)
	}
}

// TestGeminiRemainingAmountCarriedVerbatim: the unit is the tokenType the label
// already names, so the count is passed through without conversion.
func TestGeminiRemainingAmountCarriedVerbatim(t *testing.T) {
	body := `{"buckets":[{"modelId":"gemini-2.5-pro","tokenType":"REQUESTS","remainingAmount":"100","remainingFraction":0.8}]}`
	r := Parse(contextFor("gemini-cli"), 200, []byte(body), testNow)
	if r.Failure != domain.FailureNone {
		t.Fatalf("parse failed: %s", r.Err)
	}
	if r.Windows[0].RemainingAmount != "100" {
		t.Errorf("remaining amount = %q, want the verbatim string", r.Windows[0].RemainingAmount)
	}
	if !strings.Contains(r.Windows[0].Label, "REQUESTS") {
		t.Errorf("label does not name the unit: %q", r.Windows[0].Label)
	}
	// Absent stays absent.
	none := Parse(contextFor("gemini-cli"), 200, []byte(`{"buckets":[{"modelId":"a","remainingFraction":1}]}`), testNow)
	if none.Windows[0].RemainingAmount != "" {
		t.Errorf("invented a remaining amount: %q", none.Windows[0].RemainingAmount)
	}
}

// TestAntigravityBucketDisplayNamePreferred: the bucket carries its own display
// name upstream; using it keeps the label in the vendor's own words.
func TestAntigravityBucketDisplayNamePreferred(t *testing.T) {
	c := contextFor("antigravity")
	c.Profile = "current"
	body := `{"groups":[{"displayName":"Gemini Models","buckets":[
	  {"bucketId":"gemini-weekly","displayName":"Gemini 3 Pro","window":"weekly","remainingFraction":0.976},
	  {"bucketId":"fast","window":"five_hour","remainingFraction":0.5}]}]}`
	r := Parse(c, 200, []byte(body), testNow)
	if r.Failure != domain.FailureNone {
		t.Fatalf("parse failed: %s", r.Err)
	}
	want := []string{"Gemini Models · Gemini 3 Pro · 周", "Gemini Models · fast · 5小时"}
	for i, w := range r.Windows {
		if w.Label != want[i] {
			t.Errorf("label[%d] = %q, want %q", i, w.Label, want[i])
		}
	}
}

// TestLabelsDoNotChangeIdentityOrNumbers: adding Label must not perturb the
// dedup key, the applicability or the measurement.
func TestLabelsDoNotChangeIdentityOrNumbers(t *testing.T) {
	c := contextFor("antigravity")
	c.Profile = "current"
	body := `{"groups":[{"displayName":"Gemini Models","buckets":[
	          {"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.976}]}]}`
	r := Parse(c, 200, []byte(body), testNow)
	if len(r.Windows) != 1 {
		t.Fatalf("windows = %d", len(r.Windows))
	}
	w := r.Windows[0]
	if w.Name != "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1" {
		t.Errorf("Name changed: %q", w.Name)
	}
	if w.Scope != domain.ScopeGroup || w.ScopeID != "antigravity/groups/Gemini%20Models#1" {
		t.Errorf("scope identity changed: %q / %q", w.Scope, w.ScopeID)
	}
	if w.UsedPercent == nil || *w.UsedPercent < 2.39 || *w.UsedPercent > 2.41 {
		t.Errorf("measurement changed: %v", w.UsedPercent)
	}
	if w.ScopeLabel() != "group:antigravity/groups/Gemini%20Models#1" {
		t.Errorf("machine scope tuple changed: %q", w.ScopeLabel())
	}
}

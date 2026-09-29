package quota

import (
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

var testNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func contextFor(provider string) Context {
	return Context{Provider: provider, AccountID: "account-fixture", ProjectID: "project-fixture"}
}

// All fixtures are synthesized from the pinned source contracts. They are
// deliberately not production captures or evidence of upstream availability.
func TestBuild(t *testing.T) {
	cases := []struct {
		provider, profile, method, target, ua string
	}{
		{"codex", "", "GET", "https://chatgpt.com/backend-api/wham/usage", "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"},
		{"claude", "current", "GET", "https://api.anthropic.com/api/oauth/usage", ""},
		{"gemini-cli", "current", "POST", "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", ""},
		{"antigravity", "current", "POST", "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"},
		{"antigravity", "legacy", "POST", "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels", "antigravity/1.11.5 windows/amd64"},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.profile, func(t *testing.T) {
			c := contextFor(tc.provider)
			c.Profile = tc.profile
			r, err := Build(c)
			if err != nil {
				t.Fatal(err)
			}
			if r.Method != tc.method || r.URL != tc.target || r.Header["User-Agent"] != tc.ua {
				t.Fatalf("unexpected request: %#v", r)
			}
			u, err := url.Parse(r.URL)
			if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				t.Fatal("unsafe target")
			}
			if r.Header["Authorization"] != "Bearer $TOKEN$" || r.Header["Content-Type"] != "application/json" {
				t.Fatal("missing placeholder headers")
			}
			if strings.Contains(r.URL+r.Data+r.Method, "$TOKEN$") {
				t.Fatal("placeholder outside header")
			}
			for k, v := range r.Header {
				if k != "Authorization" && strings.Contains(v, "$TOKEN$") {
					t.Fatal("unexpected placeholder")
				}
			}
			if tc.provider == "codex" && r.Header["Chatgpt-Account-Id"] != c.AccountID {
				t.Fatal("missing account scope")
			}
			if tc.provider == "claude" && r.Header["anthropic-beta"] != "oauth-2025-04-20" {
				t.Fatal("missing beta header")
			}
			if tc.method == "POST" {
				var data map[string]string
				if json.Unmarshal([]byte(r.Data), &data) != nil || !reflect.DeepEqual(data, map[string]string{"project": c.ProjectID}) {
					t.Fatal("invalid body")
				}
			} else if r.Data != "" {
				t.Fatal("GET must have no body")
			}
			r.Header["Authorization"] = "changed"
			again, _ := Build(c)
			if again.Header["Authorization"] != "Bearer $TOKEN$" {
				t.Fatal("shared mutable header map")
			}
		})
	}
	a, _ := Build(contextFor("antigravity"))
	c := contextFor("antigravity")
	c.Profile = "current"
	b, _ := Build(c)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("default profile is not current")
	}
}

func TestBuildErrorsAndContextEscaping(t *testing.T) {
	cases := []struct {
		c    Context
		want error
	}{
		{Context{Provider: "unknown"}, ErrUnsupported},
		{Context{Provider: "https://attacker.invalid"}, ErrUnsupported},
		{Context{Provider: "codex", Profile: "legacy", AccountID: "secret-context"}, ErrUnsupported},
		{Context{Provider: "claude", Profile: "future"}, ErrUnsupported},
		{Context{Provider: "codex"}, ErrMissingContext},
		{Context{Provider: "gemini-cli"}, ErrMissingContext},
		{Context{Provider: "antigravity", Profile: "legacy"}, ErrMissingContext},
		{Context{Provider: "antigravity"}, ErrMissingContext},
		{Context{Provider: "codex", AccountID: " \t"}, ErrMissingContext},
		{Context{Provider: "codex", AccountID: "secret-context\r\nInjected: x"}, ErrMissingContext},
		{Context{Provider: "codex", AccountID: "$TOKEN$"}, ErrMissingContext},
		{Context{Provider: "gemini-cli", ProjectID: "$TOKEN$"}, ErrMissingContext},
	}
	for i, tc := range cases {
		r, err := Build(tc.c)
		if !errors.Is(err, tc.want) || !reflect.DeepEqual(r, Request{}) {
			t.Fatalf("case %d: wrong error/request", i)
		}
		if strings.Contains(err.Error(), "secret-context") {
			t.Fatal("context leaked")
		}
	}
	c := contextFor("gemini-cli")
	c.ProjectID = `project"quoted\\value`
	r, err := Build(c)
	var body map[string]string
	if err != nil || json.Unmarshal([]byte(r.Data), &body) != nil || body["project"] != c.ProjectID {
		t.Fatal("project not safely JSON encoded")
	}
}

func TestParseSourceShapedFixtures(t *testing.T) {
	cases := []struct {
		name, provider, profile, body string
		used                          []float64
	}{
		{"codex", "codex", "", `{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":0.95,"limit_window_seconds":18000,"reset_at":1790686800},"secondary_window":{"used_percent":100,"reset_after_seconds":3600}},"code_review_rate_limit":{"primary_window":{"used_percent":0}},"additional_rate_limits":[{"limit_name":"same","rate_limit":{"primary_window":{"used_percent":20}}},{"limit_name":"same","rate_limit":{"primary_window":{"used_percent":30}}}]}`, []float64{0.95, 100, 0, 20, 30}},
		{"claude", "claude", "", `{"five_hour":{"utilization":0.95,"resets_at":"2026-09-29T14:00:00+02:00"},"seven_day":{"utilization":100,"resets_at":null},"seven_day_opus":null,"extra_usage":{"is_enabled":true,"used_credits":10,"monthly_limit":100}}`, []float64{0.95, 100}},
		{"claude-fable", "claude", "", `{"five_hour":{"utilization":0},"limits":[{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable 5"}},"percent":0.95,"is_active":true,"resets_at":"2026-10-01T00:00:00Z"},{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable 5"}},"percent":20}]}`, []float64{0, 0.95, 20}},
		{"gemini", "gemini-cli", "", `{"buckets":[{"modelId":"gemini-2.5-pro","tokenType":"REQUESTS","remainingAmount":"100","remainingFraction":1,"resetTime":"2026-10-01T00:00:00Z"},{"modelId":"gemini-2.5-pro","tokenType":"REQUESTS","remainingFraction":0},{"modelId":"gemini-2.5-flash","remainingFraction":0.95}]}`, []float64{0, 100, 5}},
		{"antigravity-current", "antigravity", "current", `{"groups":[{"displayName":"Shared","buckets":[{"bucketId":"same","window":"weekly","remainingFraction":0,"resetTime":"2026-10-01T00:00:00Z"},{"bucketId":"same","window":"weekly","remainingFraction":1}]},{"displayName":"Shared","buckets":[{"bucketId":"same","window":"weekly","remainingFraction":0.95}]}]}`, []float64{100, 0, 5}},
		{"antigravity-legacy", "antigravity", "legacy", `{"models":{"z-model":{"displayName":"Duplicate","quotaInfo":{"remainingFraction":1}},"a-model":{"displayName":"Duplicate","quotaInfo":{"remainingFraction":0}},"catalog-only":{"displayName":"No quota capability"}}}`, []float64{100, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := contextFor(tc.provider)
			c.Profile = tc.profile
			r := Parse(c, 200, []byte(tc.body), testNow)
			if r.Failure != domain.FailureNone || r.Err != "" || r.Confidence != domain.ConfidenceReported || len(r.Windows) != len(tc.used) {
				t.Fatalf("unexpected result: %#v", r)
			}
			names := map[string]bool{}
			for i, w := range r.Windows {
				if w.UsedPercent == nil || math.Abs(*w.UsedPercent-tc.used[i]) > 1e-9 {
					t.Fatalf("window %d wrong percentage", i)
				}
				if w.Name == "" || names[w.Name] {
					t.Fatalf("duplicate/empty name: %q", w.Name)
				}
				names[w.Name] = true
			}
			if !reflect.DeepEqual(r, Parse(c, 200, []byte(tc.body), testNow)) {
				t.Fatal("nondeterministic parse")
			}
			if tc.provider == "codex" && r.Plan != "plus" {
				t.Fatal("plan missing")
			}
			if r.Balance != "" {
				t.Fatal("invented balance")
			}
		})
	}
}

func TestInvalidNumbersFailWholeResult(t *testing.T) {
	cases := []struct{ provider, profile, template string }{
		{"codex", "", `{"rate_limit":{"primary_window":{"used_percent":5},"secondary_window":{"used_percent":VALUE}}}`},
		{"claude", "", `{"five_hour":{"utilization":5},"seven_day":{"utilization":VALUE}}`},
		{"claude", "", `{"five_hour":{"utilization":5},"limits":[{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}},"percent":VALUE}]}`},
		{"gemini-cli", "", `{"buckets":[{"modelId":"a","remainingFraction":1},{"modelId":"b","remainingFraction":VALUE}]}`},
		{"antigravity", "", `{"groups":[{"buckets":[{"remainingFraction":1},{"remainingFraction":VALUE}]}]}`},
		{"antigravity", "legacy", `{"models":{"a":{"quotaInfo":{"remainingFraction":1}},"b":{"quotaInfo":{"remainingFraction":VALUE}}}}`},
	}
	for _, tc := range cases {
		bad := []string{"null", "true", "false", `"0.95"`, `"95%"`, "-0.01", "101", "1e999", "{}", "[]"}
		if strings.Contains(tc.template, "remainingFraction") {
			bad = append(bad, "1.01", "75")
		}
		for _, value := range bad {
			t.Run(tc.provider+tc.profile+"/"+value, func(t *testing.T) {
				c := contextFor(tc.provider)
				c.Profile = tc.profile
				r := Parse(c, 200, []byte(strings.ReplaceAll(tc.template, "VALUE", value)), testNow)
				assertParseFailure(t, r)
			})
		}
	}
}

func assertParseFailure(t *testing.T, r Result) {
	t.Helper()
	if r.Failure != domain.FailureParse || r.Confidence != domain.ConfidenceUnknown || len(r.Windows) != 0 || r.Plan != "" || r.Balance != "" || r.Err == "" {
		t.Fatalf("not conservative failure: %#v", r)
	}
}

func TestInsufficientOrWrongSchema(t *testing.T) {
	cases := []struct{ provider, profile, body string }{
		{"codex", "", `{"plan_type":"pro"}`},
		{"codex", "", `{"rate_limit":{"primary_window":{"reset_at":1790686800},"allowed":false,"limit_reached":true}}`},
		{"codex", "", `{"rate_limit":{"primary_window":null}}`},
		{"codex", "", `{"rate_limit":{"primary_window":{"used_percent":1}},"additional_rate_limits":[{}]}`},
		{"claude", "", `{"five_hour":{"resets_at":"2026-10-01T00:00:00Z"}}`},
		{"claude", "", `{"five_hour":null,"seven_day":null}`},
		{"claude", "", `{"five_hour":{"utilization":1},"seven_day":false}`},
		{"gemini-cli", "", `{"buckets":[{"modelId":"a","remainingAmount":"200"}]}`},
		{"gemini-cli", "", `{"buckets":[{"modelId":"a","resetTime":"2026-10-01T00:00:00Z"}]}`},
		{"gemini-cli", "", `{"buckets":[{"remainingFraction":1}]}`},
		{"gemini-cli", "", `{"buckets":[]}`},
		{"antigravity", "", `{"models":{"a":{"quotaInfo":{"remainingFraction":1}}}}`},
		{"antigravity", "legacy", `{"groups":[{"buckets":[{"remainingFraction":1}]}]}`},
		{"antigravity", "", `{"groups":[{"buckets":[{"remainingFraction":1}]},{"buckets":[]}]}`},
		{"antigravity", "legacy", `{"models":{"a":{"quotaInfo":null}}}`},
		{"antigravity", "legacy", `{"models":{"a":{"displayName":"catalogue"}}}`},
		{"claude", "", `{"five_hour":{"utilization":0,"utilization":100}}`},
		{"claude", "", `{"five_hour":{"utilization":1},"error":{"message":"quota secret-body"}}`},
	}
	for _, tc := range cases {
		c := contextFor(tc.provider)
		c.Profile = tc.profile
		assertParseFailure(t, Parse(c, 200, []byte(tc.body), testNow))
	}
	for _, body := range []string{"", "null", "[]", "{}", "true", "<html>secret-body</html>", `{} {}`, `{"five_hour":`} {
		r := Parse(contextFor("claude"), 200, []byte(body), testNow)
		assertParseFailure(t, r)
		if strings.Contains(r.Err, "secret-body") {
			t.Fatal("body leaked")
		}
	}
	assertParseFailure(t, Parse(Context{Provider: "codex"}, 200, []byte(`{"rate_limit":{"primary_window":{"used_percent":1}}}`), testNow))
}

func TestHTTPFailures(t *testing.T) {
	cases := []struct {
		status  int
		kind    domain.FailureKind
		message string
	}{
		{401, domain.FailureAuth, "upstream credential rejected"},
		{403, domain.FailureTransport, "upstream access forbidden"},
		{429, domain.FailureTransport, "upstream rate limited"},
		{0, domain.FailureTransport, "upstream request failed or timed out"},
		{-1, domain.FailureTransport, "upstream request failed or timed out"},
		{408, domain.FailureTransport, "upstream request failed or timed out"},
		{500, domain.FailureTransport, "upstream request failed or timed out"},
		{502, domain.FailureTransport, "upstream request failed or timed out"},
		{504, domain.FailureTransport, "upstream request failed or timed out"},
		{404, domain.FailureTransport, "upstream request unsuccessful"},
		{302, domain.FailureTransport, "upstream request unsuccessful"},
		{400, domain.FailureTransport, "upstream request unsuccessful"},
	}
	for _, provider := range []string{"codex", "claude", "gemini-cli", "antigravity"} {
		for _, tc := range cases {
			r := Parse(contextFor(provider), tc.status, []byte(`{"error":{"code":"quota_exceeded","message":"secret-body token expired"}}`), testNow)
			if r.Failure != tc.kind || r.Err != tc.message || r.Confidence != domain.ConfidenceUnknown || len(r.Windows) != 0 {
				t.Fatalf("%s %d: %#v", provider, tc.status, r)
			}
		}
	}
	for _, c := range []Context{{Provider: "unsupported"}, {Provider: "claude", Profile: "legacy"}} {
		if r := Parse(c, 200, []byte(`{}`), testNow); r.Failure != domain.FailureUnsupported {
			t.Fatalf("wrong unsupported result: %#v", r)
		}
	}
}

func TestResets(t *testing.T) {
	c := contextFor("codex")
	for _, seconds := range []string{"0", "3600"} {
		body := `{"rate_limit":{"primary_window":{"used_percent":0,"reset_after_seconds":` + seconds + `}}}`
		r := Parse(c, 200, []byte(body), testNow)
		if r.Failure != domain.FailureNone {
			t.Fatal(r.Err)
		}
		w := r.Windows[0]
		want := testNow
		if seconds == "3600" {
			want = want.Add(time.Hour)
		}
		if w.ResetAt == nil || !w.ResetAt.Equal(want) || w.ResetText != "derived from reset_after_seconds="+seconds {
			t.Fatalf("relative reset lost: %#v", w)
		}
	}
	body := `{"rate_limit":{"primary_window":{"used_percent":0,"reset_at":0,"reset_after_seconds":3600}}}`
	r := Parse(c, 200, []byte(body), testNow)
	if r.Failure != domain.FailureNone || r.Windows[0].ResetAt == nil || !r.Windows[0].ResetAt.Equal(time.Unix(0, 0)) || r.Windows[0].ResetText != "" {
		t.Fatal("absolute reset precedence/unit incorrect")
	}
	for _, raw := range []string{"-1", "1.5", "9223372036854775807", `"3600"`, "true"} {
		assertParseFailure(t, Parse(c, 200, []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"reset_after_seconds":`+raw+`}}}`), testNow))
	}
	assertParseFailure(t, Parse(c, 200, []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"reset_at":1790686800000}}}`), testNow))
	assertParseFailure(t, Parse(c, 200, []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"reset_after_seconds":1}}}`), time.Time{}))
	for _, raw := range []string{`"tomorrow"`, "123", "true", `"2026-09-29"`} {
		assertParseFailure(t, Parse(contextFor("claude"), 200, []byte(`{"five_hour":{"utilization":1,"resets_at":`+raw+`}}`), testNow))
	}
	r = Parse(contextFor("claude"), 200, []byte(`{"five_hour":{"utilization":1,"resets_at":"2026-09-29T14:00:00+02:00"}}`), testNow)
	if r.Failure != domain.FailureNone || r.Windows[0].ResetAt == nil || !r.Windows[0].ResetAt.Equal(testNow) {
		t.Fatal("RFC3339 reset incorrect")
	}
}

func TestWindowNamesSurviveUniqueRowReordering(t *testing.T) {
	cases := []struct{ provider, first, second string }{
		{"gemini-cli", `{"buckets":[{"modelId":"a/b","remainingFraction":0.1},{"modelId":"c","remainingFraction":0.2}]}`, `{"buckets":[{"modelId":"c","remainingFraction":0.2},{"modelId":"a/b","remainingFraction":0.1}]}`},
		{"antigravity", `{"groups":[{"displayName":"a","buckets":[{"bucketId":"x","remainingFraction":0.1}]},{"displayName":"b","buckets":[{"bucketId":"x","remainingFraction":0.2}]}]}`, `{"groups":[{"displayName":"b","buckets":[{"bucketId":"x","remainingFraction":0.2}]},{"displayName":"a","buckets":[{"bucketId":"x","remainingFraction":0.1}]}]}`},
	}
	for _, tc := range cases {
		byName := func(body string) map[string]float64 {
			r := Parse(contextFor(tc.provider), 200, []byte(body), testNow)
			if r.Failure != domain.FailureNone {
				t.Fatal(r.Err)
			}
			m := map[string]float64{}
			for _, w := range r.Windows {
				m[w.Name] = *w.UsedPercent
			}
			return m
		}
		if !reflect.DeepEqual(byName(tc.first), byName(tc.second)) {
			t.Fatal("identities changed on reorder")
		}
	}
}

func FuzzParseNeverInventsInvalidPercent(f *testing.F) {
	f.Add([]byte(`{"buckets":[{"modelId":"a","remainingFraction":0.95}]}`))
	f.Add([]byte(`{"five_hour":{"utilization":0.95}}`))
	f.Add([]byte(`{"groups":[{"buckets":[{}]}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, provider := range []string{"codex", "claude", "gemini-cli", "antigravity"} {
			r := Parse(contextFor(provider), 200, body, testNow)
			if r.Failure != domain.FailureNone {
				if len(r.Windows) != 0 || r.Confidence != domain.ConfidenceUnknown {
					t.Fatal("partial success on failure")
				}
				continue
			}
			if len(r.Windows) == 0 {
				t.Fatal("success without windows")
			}
			for _, w := range r.Windows {
				if w.UsedPercent == nil || math.IsNaN(*w.UsedPercent) || *w.UsedPercent < 0 || *w.UsedPercent > 100 {
					t.Fatal("invalid percentage")
				}
			}
		}
	})
}

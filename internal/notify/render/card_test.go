package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// walkElements visits every element in a card body, descending into panels and
// column_sets. It is how the structural tests reach nested components.
func walkElementsFor(card map[string]any) []map[string]any {
	body, _ := card["body"].(map[string]any)
	var out []map[string]any
	var visit func(els []any)
	visit = func(els []any) {
		for _, e := range els {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, m)
			if sub, ok := m["elements"].([]any); ok {
				visit(sub)
			}
			if cols, ok := m["columns"].([]any); ok {
				for _, c := range cols {
					if cm, ok := c.(map[string]any); ok {
						if se, ok := cm["elements"].([]any); ok {
							visit(se)
						}
					}
				}
			}
		}
	}
	if body != nil {
		visit(body["elements"].([]any))
	}
	return out
}

func findElements(card map[string]any, tag string) []map[string]any {
	var out []map[string]any
	for _, e := range walkElementsFor(card) {
		if e["tag"] == tag {
			out = append(out, e)
		}
	}
	return out
}

func window(used *float64, scope domain.QuotaScope) domain.QuotaWindow {
	return domain.QuotaWindow{Name: "w", Label: "账号 · 周窗口", Scope: scope, UsedPercent: used}
}

func pctp(v float64) *float64 { return &v }

// TestRemainingCaliberConversion pins 剩余 = 100 - 已用, including the clamps,
// so a change to the display caliber cannot drift from the CPA console.
func TestRemainingCaliberConversion(t *testing.T) {
	cases := []struct {
		used float64
		want string
	}{
		{0, "100.0%"},
		{17, "83.0%"},
		{35, "65.0%"},
		{41, "59.0%"},
		{100, "0.0%"},
		{120, "0.0%"}, // over-100 is clamped, never rendered negative
	}
	for _, tc := range cases {
		if got := remainingPct(tc.used); got != tc.want {
			t.Errorf("remainingPct(%.1f) = %q, want %q", tc.used, got, tc.want)
		}
	}
	// A missing reading is never 0% or 100%.
	if got := remainingPctOrUnknown(nil); got != "未上报" {
		t.Errorf("nil remaining = %q, want 未上报", got)
	}
	// Thresholds stay on the used scale: a 95% used credential is a warning,
	// and its card text shows 5% remaining.
	warn := 95.0
	rep := oneProviderReport(domain.StateWarning, window(&warn, domain.ScopeAccount))
	card := New("").WithLocation(time.UTC).Card(domain.Message{Report: rep})
	if !containsJSON(t, card, "最紧剩余 5.0%") {
		t.Error("used 95% did not render as 5.0% remaining")
	}
	if !containsJSON(t, card, "额度告警") {
		t.Error("used-based warning state lost")
	}
}

// TestChartValuesAreFractionsAndMatchText: every chart value is a 0–1 decimal
// and its text is that same fraction as a percentage, so bar length and label
// can never disagree.
func TestChartValuesAreFractionsAndMatchText(t *testing.T) {
	u1, u2 := 0.0, 17.0
	rep := &domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: 2,
		States: map[string]domain.CredentialState{"a": domain.StateHealthy, "b": domain.StateHealthy},
		Snapshots: []domain.QuotaSnapshot{
			{Credential: domain.Credential{Key: "a", Alias: "a"}, OK: true, Windows: []domain.QuotaWindow{window(&u1, domain.ScopeAccount)}},
			{Credential: domain.Credential{Key: "b", Alias: "b"}, OK: true, Windows: []domain.QuotaWindow{window(&u2, domain.ScopeAccount)}},
		},
	}}}
	card := New("").WithLocation(time.UTC).Card(domain.Message{Report: rep})

	charts := findElements(card, "chart")
	if len(charts) != 1 {
		t.Fatalf("charts = %d, want 1", len(charts))
	}
	spec, _ := charts[0]["chart_spec"].(map[string]any)
	if spec["type"] != "linearProgress" || spec["direction"] != "horizontal" {
		t.Fatalf("unexpected chart_spec: %#v", spec)
	}
	data, _ := spec["data"].(map[string]any)
	values, _ := data["values"].([]any)
	if len(values) != 2 {
		t.Fatalf("values = %d, want 2 (one bar per credential)", len(values))
	}
	for _, v := range values {
		vm, _ := v.(map[string]any)
		f, ok := vm["value"].(float64)
		if !ok {
			t.Fatalf("value not a number: %#v", vm["value"])
		}
		if f < 0 || f > 1 {
			t.Errorf("chart value %v outside 0–1", f)
		}
		wantText := fmt.Sprintf("%.0f%%", f*100)
		if vm["text"] != wantText {
			t.Errorf("chart text %v != value-derived %q", vm["text"], wantText)
		}
	}
	// 0% used -> 100% remaining -> value 1; 17% used -> 83% -> 0.83.
	if values[0].(map[string]any)["value"].(float64) != 1.0 {
		t.Errorf("0%% used should be a full remaining bar")
	}
}

// TestStateTagColorMapping pins the colour contract. Text and colour always
// appear together.
func TestStateTagColorMapping(t *testing.T) {
	cases := map[domain.CredentialState]string{
		domain.StateHealthy:   "green",
		domain.StateNotice:    "orange",
		domain.StateWarning:   "orange",
		domain.StateExhausted: "red",
		domain.StateInvalid:   "red",
		domain.StateSuspect:   "orange",
		domain.StateStale:     "neutral",
		domain.StateUnknown:   "neutral",
		domain.StateLimited:   "blue",
	}
	for st, want := range cases {
		if got := stateTagColor(st); got != want {
			t.Errorf("stateTagColor(%s) = %s, want %s", st, got, want)
		}
	}
	// The inline tag carries both the colour and the text.
	tag := inlineTag("red", "已用满")
	if tag != "<text_tag color='red'>已用满</text_tag>" {
		t.Errorf("inlineTag shape wrong: %q", tag)
	}
}

// TestHeaderTagsCappedAtThree: Feishu keeps only the first three header tags,
// and all of ours must name abnormal channels.
func TestHeaderTagsCappedAtThree(t *testing.T) {
	bad := 100.0
	rep := &domain.Report{Providers: []domain.ProviderReport{
		{Provider: domain.ProviderCodex, Total: 1, States: map[string]domain.CredentialState{"a": domain.StateExhausted}, Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "a", Alias: "a"}, OK: true, Windows: []domain.QuotaWindow{window(&bad, domain.ScopeAccount)}}}},
		{Provider: domain.ProviderClaude, Total: 1, States: map[string]domain.CredentialState{"b": domain.StateInvalid}, Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "b", Alias: "b"}}}},
		{Provider: domain.ProviderAntigravity, Total: 1, States: map[string]domain.CredentialState{"c": domain.StateExhausted}, Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "c", Alias: "c"}, OK: true, Windows: []domain.QuotaWindow{window(&bad, domain.ScopeAccount)}}}},
		{Provider: domain.ProviderGeminiCLI, Total: 1, States: map[string]domain.CredentialState{"d": domain.StateInvalid}, Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "d", Alias: "d"}}}},
	}}
	card := New("").WithLocation(time.UTC).Card(domain.Message{Report: rep})
	header, _ := card["header"].(map[string]any)
	tags, _ := header["text_tag_list"].([]any)
	if len(tags) != 3 {
		t.Fatalf("header tags = %d, want at most 3", len(tags))
	}
	// A healthy-only report carries no header tags.
	good := 10.0
	healthy := oneProviderReport(domain.StateHealthy, window(&good, domain.ScopeAccount))
	hc := New("").WithLocation(time.UTC).Card(domain.Message{Report: healthy})
	if _, ok := hc["header"].(map[string]any)["text_tag_list"]; ok {
		t.Error("healthy report should have no header tags")
	}
}

// TestCollapsiblePanelOnlyForAbnormalChannels: a healthy or limited channel
// keeps its overview line and no panel; an exhausted/invalid one gets a panel.
func TestCollapsiblePanelOnlyForAbnormalChannels(t *testing.T) {
	low := 5.0
	full := 100.0

	healthy := oneProviderReport(domain.StateHealthy, window(&low, domain.ScopeAccount))
	if n := len(findElements(New("").Card(domain.Message{Report: healthy}), "collapsible_panel")); n != 0 {
		t.Errorf("healthy channel produced %d panels, want 0", n)
	}

	limited := oneProviderReport(domain.StateLimited, window(&low, domain.ScopeAccount))
	if n := len(findElements(New("").Card(domain.Message{Report: limited}), "collapsible_panel")); n != 0 {
		t.Errorf("limited channel produced %d panels, want 0", n)
	}

	exhausted := oneProviderReport(domain.StateExhausted, window(&full, domain.ScopeAccount))
	if n := len(findElements(New("").Card(domain.Message{Report: exhausted}), "collapsible_panel")); n != 1 {
		t.Errorf("exhausted channel produced %d panels, want 1", n)
	}

	invalid := &domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: 1,
		States:    map[string]domain.CredentialState{"k": domain.StateInvalid},
		Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "k", Alias: "k"}, Failure: domain.FailureAuth}},
	}}}
	if n := len(findElements(New("").Card(domain.Message{Report: invalid}), "collapsible_panel")); n != 1 {
		t.Errorf("invalid channel produced %d panels, want 1", n)
	}
}

// TestButtonsAppearOnlyWhenActionable: the refresh callback is always present;
// the CPA link only when a credential is broken AND a URL is configured.
func TestButtonsAppearOnlyWhenActionable(t *testing.T) {
	full := 100.0
	healthy := oneProviderReport(domain.StateHealthy, window(pctp(50), domain.ScopeAccount))
	broken := oneProviderReport(domain.StateInvalid)

	// Healthy, no URL: exactly one callback button.
	card := New("").Card(domain.Message{Report: healthy})
	if got := buttonCount(card); got != 1 {
		t.Errorf("healthy card buttons = %d, want 1", got)
	}

	// Healthy with a URL: still one button (no breakage -> no jump).
	t.Setenv("NOTIFY_CPA_PAGE_URL", "https://cpa.example.com")
	card = New("").Card(domain.Message{Report: healthy})
	if got := buttonCount(card); got != 1 {
		t.Errorf("healthy card with URL buttons = %d, want 1", got)
	}

	// Broken without a URL: still one button.
	t.Setenv("NOTIFY_CPA_PAGE_URL", "")
	card = New("").Card(domain.Message{Report: broken})
	if got := buttonCount(card); got != 1 {
		t.Errorf("broken card without URL buttons = %d, want 1", got)
	}
	if !containsJSON(t, card, RefreshAction) {
		t.Error("refresh callback missing")
	}

	// Broken with a URL: refresh + open_url.
	t.Setenv("NOTIFY_CPA_PAGE_URL", "https://cpa.example.com")
	card = New("").Card(domain.Message{Report: broken})
	if got := buttonCount(card); got != 2 {
		t.Fatalf("broken card with URL buttons = %d, want 2", got)
	}
	if !containsJSON(t, card, "https://cpa.example.com") {
		t.Error("CPA URL not rendered")
	}
	if n := countBehaviors(card, "open_url"); n != 1 {
		t.Errorf("open_url behaviors = %d, want 1", n)
	}
	// No button ever mutates state: the only callback is the read-only refresh.
	if n := countBehaviors(card, "callback"); n != 1 {
		t.Errorf("callback behaviors = %d, want 1", n)
	}
	_ = full
}

// TestComponentCountBounded: a report with many credentials must stay under
// Feishu's 200-component ceiling.
func TestComponentCountBounded(t *testing.T) {
	var snaps []domain.QuotaSnapshot
	states := map[string]domain.CredentialState{}
	for i := 0; i < 120; i++ {
		key := fmt.Sprintf("k%d", i)
		u := float64(i % 101)
		windows := []domain.QuotaWindow{
			window(&u, domain.ScopeAccount),
			window(&u, domain.ScopeModel),
			window(&u, domain.ScopeGroup),
		}
		snaps = append(snaps, domain.QuotaSnapshot{
			Credential: domain.Credential{Key: key, Alias: key}, OK: true, Windows: windows,
		})
		states[key] = domain.StateExhausted
	}
	rep := &domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: len(snaps), States: states, Snapshots: snaps,
	}}}
	card := New("").WithLocation(time.UTC).Card(domain.Message{Report: rep, Detailed: true})
	got := countCardElements(card)
	if got > maxCardElements {
		t.Errorf("card has %d elements, exceeding the %d ceiling", got, maxCardElements)
	}
}

// countCardElements counts every component in a card body, including nested
// panel elements, column elements and table rows.
func countCardElements(card map[string]any) int {
	n := 0
	for _, e := range walkElementsFor(card) {
		n++
		if rows, ok := e["rows"].([]any); ok {
			n += len(rows)
		}
	}
	return n
}

// TestDegradedCardHasNoRiskyComponents: the fallback path uses only components
// whose strict-mode support is certain.
func TestDegradedCardHasNoRiskyComponents(t *testing.T) {
	full := 100.0
	rep := oneProviderReport(domain.StateExhausted, window(&full, domain.ScopeAccount))
	card := New("").CardSimple(domain.Message{Report: rep})
	for _, tag := range []string{"chart", "collapsible_panel", "table"} {
		if n := len(findElements(card, tag)); n != 0 {
			t.Errorf("degraded card contains %d %s elements", n, tag)
		}
	}
	// The evidence still survives as markdown.
	if !containsJSON(t, card, "已用满") {
		t.Error("degraded card lost the state evidence")
	}
	// Degraded markdown uses the remaining caliber too.
	if !containsJSON(t, card, "最紧剩余 0.0%") {
		t.Error("degraded card did not use the remaining caliber")
	}
	if containsJSON(t, card, "已用 100.0%") {
		t.Error("degraded card printed the used caliber")
	}
}

// TestChartsCanBeDisabledWithoutCodeChange: CARD_CHARTS_ENABLED=false must
// produce a chart-free card even on the full path.
func TestChartsCanBeDisabledWithoutCodeChange(t *testing.T) {
	low := 10.0
	rep := oneProviderReport(domain.StateHealthy, window(&low, domain.ScopeAccount))
	card := New("").WithCharts(false).Card(domain.Message{Report: rep})
	if n := len(findElements(card, "chart")); n != 0 {
		t.Errorf("charts disabled but %d chart elements present", n)
	}
	// The rest of the card is unaffected.
	if !containsJSON(t, card, "最紧剩余 90.0%") {
		t.Error("disabling charts dropped the channel line")
	}
}

// TestCardNeverLeaksSecretsOrHandles: the internal auth handle, a cookie and a
// raw upstream error must not appear anywhere in the serialized card.
func TestCardNeverLeaksSecretsOrHandles(t *testing.T) {
	u := 99.0
	rep := &domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: 1,
		States: map[string]domain.CredentialState{"k": domain.StateWarning},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "k", Alias: "acct", ShortID: "a1b2", AuthIndex: "auth-index-must-not-leak"},
			OK:         true, Windows: []domain.QuotaWindow{window(&u, domain.ScopeAccount)},
			Err: "upstream error with SUPERSECRET_COOKIE_abc123 and Bearer sk-live-xyz789",
		}},
	}}}
	for _, full := range []bool{true, false} {
		r := New("")
		var card map[string]any
		if full {
			card = r.Card(domain.Message{Report: rep})
		} else {
			card = r.CardSimple(domain.Message{Report: rep})
		}
		raw, _ := json.Marshal(card)
		for _, secret := range []string{"auth-index-must-not-leak", "SUPERSECRET_COOKIE_abc123", "sk-live-xyz789", "auth_index"} {
			if containsJSON(t, card, secret) {
				t.Errorf("card (full=%v) leaked %q", full, secret)
			}
			_ = raw
		}
	}
}

// TestCardTimesUseConfiguredZone: the footer data time is converted, never the
// upstream wall clock.
func TestCardTimesUseConfiguredZone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	gen := time.Date(2026, 9, 30, 2, 39, 0, 0, time.UTC) // 10:39 +08
	rep := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: 1,
		States:    map[string]domain.CredentialState{"k": domain.StateHealthy},
		Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "k"}, OK: true, Windows: []domain.QuotaWindow{window(pctp(10), domain.ScopeAccount)}}},
	}}}
	card := New("").WithLocation(loc).Card(domain.Message{Report: rep})
	if !containsJSON(t, card, "2026-09-30 10:39") {
		t.Error("footer time not in the configured zone")
	}
	if containsJSON(t, card, "2026-09-30 02:39") {
		t.Error("footer printed the upstream wall clock")
	}
}

// --- helpers ---------------------------------------------------------------

func oneProviderReport(st domain.CredentialState, windows ...domain.QuotaWindow) *domain.Report {
	key := "k"
	snap := domain.QuotaSnapshot{
		Credential: domain.Credential{Key: key, Provider: domain.ProviderCodex, Alias: "acct"},
		OK:         true, Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported,
	}
	snap.Windows = windows
	return &domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: 1,
		States:    map[string]domain.CredentialState{key: st},
		Snapshots: []domain.QuotaSnapshot{snap},
	}}}
}

func containsJSON(t *testing.T, card map[string]any, want string) bool {
	t.Helper()
	b, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return strings.Contains(string(b), want)
}

func buttonCount(card map[string]any) int {
	return len(findElements(card, "button"))
}

func countBehaviors(card map[string]any, typ string) int {
	n := 0
	for _, b := range findElements(card, "button") {
		for _, beh := range b["behaviors"].([]any) {
			bm, _ := beh.(map[string]any)
			if bm["type"] == typ {
				n++
			}
		}
	}
	return n
}

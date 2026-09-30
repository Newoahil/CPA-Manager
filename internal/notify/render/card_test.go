package render

import (
	"encoding/json"
	"fmt"
	"regexp"
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
	card := New("").WithCharts(true).WithLocation(time.UTC).Card(domain.Message{Report: rep})

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
	// One channel => no separator; two credentials => two bars.
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

// TestConfigHasNoWideScreenMode: JSON 2.0's config is exactly
// {update_multi: true}; wide_screen_mode is a 1.0-era property and strict mode
// rejects unknown properties rather than ignoring them.
func TestConfigHasNoWideScreenMode(t *testing.T) {
	rep := oneProviderReport(domain.StateHealthy, window(pctp(10), domain.ScopeAccount))
	for _, full := range []bool{true, false} {
		r := New("")
		var card map[string]any
		if full {
			card = r.Card(domain.Message{Report: rep})
		} else {
			card = r.CardSimple(domain.Message{Report: rep})
		}
		cfg, ok := card["config"].(map[string]any)
		if !ok {
			t.Fatalf("card has no config")
		}
		if _, bad := cfg["wide_screen_mode"]; bad {
			t.Error("config still carries wide_screen_mode (not valid in JSON 2.0)")
		}
		if cfg["update_multi"] != true {
			t.Errorf("update_multi = %v, want true", cfg["update_multi"])
		}
		if len(cfg) != 1 {
			t.Errorf("config has %d keys, want only update_multi: %v", len(cfg), cfg)
		}
	}
}

// TestNoDefaultSpacing: the documented enum for horizontal_spacing/vertical_spacing
// is px sizes only; "default" is not in it, so the field must be absent.
func TestNoDefaultSpacing(t *testing.T) {
	rep := oneProviderReport(domain.StateInvalid)
	card := New("").Card(domain.Message{Report: rep})
	for _, e := range walkElementsFor(card) {
		for _, k := range []string{"horizontal_spacing", "vertical_spacing"} {
			if v, ok := e[k]; ok {
				t.Errorf("element %v carries %s=%v; use a px size or omit it", e["tag"], k, v)
			}
		}
	}
	if containsJSON(t, card, `"horizontal_spacing"`) {
		t.Error("serialized card still contains horizontal_spacing")
	}
}

// TestChartsOffByDefault: with no explicit opt-in the card carries no chart at
// all — the text lines already give the exact numbers.
func TestChartsOffByDefault(t *testing.T) {
	rep := oneProviderReport(domain.StateHealthy, window(pctp(35), domain.ScopeAccount))
	// New("") leaves charts at their zero value (false).
	card := New("").Card(domain.Message{Report: rep})
	if n := len(findElements(card, "chart")); n != 0 {
		t.Errorf("default card has %d charts, want 0", n)
	}
	if !containsJSON(t, card, "最紧剩余 65.0%") {
		t.Error("text overview line missing without the chart")
	}
}

// TestSingleChartForWholeCard: when charts are enabled the card has exactly one
// chart, of fixed height, covering every credential — not one chart per channel.
func TestSingleChartForWholeCard(t *testing.T) {
	mk := func(k domain.ProviderKind, key string, st domain.CredentialState, u float64) domain.ProviderReport {
		return domain.ProviderReport{
			Provider:  k,
			Total:     1,
			States:    map[string]domain.CredentialState{key: st},
			Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: key, Alias: key}, OK: true, Windows: []domain.QuotaWindow{window(&u, domain.ScopeAccount)}}},
		}
	}
	a, b, c, d := 10.0, 35.0, 41.0, 100.0
	rep := &domain.Report{Providers: []domain.ProviderReport{
		mk(domain.ProviderCodex, "cx", domain.StateHealthy, a),
		mk(domain.ProviderClaude, "cl", domain.StateHealthy, b),
		mk(domain.ProviderAntigravity, "ag", domain.StateHealthy, c),
		mk(domain.ProviderGeminiCLI, "gm", domain.StateExhausted, d),
	}}
	card := New("").WithCharts(true).WithLocation(time.UTC).Card(domain.Message{Report: rep})

	charts := findElements(card, "chart")
	if len(charts) != 1 {
		t.Fatalf("charts = %d, want exactly 1 for the whole card", len(charts))
	}
	ch := charts[0]
	if ch["height"] != chartHeight {
		t.Errorf("height = %v, want fixed %s", ch["height"], chartHeight)
	}
	if _, ok := ch["aspect_ratio"]; ok {
		t.Error("aspect_ratio must be replaced by a fixed height")
	}
	spec, _ := ch["chart_spec"].(map[string]any)
	if _, ok := spec["seriesField"]; ok {
		t.Error("seriesField must be absent so bars do not get rainbow colours")
	}
	// All four credentials are represented in the single chart, plus three
	// channel separators.
	data, _ := spec["data"].(map[string]any)
	values, _ := data["values"].([]any)
	labels := map[string]bool{}
	for _, val := range values {
		vm, _ := val.(map[string]any)
		labels[fmt.Sprint(vm["type"])] = true
	}
	for _, want := range []string{"cx", "cl", "ag", "gm"} {
		if !labels[want] {
			t.Errorf("credential %q missing from the card chart (labels=%v)", want, labels)
		}
	}
	// No separator rows: every value is a real credential reading, never a
	// zero-value spacer that would look like missing data.
	if len(values) != 4 {
		t.Errorf("values = %d, want 4 credential bars (labels=%v)", len(values), labels)
	}
	// value and text always agree.
	for _, val := range values {
		vm, _ := val.(map[string]any)
		if f, ok := vm["value"].(float64); ok {
			if f < 0 || f > 1 {
				t.Errorf("chart value %v outside 0–1", f)
			}
			if vm["text"] != fmt.Sprintf("%.0f%%", f*100) {
				t.Errorf("text %v disagrees with value %v", vm["text"], f)
			}
		}
	}
}

// TestChartLabelsAreShortAliases: the axis label must be the short alias, not
// "alias · shortid", which was truncated on the real card.
func TestChartLabelsAreShortAliases(t *testing.T) {
	u := 41.0
	rep := &domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: 1,
		States: map[string]domain.CredentialState{"k": domain.StateHealthy},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "k", Alias: "antigravity-leacanva92", ShortID: "ade0"},
			OK:         true,
			Windows:    []domain.QuotaWindow{window(&u, domain.ScopeAccount)},
		}},
	}}}
	card := New("").WithCharts(true).Card(domain.Message{Report: rep})
	ch := findElements(card, "chart")
	if len(ch) != 1 {
		t.Fatalf("charts = %d", len(ch))
	}
	spec, _ := ch[0]["chart_spec"].(map[string]any)
	data, _ := spec["data"].(map[string]any)
	values, _ := data["values"].([]any)
	found := false
	for _, val := range values {
		vm, _ := val.(map[string]any)
		label := fmt.Sprint(vm["type"])
		if strings.Contains(label, " · ") {
			t.Errorf("chart label %q still carries the truncated 'alias · shortid' form", label)
		}
		if label == "antigravity-leacanva92" {
			found = true
		}
	}
	if !found {
		t.Error("short alias not used as the chart label")
	}
}

// TestChartFallsBackToTextOverBarLimit: more bars than the readable maximum
// means no chart, and every channel keeps its text line.
func TestChartFallsBackToTextOverBarLimit(t *testing.T) {
	var snaps []domain.QuotaSnapshot
	states := map[string]domain.CredentialState{}
	for i := 0; i < maxChartBars+2; i++ {
		key := fmt.Sprintf("c%d", i)
		u := float64(10 + i)
		snaps = append(snaps, domain.QuotaSnapshot{
			Credential: domain.Credential{Key: key, Alias: key}, OK: true,
			Windows: []domain.QuotaWindow{window(&u, domain.ScopeAccount)},
		})
		states[key] = domain.StateHealthy
	}
	rep := &domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: len(snaps), States: states, Snapshots: snaps,
	}}}
	card := New("").WithCharts(true).Card(domain.Message{Report: rep})
	if n := len(findElements(card, "chart")); n != 0 {
		t.Errorf("over-limit card has %d charts, want 0 (fall back to text)", n)
	}
	// Tightest of 10..19% used is 19% used -> 81% remaining.
	text := jsonText2(t, card)
	if !strings.Contains(text, "最紧剩余 81.0%") {
		t.Errorf("text fallback lost the channel overview line:\n%s", text)
	}
}

// TestPanelContainsNoTableOrChart is the regression for the production failure
// "type of element is not supported tag: table, path: ... collapsible_panel":
// Feishu rejects a table (and we must not risk a chart) inside a panel, so the
// panel contains markdown only.
func TestPanelContainsNoTableOrChart(t *testing.T) {
	full := 100.0
	rep := oneProviderReport(domain.StateExhausted, window(&full, domain.ScopeAccount))
	card := New("").WithLocation(time.UTC).Card(domain.Message{Report: rep})

	panels := findElements(card, "collapsible_panel")
	if len(panels) != 1 {
		t.Fatalf("panels = %d, want 1", len(panels))
	}
	if len(findElements(card, "table")) != 0 {
		t.Error("card still contains a table")
	}
	inner, _ := panels[0]["elements"].([]any)
	if len(inner) == 0 {
		t.Fatal("panel has no elements")
	}
	for _, e := range inner {
		m, _ := e.(map[string]any)
		if m["tag"] != "markdown" {
			t.Errorf("panel contains unsupported element %v, want markdown only", m["tag"])
		}
	}
	// The panel's field is "elements" (matching the official schema), and its
	// markdown still carries the per-credential evidence.
	if !containsJSON(t, card, "最紧剩余 0.0%") {
		t.Error("panel markdown lost the remaining evidence")
	}
}

// TestNoLiteralMarkdownDelimiters is the regression for "**结论: **Claude":
// a closing ** followed by a non-space character is not parsed as an emphasis
// boundary, so the asterisks printed literally.
func TestNoLiteralMarkdownDelimiters(t *testing.T) {
	used := 94.0
	rep := oneProviderReport(domain.StateHealthy, window(&used, domain.ScopeAccount))
	for _, full := range []bool{true, false} {
		r := New("")
		var card map[string]any
		if full {
			card = r.Card(domain.Message{Report: rep})
		} else {
			card = r.CardSimple(domain.Message{Report: rep})
		}
		for _, e := range walkElementsFor(card) {
			if e["tag"] != "markdown" {
				continue
			}
			content, _ := e["content"].(string)
			// The old, unparsed form: "**结论：**Claude" put a closing **
			// directly before a letter. The corrected form is "**结论：** x".
			if strings.Contains(content, "**Claude") || strings.Contains(content, "**Antigravity") {
				t.Errorf("markdown content has a non-parsing delimiter:\n%s", content)
			}
		}
	}
	// The heading itself still exists, just in a form that renders: the closing
	// ** is followed by a space.
	card := New("").Card(domain.Message{Report: rep})
	if !containsJSON(t, card, "**结论：** ") {
		t.Error("conclusion heading lost its emphasis form")
	}
}

// TestSingleCaliberNoUsedRemainingMix: one card must not mix "已用 N%" and
// "剩余 M%" in user-visible text.
func TestSingleCaliberNoUsedRemainingMix(t *testing.T) {
	used := 94.0
	rep := oneProviderReport(domain.StateWarning, window(&used, domain.ScopeAccount))
	rep.Recommendations = []domain.Recommendation{{
		Provider:  domain.ProviderCodex,
		Direction: domain.DirectionEaseOff,
		// Exactly the evaluator's wording: used-caliber plus threshold phrase.
		Reason: "所选单个凭证的 账号 · 周窗口 已用 94%，达到通知阈值，建议放缓该凭证的使用。",
	}}
	r := New("")
	card := r.Card(domain.Message{Report: rep})
	if containsJSON(t, card, "已用 94%") {
		t.Error("card leaked the evaluator's used-caliber wording")
	}
	if containsJSON(t, card, "达到通知阈值") {
		t.Error("card leaked the evaluator's used-caliber threshold phrase")
	}
	// The recommendation is restated in the remaining caliber.
	if !containsJSON(t, card, "剩余 6.0%") {
		t.Errorf("advice not restated as remaining:\n%s", jsonText2(t, card))
	}
	if !containsJSON(t, card, "低于提醒线") {
		t.Error("threshold phrase not restated in the remaining caliber")
	}

	// The raw helper is the single choke point.
	if got := toRemainingCaliber("已用 94%"); got != "剩余 6.0%" {
		t.Errorf("toRemainingCaliber = %q, want 剩余 6.0%%", got)
	}
	if got := toRemainingCaliber("额度已耗尽"); got != "额度已耗尽" {
		t.Errorf("caliber rewrite touched wording with no percentage: %q", got)
	}
	if got := toRemainingCaliber("已用 12.5 credits"); got != "已用 12.5 credits" {
		t.Errorf("caliber rewrite touched a credit amount: %q", got)
	}
}

func jsonText2(t *testing.T, card map[string]any) string {
	t.Helper()
	b, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
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

// TestBrandColorsNonStatus verifies that brand tag colors are distinctive non-status colors.
func TestBrandColorsNonStatus(t *testing.T) {
	cases := []struct {
		provider domain.ProviderKind
		want     string
	}{
		{domain.ProviderClaude, "violet"},
		{domain.ProviderAntigravity, "blue"},
		{domain.ProviderCodex, "turquoise"},
		{domain.ProviderGeminiCLI, "indigo"},
		{domain.ProviderOllama, "purple"},
	}
	for _, tc := range cases {
		got := brandTagColor(tc.provider)
		if got != tc.want {
			t.Errorf("brandTagColor(%s) = %q, want %q", tc.provider, got, tc.want)
		}
		if got == "red" || got == "orange" || got == "green" {
			t.Errorf("brandTagColor(%s) must not use status alarm colors (got %s)", tc.provider, got)
		}
	}
}

// TestErrorCodeRenderedInAbnormalCredential tests that Code is rendered in evidence when present.
func TestErrorCodeRenderedInAbnormalCredential(t *testing.T) {
	rep := &domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: 1,
		States: map[string]domain.CredentialState{"k": domain.StateInvalid},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "k", Alias: "codex-design"},
			Failure:    domain.FailureAuth,
			Code:       "401",
		}},
	}}}
	card := New("").Card(domain.Message{Report: rep})
	text := jsonText2(t, card)
	if !strings.Contains(text, "401") {
		t.Errorf("card missing error code 401:\n%s", text)
	}
	if !strings.Contains(text, "认证被上游拒绝") {
		t.Errorf("card missing failure reason:\n%s", text)
	}
}

// TestIsomorphicHealthyChannelFolding tests that an all-healthy channel with identical window structure
// displays 1 sample and folds the rest with a clear title.
func TestIsomorphicHealthyChannelFolding(t *testing.T) {
	u1, u2 := 5.0, 10.0
	agWindow := func(name, label string, used *float64) domain.QuotaWindow {
		return domain.QuotaWindow{Name: name, Label: label, Scope: domain.ScopeGroup, ScopeID: "gemini", UsedPercent: used}
	}
	rep := &domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderAntigravity, Total: 2,
		States: map[string]domain.CredentialState{"ag1": domain.StateHealthy, "ag2": domain.StateHealthy},
		Snapshots: []domain.QuotaSnapshot{
			{
				Credential: domain.Credential{Key: "ag1", Alias: "ag-alpha"},
				OK:         true,
				Windows: []domain.QuotaWindow{
					agWindow("w-week", "周窗口", &u1),
					agWindow("w-5h", "5h窗口", &u1),
				},
			},
			{
				Credential: domain.Credential{Key: "ag2", Alias: "ag-beta"},
				OK:         true,
				Windows: []domain.QuotaWindow{
					agWindow("w-week", "周窗口", &u2),
					agWindow("w-5h", "5h窗口", &u2),
				},
			},
		},
	}}}
	card := New("").Card(domain.Message{Report: rep})
	text := jsonText2(t, card)
	if !strings.Contains(text, "样本") {
		t.Errorf("isomorphic channel missing sample tag:\n%s", text)
	}
	if !strings.Contains(text, "另 1 个号结构相同，均正常（最紧 90.0%）") {
		t.Errorf("isomorphic channel missing folded summary line:\n%s", text)
	}
	panels := findElements(card, "collapsible_panel")
	if len(panels) != 1 {
		t.Fatalf("expected 1 collapsible panel for isomorphic rest, got %d", len(panels))
	}
}

// TestCardChannelSeparators verifies hr element separates multiple providers.
func TestCardChannelSeparators(t *testing.T) {
	u := 10.0
	rep := &domain.Report{Providers: []domain.ProviderReport{
		{
			Provider: domain.ProviderClaude, Total: 1,
			States:    map[string]domain.CredentialState{"c1": domain.StateHealthy},
			Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "c1", Alias: "c1"}, OK: true, Windows: []domain.QuotaWindow{window(&u, domain.ScopeAccount)}}},
		},
		{
			Provider: domain.ProviderCodex, Total: 1,
			States:    map[string]domain.CredentialState{"c2": domain.StateHealthy},
			Snapshots: []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "c2", Alias: "c2"}, OK: true, Windows: []domain.QuotaWindow{window(&u, domain.ScopeAccount)}}},
		},
	}}
	card := New("").Card(domain.Message{Report: rep})
	hrs := findElements(card, "hr")
	// There should be at least: kicker/head hr (if any) or provider separator hr, and tail hr
	if len(hrs) < 2 {
		t.Errorf("expected at least 2 hr elements with multiple channels, got %d", len(hrs))
	}
}

// TestCardDefectFixes tests the 8 specific defect fixes.
func TestCardDefectFixes(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	gen := time.Date(2026, 9, 30, 9, 48, 0, 0, time.UTC)
	r := New("casual").WithLocation(loc)

	// 1. 夹具带 Confidence 时健康渠道不出现任何 caution 文字；
	u := 10.0
	healthySnap := domain.QuotaSnapshot{
		Credential: domain.Credential{Key: "cx1", Provider: domain.ProviderCodex, Alias: "codex-test"},
		OK:         true,
		Confidence: domain.ConfidenceReported,
		Windows:    []domain.QuotaWindow{{Name: "w1", Label: "账号 · 周", Scope: domain.ScopeAccount, UsedPercent: &u}},
	}
	rep1 := &domain.Report{Providers: []domain.ProviderReport{{
		Provider:   domain.ProviderCodex,
		Total:      1,
		Normal:     1,
		WorstState: domain.StateHealthy,
		States:     map[string]domain.CredentialState{"cx1": domain.StateHealthy},
		Snapshots:  []domain.QuotaSnapshot{healthySnap},
	}}}
	card1 := r.Card(domain.Message{Report: rep1})
	text1 := jsonText2(t, card1)
	if strings.Contains(text1, "全部取不到") || strings.Contains(text1, "部分取不到") || strings.Contains(text1, "估算值") {
		t.Errorf("healthy channel with ConfidenceReported should not have caution words:\n%s", text1)
	}

	// 2. 失败快照不导致整渠道被标「全部取不到」；
	rep2 := &domain.Report{Providers: []domain.ProviderReport{{
		Provider:   domain.ProviderCodex,
		Total:      2,
		Normal:     1,
		Abnormal:   1,
		WorstState: domain.StateInvalid,
		States:     map[string]domain.CredentialState{"cx1": domain.StateHealthy, "cx2": domain.StateInvalid},
		Snapshots: []domain.QuotaSnapshot{
			healthySnap,
			{
				Credential: domain.Credential{Key: "cx2", Provider: domain.ProviderCodex, Alias: "codex-bad"},
				OK:         false,
				Failure:    domain.FailureAuth,
				Code:       "401",
			},
		},
	}}}
	card2 := r.Card(domain.Message{Report: rep2})
	text2 := jsonText2(t, card2)
	if strings.Contains(text2, "全部取不到") {
		t.Errorf("failure snapshot caused entire channel to be marked 全部取不到:\n%s", text2)
	}

	// 3. 同一渠道名不重复出现（例如不能是 **Claude** Claude 或 **Claude** <text_tag...>Claude</text_tag>）；
	for _, prov := range []string{"Claude", "Antigravity", "Codex"} {
		if strings.Contains(text1, "**"+prov+"** "+prov) || strings.Contains(text1, "**"+prov+"** <text_tag") {
			t.Errorf("channel %s has duplicate name:\n%s", prov, text1)
		}
	}

	// 4. 失效行原因只出现一次；
	if n := strings.Count(text2, "认证被上游拒绝"); n != 1 {
		t.Errorf("failure reason '认证被上游拒绝' should appear exactly once in credential evidence, got %d:\n%s", n, text2)
	}

	// 5. 全正常时 header 非橙非红；
	allHealthyRep := &domain.Report{Providers: []domain.ProviderReport{
		{
			Provider:   domain.ProviderCodex,
			Total:      1,
			Normal:     1,
			WorstState: domain.StateHealthy,
			States:     map[string]domain.CredentialState{"cx1": domain.StateHealthy},
			Snapshots:  []domain.QuotaSnapshot{healthySnap},
		},
		{
			Provider:   domain.ProviderAntigravity,
			Total:      1,
			Limited:    1,
			WorstState: domain.StateLimited,
			States:     map[string]domain.CredentialState{"ag1": domain.StateLimited},
			Snapshots: []domain.QuotaSnapshot{{
				Credential: domain.Credential{Key: "ag1", Provider: domain.ProviderAntigravity, Alias: "ag-1"},
				OK:         true,
				Confidence: domain.ConfidenceReported,
				Windows:    []domain.QuotaWindow{{Name: "ag/w", Label: "周", Scope: domain.ScopeGroup, ScopeID: "g1", UsedPercent: &u}},
			}},
		},
	}}
	cardAllHealthy := r.Card(domain.Message{Kind: KindQuery, Report: allHealthyRep})
	headerTmpl := cardAllHealthy["header"].(map[string]any)["template"].(string)
	if headerTmpl == "orange" || headerTmpl == "red" {
		t.Errorf("all healthy report header template should not be orange or red, got %q", headerTmpl)
	}

	// 6. 面板标题计数等于面板内账号数；
	// In rep2, Codex has 2 credentials total, but only 1 is abnormal (cx2), so panel should say "1 个号"
	if !strings.Contains(text2, "Codex 异常明细（1 个号）") {
		t.Errorf("panel title should count accounts in panel (1 个号), got:\n%s", text2)
	}

	// 7. 用户可见文本不含 [分组]；
	agSnapTwoGroups := domain.QuotaSnapshot{
		Credential: domain.Credential{Key: "ag-sample", Provider: domain.ProviderAntigravity, Alias: "ag-sample"},
		OK:         true,
		Confidence: domain.ConfidenceReported,
		Plan:       "Pro",
		Windows: []domain.QuotaWindow{
			{Name: "ag/g1/5h", Label: "Gemini Models · 5小时", Scope: domain.ScopeGroup, ScopeID: "antigravity/groups/Gemini%20Models#1", UsedPercent: &u},
			{Name: "ag/g2/5h", Label: "Claude 和 GPT 模型组 · 5小时", Scope: domain.ScopeGroup, ScopeID: "antigravity/groups/Claude%20and%20GPT%20Models#1", UsedPercent: &u},
		},
	}
	agRepSample := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{{
		Provider:   domain.ProviderAntigravity,
		Total:      2,
		Limited:    2,
		WorstState: domain.StateLimited,
		States:     map[string]domain.CredentialState{"ag-sample": domain.StateLimited, "ag-2": domain.StateLimited},
		Snapshots:  []domain.QuotaSnapshot{agSnapTwoGroups, agSnapTwoGroups},
	}}}
	cardAG := r.Card(domain.Message{Kind: KindQuery, Report: agRepSample})
	textAG := jsonText2(t, cardAG)
	if strings.Contains(textAG, "[分组]") {
		t.Errorf("user visible text leaked internal '[分组]':\n%s", textAG)
	}

	// 8. 样本账号包含其全部模型组。
	if !strings.Contains(textAG, "▸ 分组 Gemini 模型") || !strings.Contains(textAG, "▸ 分组 Claude / GPT 模型") {
		t.Errorf("sample credential must expand both model groups with translated names, got:\n%s", textAG)
	}
}

// TestFourScreenDefectFixes tests:
// 1. All user-visible percentages rounded to <= 1 decimal place (e.g. no 88.50824%).
// 2. Window rows in tier 4 contain only normalized window names, without duplicating group names or raw bucket strings.
// 3. Known group names translated to Chinese, unknown preserved verbatim.
// 4. Healthy single-account or non-isomorphic multi-account channels render compact credential rows with window remainders.
func TestFourScreenDefectFixes(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	gen := time.Date(2026, 9, 30, 9, 48, 0, 0, time.UTC)
	r := New("casual").WithLocation(loc)

	// 1. Percentage precision test: 88.50824% -> 88.5%
	pctVal := 11.49176 // 100 - 11.49176 = 88.50824
	agSnap := func(key, alias string) domain.QuotaSnapshot {
		return domain.QuotaSnapshot{
			Credential: domain.Credential{Key: key, Provider: domain.ProviderAntigravity, Alias: alias},
			Plan:       "Pro", OK: true, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen,
			Windows: []domain.QuotaWindow{
				{
					Name:        "antigravity/groups/Gemini%20Models#1/buckets/gemini-5h/five_hour#1",
					Label:       "Gemini Models · Five Hour Limit Remaining · 5小时",
					Scope:       domain.ScopeGroup,
					ScopeID:     "antigravity/groups/Gemini%20Models#1",
					UsedPercent: &pctVal,
				},
			},
		}
	}
	repAG := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{{
		Provider:   domain.ProviderAntigravity,
		Total:      2,
		Limited:    2,
		WorstState: domain.StateLimited,
		States:     map[string]domain.CredentialState{"ag1": domain.StateLimited, "ag2": domain.StateLimited},
		Snapshots:  []domain.QuotaSnapshot{agSnap("ag1", "ag-1"), agSnap("ag2", "ag-2")},
	}}}
	cardAG := r.Card(domain.Message{Kind: KindQuery, Report: repAG})
	textAG := jsonText2(t, cardAG)

	// Assert no percentage with > 1 decimal place exists
	overPrecisionRegex := regexp.MustCompile(`\d+\.\d{2,}%`)
	if m := overPrecisionRegex.FindString(textAG); m != "" {
		t.Errorf("found over-precise unrounded percentage %q in card JSON:\n%s", m, textAG)
	}
	if !strings.Contains(textAG, "88.5%") {
		t.Errorf("expected rounded 88.5%%, got:\n%s", textAG)
	}

	// 2. Window rows: only normalized window name, no group name, no raw bucket text
	if strings.Contains(textAG, "Five Hour Limit Remaining") {
		t.Errorf("tier 4 window line contains raw upstream bucket text 'Five Hour Limit Remaining':\n%s", textAG)
	}
	// In the expanded sample, the tier-4 window line should be "      · 5小时 88.5%  重置 未上报" without repeating "Gemini"
	if !strings.Contains(textAG, `\n      · 5小时 88.5%  重置 未上报`) {
		t.Errorf("expected normalized tier 4 window line '· 5小时 88.5%%', got:\n%s", textAG)
	}
	if strings.Contains(textAG, "· Gemini") {
		t.Errorf("window line should not repeat group name 'Gemini':\n%s", textAG)
	}

	// 3. Known group names translated to Chinese, unknown preserved verbatim
	if !strings.Contains(textAG, "▸ 分组 Gemini 模型") {
		t.Errorf("expected translated group name '▸ 分组 Gemini 模型', got:\n%s", textAG)
	}

	// Unknown group name test:
	repUnknownGroup := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{{
		Provider:   domain.ProviderAntigravity,
		Total:      1,
		Limited:    1,
		WorstState: domain.StateLimited,
		States:     map[string]domain.CredentialState{"ag1": domain.StateLimited},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "ag1", Provider: domain.ProviderAntigravity, Alias: "ag-custom"},
			OK:         true, Confidence: domain.ConfidenceReported,
			Windows: []domain.QuotaWindow{{
				Name: "ag/g/custom", Label: "Custom Experimental Group · 周",
				Scope: domain.ScopeGroup, ScopeID: "antigravity/groups/Custom%20Experimental%20Group#1",
				UsedPercent: &pctVal,
			}},
		}},
	}}}
	cardUnknown := r.Card(domain.Message{Kind: KindQuery, Report: repUnknownGroup, Detailed: true})
	textUnknown := jsonText2(t, cardUnknown)
	if !strings.Contains(textUnknown, "Custom Experimental Group") {
		t.Errorf("unknown group name should be preserved verbatim, got:\n%s", textUnknown)
	}

	// 4. Normal channels detail rows:
	// Codex (1 normal account) must render its compact line!
	uCodex := 56.0
	repCodex := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{{
		Provider:   domain.ProviderCodex,
		Total:      1,
		Normal:     1,
		WorstState: domain.StateHealthy,
		States:     map[string]domain.CredentialState{"cx1": domain.StateHealthy},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "cx1", Provider: domain.ProviderCodex, Alias: "codex-single"},
			Plan:       "pro", OK: true, Confidence: domain.ConfidenceReported,
			Windows: []domain.QuotaWindow{
				{Name: "codex/weekly", Label: "账号 · 周窗口", Scope: domain.ScopeAccount, UsedPercent: &uCodex},
			},
		}},
	}}}
	cardCodex := r.Card(domain.Message{Kind: KindQuery, Report: repCodex})
	textCodex := jsonText2(t, cardCodex)
	if !strings.Contains(textCodex, "codex-single") || !strings.Contains(textCodex, "周 剩 44.0%") {
		t.Errorf("single normal account Codex must render compact detail row:\n%s", textCodex)
	}

	// Claude (2 accounts, non-isomorphic windows, all healthy) must render BOTH compact lines!
	uClaude1, uClaude2 := 17.0, 46.0
	repClaude := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{{
		Provider:   domain.ProviderClaude,
		Total:      2,
		Normal:     2,
		WorstState: domain.StateHealthy,
		States:     map[string]domain.CredentialState{"cl1": domain.StateHealthy, "cl2": domain.StateHealthy},
		Snapshots: []domain.QuotaSnapshot{
			{
				Credential: domain.Credential{Key: "cl1", Provider: domain.ProviderClaude, Alias: "claude-1"},
				OK:         true, Confidence: domain.ConfidenceReported,
				Windows: []domain.QuotaWindow{
					{Name: "claude/5h", Label: "账号 · 5小时", Scope: domain.ScopeAccount, UsedPercent: &uClaude1},
				},
			},
			{
				Credential: domain.Credential{Key: "cl2", Provider: domain.ProviderClaude, Alias: "claude-2"},
				OK:         true, Confidence: domain.ConfidenceReported,
				Windows: []domain.QuotaWindow{
					{Name: "claude/7d", Label: "账号 · 7天", Scope: domain.ScopeAccount, UsedPercent: &uClaude2},
				},
			},
		},
	}}}
	cardClaude := r.Card(domain.Message{Kind: KindQuery, Report: repClaude})
	textClaude := jsonText2(t, cardClaude)
	if !strings.Contains(textClaude, "claude-1") || !strings.Contains(textClaude, "5h 剩 83.0%") {
		t.Errorf("non-isomorphic Claude account 1 missing:\n%s", textClaude)
	}
	if !strings.Contains(textClaude, "claude-2") || !strings.Contains(textClaude, "7d 剩 54.0%") {
		t.Errorf("non-isomorphic Claude account 2 missing:\n%s", textClaude)
	}
}
// (1: single channel abnormal, 2: all channels healthy, 3: multiple channels broken)
// and verifies their structure and logs the actual JSON.
func TestThreeProductionScenariosCardJSON(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	gen := time.Date(2026, 9, 30, 9, 48, 0, 0, time.UTC)
	r := New("casual").WithLocation(loc)

	atTime := func(mm, dd, hh, mi int) *time.Time {
		tm := time.Date(2026, time.Month(mm), dd, hh, mi, 0, 0, time.UTC)
		return &tm
	}

	// Scenario 1: Single channel abnormal (Claude External0.2 5h remaining 21%, Antigravity 4 isomorphic normal, Codex 1 normal)
	s1Claude := domain.ProviderReport{
		Provider: domain.ProviderClaude, Total: 2, Normal: 1, Abnormal: 1,
		WorstState: domain.StateNotice,
		States:     map[string]domain.CredentialState{"cl1": domain.StateNotice, "cl2": domain.StateHealthy},
		Snapshots: []domain.QuotaSnapshot{
			{
				Credential: domain.Credential{Key: "cl1", Provider: domain.ProviderClaude, Alias: "claude-External0.2"},
				Plan:       "团队版", OK: true, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen,
				Windows: []domain.QuotaWindow{
					{Name: "claude/five_hour", Label: "账号 · 5小时", Scope: domain.ScopeAccount, UsedPercent: pctp(79.0), ResetAt: atTime(9, 30, 12, 0)},
					{Name: "claude/seven_day", Label: "账号 · 7天", Scope: domain.ScopeAccount, UsedPercent: pctp(30.0), ResetAt: atTime(10, 5, 9, 0)},
				},
			},
			{
				Credential: domain.Credential{Key: "cl2", Provider: domain.ProviderClaude, Alias: "claude-External"},
				OK:         true, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen,
				Windows: []domain.QuotaWindow{
					{Name: "claude/five_hour", Label: "账号 · 5小时", Scope: domain.ScopeAccount, UsedPercent: pctp(17.0), ResetAt: atTime(9, 30, 14, 0)},
					{Name: "claude/seven_day", Label: "账号 · 7天", Scope: domain.ScopeAccount, UsedPercent: pctp(44.0), ResetAt: atTime(10, 4, 9, 0)},
					{Name: "claude/fable", Label: "fable 模型 · 周", Scope: domain.ScopeModel, ScopeID: "fable", UsedPercent: pctp(7.0), ResetAt: atTime(10, 1, 21, 0)},
				},
			},
		},
	}
	agSnap := func(key, alias string, used5h, usedWeek float64) domain.QuotaSnapshot {
		return domain.QuotaSnapshot{
			Credential: domain.Credential{Key: key, Provider: domain.ProviderAntigravity, Alias: alias},
			Plan:       "Pro", OK: true, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen,
			Windows: []domain.QuotaWindow{
				{Name: "antigravity/groups/Gemini%20Models#1/buckets/gemini-5h/five_hour#1", Label: "Gemini Models · 5小时", Scope: domain.ScopeGroup, ScopeID: "antigravity/groups/Gemini%20Models#1", UsedPercent: pctp(used5h), ResetAt: atTime(9, 30, 13, 9)},
				{Name: "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1", Label: "Gemini Models · 周", Scope: domain.ScopeGroup, ScopeID: "antigravity/groups/Gemini%20Models#1", UsedPercent: pctp(usedWeek), ResetAt: atTime(10, 6, 8, 48)},
				{Name: "antigravity/groups/Claude%20and%20GPT%20Models#1/buckets/cg-5h/five_hour#1", Label: "Claude 和 GPT 模型组 · 5小时", Scope: domain.ScopeGroup, ScopeID: "antigravity/groups/Claude%20and%20GPT%20Models#1", UsedPercent: pctp(used5h * 0.5), ResetAt: atTime(9, 30, 14, 30)},
				{Name: "antigravity/groups/Claude%20and%20GPT%20Models#1/buckets/cg-weekly/weekly#1", Label: "Claude 和 GPT 模型组 · 周", Scope: domain.ScopeGroup, ScopeID: "antigravity/groups/Claude%20and%20GPT%20Models#1", UsedPercent: pctp(usedWeek * 0.5), ResetAt: atTime(10, 6, 8, 48)},
			},
		}
	}
	s1AG := domain.ProviderReport{
		Provider: domain.ProviderAntigravity, Total: 4, Limited: 4,
		WorstState: domain.StateLimited,
		States: map[string]domain.CredentialState{
			"ag1": domain.StateLimited, "ag2": domain.StateLimited, "ag3": domain.StateLimited, "ag4": domain.StateLimited,
		},
		Snapshots: []domain.QuotaSnapshot{
			agSnap("ag1", "antigravity-hongwane3", 7.0, 4.0),
			agSnap("ag2", "antigravity-leacanva92", 7.0, 4.0),
			agSnap("ag3", "antigravity-leejhyijiaace5", 5.0, 2.0),
			agSnap("ag4", "antigravity-newoahil", 3.0, 1.0),
		},
	}
	s1Codex := domain.ProviderReport{
		Provider: domain.ProviderCodex, Total: 1, Normal: 1,
		WorstState: domain.StateHealthy,
		States:     map[string]domain.CredentialState{"cx1": domain.StateHealthy},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "cx1", Provider: domain.ProviderCodex, Alias: "codex-vinsprite78"},
			Plan:       "Pro 20x", OK: true, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen,
			Windows: []domain.QuotaWindow{
				{Name: "codex/weekly", Label: "账号 · 周窗口", Scope: domain.ScopeAccount, UsedPercent: pctp(16.0), ResetAt: atTime(10, 6, 9, 48)},
			},
		}},
	}

	rep1 := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{s1Claude, s1AG, s1Codex}}
	card1 := r.Card(domain.Message{Kind: KindQuery, Report: rep1, Freshness: "实时"})
	json1 := jsonText2(t, card1)
	t.Logf("\n=== 工况 1（单号异常）JSON ===\n%s", json1)
	if !strings.Contains(json1, "claude-External") || !strings.Contains(json1, "5h 剩 83.0%") {
		t.Errorf("Scenario 1 missing compact normal sibling line:\n%s", json1)
	}
	if !strings.Contains(json1, "Claude 异常明细（1 个号）") {
		t.Errorf("Scenario 1 panel title must be 'Claude 异常明细（1 个号）':\n%s", json1)
	}
	if !strings.Contains(json1, "另 3 个号结构相同，均正常（最紧 93.0%）") {
		t.Errorf("Scenario 1 missing isomorphic folded title:\n%s", json1)
	}
	if strings.Contains(json1, "全部取不到") {
		t.Errorf("Scenario 1 should not contain 全部取不到:\n%s", json1)
	}
	if strings.Contains(json1, "[分组]") {
		t.Errorf("Scenario 1 leaked [分组]:\n%s", json1)
	}
	if !strings.Contains(json1, "▸ 分组 Gemini 模型") || !strings.Contains(json1, "▸ 分组 Claude / GPT 模型") {
		t.Errorf("Scenario 1 Antigravity sample must expand both model groups:\n%s", json1)
	}

	// Scenario 2: All healthy
	s2Claude := s1Claude
	s2Claude.Normal = 2
	s2Claude.Abnormal = 0
	s2Claude.WorstState = domain.StateHealthy
	s2Claude.States = map[string]domain.CredentialState{"cl1": domain.StateHealthy, "cl2": domain.StateHealthy}
	s2Claude.Snapshots = []domain.QuotaSnapshot{
		{
			Credential: domain.Credential{Key: "cl1", Provider: domain.ProviderClaude, Alias: "claude-main"},
			OK:         true, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen,
			Windows: []domain.QuotaWindow{
				{Name: "claude/five_hour", Label: "账号 · 5小时", Scope: domain.ScopeAccount, UsedPercent: pctp(17.0), ResetAt: atTime(9, 30, 14, 0)},
			},
		},
		s1Claude.Snapshots[1],
	}
	rep2 := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{s2Claude, s1AG, s1Codex}}
	card2 := r.Card(domain.Message{Kind: KindQuery, Report: rep2, Freshness: "实时"})
	json2 := jsonText2(t, card2)
	t.Logf("\n=== 工况 2（全部正常）JSON ===\n%s", json2)
	header2 := card2["header"].(map[string]any)
	if header2["template"] != "blue" {
		t.Errorf("Scenario 2 (all healthy) header template must be 'blue', got %q", header2["template"])
	}
	if strings.Contains(json2, "全部取不到") {
		t.Errorf("Scenario 2 should not contain 全部取不到:\n%s", json2)
	}
	// Verify normal accounts are now rendered!
	if !strings.Contains(json2, "claude-main") || !strings.Contains(json2, "codex-vinsprite78") {
		t.Errorf("Scenario 2 must render compact rows for healthy Claude and Codex:\n%s", json2)
	}

	// Scenario 3: Multi-channel abnormal (with error code 401)
	s3Codex := domain.ProviderReport{
		Provider: domain.ProviderCodex, Total: 2, Abnormal: 2,
		WorstState: domain.StateInvalid,
		States:     map[string]domain.CredentialState{"cx-bad": domain.StateInvalid, "cx-full": domain.StateExhausted},
		Snapshots: []domain.QuotaSnapshot{
			{
				Credential: domain.Credential{Key: "cx-bad", Provider: domain.ProviderCodex, Alias: "codex-design"},
				Failure:    domain.FailureAuth, Code: "401",
			},
			{
				Credential: domain.Credential{Key: "cx-full", Provider: domain.ProviderCodex, Alias: "codex-vintechg1"},
				Plan:       "pro", OK: true, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen,
				Windows: []domain.QuotaWindow{
					{Name: "codex/weekly", Label: "账号 · 周窗口", Scope: domain.ScopeAccount, UsedPercent: pctp(100.0), ResetAt: atTime(10, 3, 17, 0)},
				},
			},
		},
	}
	s3Claude := domain.ProviderReport{
		Provider: domain.ProviderClaude, Total: 1, Abnormal: 1,
		WorstState: domain.StateExhausted,
		States:     map[string]domain.CredentialState{"cl1": domain.StateExhausted},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "cl1", Provider: domain.ProviderClaude, Alias: "claude-External0.2"},
			OK:         true, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen,
			Windows: []domain.QuotaWindow{
				{Name: "claude/five_hour", Label: "账号 · 5小时", Scope: domain.ScopeAccount, UsedPercent: pctp(100.0), ResetAt: atTime(9, 30, 12, 0)},
			},
		}},
	}
	rep3 := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{s3Codex, s3Claude, s1AG}}
	card3 := r.Card(domain.Message{Kind: string(domain.AlertQuotaExhausted), Report: rep3, Freshness: "实时"})
	json3 := jsonText2(t, card3)
	t.Logf("\n=== 工况 3（多渠道同时异常）JSON ===\n%s", json3)
	if !strings.Contains(json3, "凭证失效 · 401 · 认证被上游拒绝") {
		t.Errorf("Scenario 3 missing error code 401 format:\n%s", json3)
	}
	if strings.Count(json3, "认证被上游拒绝") != 1 {
		t.Errorf("Scenario 3 failure reason duplicated in:\n%s", json3)
	}
	if strings.Contains(json3, "全部取不到") {
		t.Errorf("Scenario 3 should not contain 全部取不到:\n%s", json3)
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

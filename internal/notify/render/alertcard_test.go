package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/calendar"
	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/evaluate"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

// engineTestConfig is the threshold configuration the evaluator tests use, so
// the alert facts and levels produced here match production shapes.
func engineTestConfig() config.Config {
	return config.Config{
		DefaultThresholds:  config.Thresholds{Notice: 90, Warn: 95, Urgent: 100},
		ProviderThresholds: map[domain.ProviderKind]config.Thresholds{},
		StaleAfterFailures: 2,
		AnomalyConsecutive: 3,
		AnomalyWindow:      5 * time.Minute,
		AnomalyFailureRate: 0.2,
		AnomalyMinRequests: 5,
		Location:           time.UTC,
	}
}

func engineState() *state.State {
	return &state.State{Bootstrapped: true, Credentials: map[string]state.CredentialRecord{}}
}

// TestEngineAlertCardNamesTheWindow is the regression for the root bug: a
// threshold alert went out without a "窗口: " fact, so the card fell back to
// listing every window with no big number and no reset time.
//
// It drives the REAL evaluate path and renders whatever alert it produced (the
// fact is prepended by internal/evaluate/scope.go). If the renderer ever stops
// consuming that fact, this fails: the second window of the snapshot would
// reappear in the card and the named window/reset time would vanish.
func TestEngineAlertCardNamesTheWindow(t *testing.T) {
	loc := time.UTC
	cfg := engineTestConfig()
	eng := evaluate.New(cfg, calendar.New(loc))

	cred := domain.Credential{Key: "cx1", Provider: domain.ProviderCodex, Alias: "codex-main", ShortID: "cb31"}
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, loc)
	reset := time.Date(2026, 10, 9, 18, 0, 0, 0, loc)
	secondary := 10.0
	snap := func(used float64) domain.QuotaSnapshot {
		val := used // fresh pointer each call: the record keeps it
		return domain.QuotaSnapshot{
			Credential: cred,
			Windows: []domain.QuotaWindow{
				// Label deliberately has no "账号 · " prefix: the DisplayLabel
				// is then also the short window name, so it is directly
				// asserted in the rendered card.
				{Name: "codex/rate_limit/primary_window", Label: "主额度窗口", Scope: domain.ScopeAccount, UsedPercent: &val, ResetAt: &reset},
				{Name: "codex/rate_limit/secondary_window", Label: "次要窗口", Scope: domain.ScopeAccount, UsedPercent: &secondary, ResetAt: &reset},
			},
			Source: domain.SourceCPA, Confidence: domain.ConfidenceReported,
			FetchedAt: now, LastSuccessAt: now, OK: true,
		}
	}

	prev := engineState()
	_, _, prev = eng.Evaluate(prev, []domain.QuotaSnapshot{snap(10)}, now)
	report, alerts, _ := eng.Evaluate(prev, []domain.QuotaSnapshot{snap(93.5)}, now.Add(time.Minute))

	a, ok := findAlertKind(alerts, domain.AlertQuotaThreshold)
	if !ok {
		t.Fatalf("no threshold alert from a 90→93.5 crossing: %+v", kindsOf(alerts))
	}
	// The engine must name the window in a fact so the renderer can find it.
	if len(a.Facts) == 0 || a.Facts[0] != "窗口: 主额度窗口" {
		t.Fatalf("alert does not lead with the window fact: %+v", a.Facts)
	}

	msg := domain.Message{Kind: string(domain.AlertQuotaThreshold), Alerts: alerts, Report: &report, Freshness: "实时"}
	r := New(config.ToneCasual).WithLocation(loc)
	card := r.Card(msg)
	raw, _ := json.Marshal(card)
	text := string(raw)

	// Severity is Info (a notice crossing) yet the header follows the KIND, so
	// a threshold alert is orange even at the quietest severity.
	if tmpl := card["header"].(map[string]any)["template"]; tmpl != "orange" {
		t.Errorf("threshold alert at Info severity header = %v, want orange", tmpl)
	}
	// The named window, its remaining share and its refresh time lead the card.
	for _, want := range []string{"主额度窗口", "剩 ", "10-09 18:00", "刷新"} {
		if !strings.Contains(text, want) {
			t.Errorf("threshold card missing %q:\n%s", want, text)
		}
	}
	// The unnamed second window must NOT appear in the alert row itself: the
	// window fact matched, so the row shows only the alert's window. (The
	// folded detail panel legitimately lists every window.)
	if strings.Contains(alertRowMarkdown(t, card), "次要窗口") {
		t.Errorf("alert row fell back to listing all windows:\n%s", alertRowMarkdown(t, card))
	}
	if strings.Contains(text, "****") {
		t.Errorf("card leaked a masked id:\n%s", text)
	}

	// A confirmed threshold at Warn severity does carry the conclusion, with
	// the provider · alias and the switch target.
	_, warnAlerts, _ := eng.Evaluate(engineState(), []domain.QuotaSnapshot{snap(96)}, now)
	warnMsg := domain.Message{Kind: string(domain.AlertQuotaThreshold), Alerts: warnAlerts, Report: &report}
	warnText := renderText(t, New(config.ToneCasual).WithLocation(loc), warnMsg)
	if !strings.Contains(warnText, "**结论：** Codex · main 的 主额度窗口 快用完了") {
		t.Errorf("warn threshold conclusion missing account/window:\n%s", warnText)
	}
}

// TestEngineResetCardHeaderIsGreen renders a real quota-reset alert (a sharp
// drop with a prior notice) and pins the green header and the refreshed window.
func TestEngineResetCardHeaderIsGreen(t *testing.T) {
	loc := time.UTC
	eng := evaluate.New(engineTestConfig(), calendar.New(loc))
	cred := domain.Credential{Key: "cx1", Provider: domain.ProviderCodex, Alias: "codex-main", ShortID: "cb31"}
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, loc)
	reset := time.Date(2026, 10, 10, 9, 0, 0, 0, loc)
	snap := func(v float64) domain.QuotaSnapshot {
		val := v // fresh pointer each call: the persisted record keeps it
		return domain.QuotaSnapshot{
			Credential: cred,
			Windows: []domain.QuotaWindow{{
				Name: "codex/rate_limit/primary_window", Label: "主额度窗口",
				Scope: domain.ScopeAccount, UsedPercent: &val, ResetAt: &reset,
			}},
			Source: domain.SourceCPA, Confidence: domain.ConfidenceReported,
			FetchedAt: now, LastSuccessAt: now, OK: true,
		}
	}
	prev := engineState()
	_, _, prev = eng.Evaluate(prev, []domain.QuotaSnapshot{snap(99)}, now)
	report, alerts, _ := eng.Evaluate(prev, []domain.QuotaSnapshot{snap(5)}, now.Add(time.Minute))

	if _, ok := findAlertKind(alerts, domain.AlertQuotaReset); !ok {
		t.Fatalf("no reset alert from a 99→5 drop: %+v", kindsOf(alerts))
	}
	msg := domain.Message{Kind: string(domain.AlertQuotaReset), Alerts: alerts, Report: &report, Freshness: "实时"}
	r := New(config.ToneCasual).WithLocation(loc)
	card := r.Card(msg)
	if tmpl := card["header"].(map[string]any)["template"]; tmpl != "green" {
		t.Errorf("reset alert header = %v, want green", tmpl)
	}
	text := jsonText2(t, card)
	for _, want := range []string{"主额度窗口", "95.0%", "10-10 09:00", "刷新"} {
		if !strings.Contains(text, want) {
			t.Errorf("reset card missing %q:\n%s", want, text)
		}
	}
}

// renderText marshals a rendered text reply for substring assertions.
func renderText(t *testing.T, r *Renderer, msg domain.Message) string {
	t.Helper()
	b, err := json.Marshal(r.Card(msg))
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	return string(b)
}

// alertRowMarkdown is the text of every interactive_container (the alert rows),
// excluding the folded detail panel, so a fallback-to-all-windows regression is
// visible in the row a reader actually sees.
func alertRowMarkdown(t *testing.T, card map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, e := range findElements(card, "interactive_container") {
		b.WriteString(markdownText(e))
	}
	return b.String()
}

// markdownText collects every markdown content nested under one element.
func markdownText(v any) string {
	var out string
	switch x := v.(type) {
	case map[string]any:
		if x["tag"] == "markdown" {
			if c, ok := x["content"].(string); ok {
				out += c + "\n"
			}
		}
		for _, child := range x {
			out += markdownText(child)
		}
	case []any:
		for _, child := range x {
			out += markdownText(child)
		}
	}
	return out
}

func findAlertKind(alerts []domain.Alert, k domain.AlertKind) (domain.Alert, bool) {
	for _, a := range alerts {
		if a.Kind == k {
			return a, true
		}
	}
	return domain.Alert{}, false
}

func kindsOf(alerts []domain.Alert) []domain.AlertKind {
	out := make([]domain.AlertKind, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, a.Kind)
	}
	return out
}

// cardTitle reads the header title off a rendered card.
func cardTitle(t *testing.T, card map[string]any) string {
	t.Helper()
	h, _ := card["header"].(map[string]any)
	title, _ := h["title"].(map[string]any)
	s, _ := title["content"].(string)
	return s
}

// TestClearedCollapsesToOneRow pins the "额度恢复" intent: an account that CPA
// paused and then released is ONE change, however many model scopes moved with
// it. Five cleared alerts (one account-scope, four model-scope) must render one
// row, titled 额度恢复, with no 429 and no "限流解除".
func TestClearedCollapsesToOneRow(t *testing.T) {
	loc := time.UTC
	gen := time.Date(2026, 10, 9, 9, 0, 0, 0, loc)
	cred := domain.Credential{Key: "cx1", Provider: domain.ProviderCodex, Alias: "codex-main", ShortID: "cb31"}

	cleared := func(scope domain.QuotaScope, id string) domain.Alert {
		return domain.Alert{
			Kind: domain.AlertRateLimitCleared, Severity: domain.SeverityInfo, Evidence: domain.EvidenceConfirmed,
			Credential: cred, Scope: scope, ScopeID: id, OccurredAt: gen,
			Facts: []string{"状态码: 429", "持续: 17m"},
		}
	}
	alerts := []domain.Alert{
		cleared(domain.ScopeAccount, ""),
		cleared(domain.ScopeModel, "claude-sonnet-4-5"),
		cleared(domain.ScopeModel, "gpt-5"),
		cleared(domain.ScopeModel, "gemini-2.5-pro"),
		cleared(domain.ScopeModel, "fable"),
	}
	msg := domain.Message{Kind: string(domain.AlertRateLimitCleared), Alerts: alerts}
	r := New(config.ToneCasual).WithLocation(loc)
	card := r.Card(msg)
	text := jsonText2(t, card)

	if got := cardTitle(t, card); got != "额度恢复" {
		t.Errorf("cleared card title = %q, want 额度恢复", got)
	}
	if n := len(findElements(card, "interactive_container")); n != 1 {
		t.Errorf("cleared card rows = %d, want 1 (collapsed by credential+kind)", n)
	}
	if strings.Contains(text, "429") {
		t.Errorf("cleared card must not show 429 anywhere:\n%s", text)
	}
	if strings.Contains(text, "限流解除") {
		t.Errorf("cleared card must not say 限流解除:\n%s", text)
	}
	rows := alertRowMarkdown(t, card)
	for _, want := range []string{"已恢复", "停用 17 分钟", "main"} {
		if !strings.Contains(rows, want) {
			t.Errorf("cleared row missing %q:\n%s", want, rows)
		}
	}
	// A single collapsed account reads as one change, not "N 条变化".
	if strings.Contains(text, "条变化") {
		t.Errorf("collapsed single-account card should not count raw alerts:\n%s", text)
	}
	if !strings.Contains(text, "**结论：** Codex · main 已恢复") {
		t.Errorf("cleared conclusion missing trimmed account:\n%s", text)
	}
}

// TestClearedDifferentAccountsStaySeparate: two credentials are two changes,
// and the subtitle/conclusion count rows after collapsing, not raw alerts.
func TestClearedDifferentAccountsStaySeparate(t *testing.T) {
	loc := time.UTC
	gen := time.Date(2026, 10, 9, 9, 0, 0, 0, loc)
	a := domain.Credential{Key: "cx1", Provider: domain.ProviderCodex, Alias: "codex-main", ShortID: "cb31"}
	b := domain.Credential{Key: "cx2", Provider: domain.ProviderCodex, Alias: "codex-backup", ShortID: "e1ae"}
	cleared := func(c domain.Credential) domain.Alert {
		return domain.Alert{
			Kind: domain.AlertRateLimitCleared, Severity: domain.SeverityInfo, Evidence: domain.EvidenceConfirmed,
			Credential: c, Scope: domain.ScopeAccount, OccurredAt: gen,
			Facts: []string{"状态码: 429", "持续: 5m"},
		}
	}
	// Two model-scope alerts for the first account must still collapse to one.
	alerts := []domain.Alert{
		cleared(a),
		func() domain.Alert {
			m := cleared(a)
			m.Scope, m.ScopeID = domain.ScopeModel, "gpt-5"
			return m
		}(),
		cleared(b),
	}
	msg := domain.Message{Kind: string(domain.AlertRateLimitCleared), Alerts: alerts}
	r := New(config.ToneCasual).WithLocation(loc)
	card := r.Card(msg)
	text := jsonText2(t, card)

	if n := len(findElements(card, "interactive_container")); n != 2 {
		t.Errorf("two credentials should render 2 rows, got %d", n)
	}
	if !strings.Contains(text, `"content":"2 条变化"`) {
		t.Errorf("subtitle should count collapsed rows:\n%s", text)
	}
	if !strings.Contains(text, "**结论：** 2 个账号有变化，详见下方") {
		t.Errorf("conclusion should count collapsed rows:\n%s", text)
	}
	rows := alertRowMarkdown(t, card)
	if !strings.Contains(rows, "`main`") || !strings.Contains(rows, "`backup`") {
		t.Errorf("both trimmed aliases should appear:\n%s", rows)
	}
}

// TestModelOnlyCollapseCountsScopes: when a credential has only model-scope
// alerts for one kind, they collapse to one row that says how many models moved.
func TestModelOnlyCollapseCountsScopes(t *testing.T) {
	loc := time.UTC
	gen := time.Date(2026, 10, 9, 9, 0, 0, 0, loc)
	cred := domain.Credential{Key: "cx1", Provider: domain.ProviderCodex, Alias: "codex-main"}
	th := func(id string) domain.Alert {
		return domain.Alert{
			Kind: domain.AlertQuotaThreshold, Severity: domain.SeverityWarn, Evidence: domain.EvidenceConfirmed,
			Credential: cred, Scope: domain.ScopeModel, ScopeID: id, OccurredAt: gen,
		}
	}
	msg := domain.Message{Kind: string(domain.AlertQuotaThreshold), Alerts: []domain.Alert{th("a"), th("b"), th("c")}}
	r := New(config.ToneCasual).WithLocation(loc)
	card := r.Card(msg)
	rows := alertRowMarkdown(t, card)
	if n := len(findElements(card, "interactive_container")); n != 1 {
		t.Errorf("model-only alerts should collapse to 1 row, got %d", n)
	}
	if !strings.Contains(rows, "含 3 个模型") {
		t.Errorf("collapsed model-only row should count the scopes:\n%s", rows)
	}
}

// TestSameCredentialTwoThresholdWindowsBothVisibleUnexpanded: distinct quota
// windows (e.g. 5h and 7d) for the same credential must survive as separate
// unexpanded interactive rows instead of being merged by key+kind.
func TestSameCredentialTwoThresholdWindowsBothVisibleUnexpanded(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, loc)
	reset5h := now.Add(2 * time.Hour)
	reset7d := now.Add(4 * 24 * time.Hour)
	cred := domain.Credential{Key: "cx1", Provider: domain.ProviderClaude, Alias: "claude-main", ShortID: "cb31"}
	u5h := 94.0
	u7d := 91.0
	snap := domain.QuotaSnapshot{
		Credential: cred,
		Windows: []domain.QuotaWindow{
			{Name: "claude/five_hour", Label: "5小时", Scope: domain.ScopeAccount, UsedPercent: &u5h, ResetAt: &reset5h},
			{Name: "claude/seven_day", Label: "7天", Scope: domain.ScopeAccount, UsedPercent: &u7d, ResetAt: &reset7d},
		},
		Source: domain.SourceCPA, Confidence: domain.ConfidenceReported,
		FetchedAt: now, LastSuccessAt: now, OK: true,
	}
	rep := &domain.Report{
		GeneratedAt: now,
		Providers: []domain.ProviderReport{
			{
				Provider:  domain.ProviderClaude,
				Total:     1,
				Snapshots: []domain.QuotaSnapshot{snap},
			},
		},
	}
	alerts := []domain.Alert{
		{
			Kind: domain.AlertQuotaThreshold, Severity: domain.SeverityWarn, Evidence: domain.EvidenceConfirmed,
			Credential: cred, Scope: domain.ScopeAccount, OccurredAt: now,
			Facts: []string{"窗口: 5小时"},
		},
		{
			Kind: domain.AlertQuotaThreshold, Severity: domain.SeverityInfo, Evidence: domain.EvidenceConfirmed,
			Credential: cred, Scope: domain.ScopeAccount, OccurredAt: now,
			Facts: []string{"窗口: 7天"},
		},
	}
	msg := domain.Message{Kind: string(domain.AlertQuotaThreshold), Alerts: alerts, Report: rep}
	r := New(config.ToneCasual).WithLocation(loc)
	card := r.Card(msg)

	rows := findElements(card, "interactive_container")
	if len(rows) != 2 {
		t.Fatalf("expected 2 unexpanded rows for distinct windows, got %d", len(rows))
	}
	rowText := alertRowMarkdown(t, card)
	if !strings.Contains(rowText, "5h") || !strings.Contains(rowText, "6.0%") {
		t.Errorf("5h window not found in rows:\n%s", rowText)
	}
	if !strings.Contains(rowText, "7d") || !strings.Contains(rowText, "9.0%") {
		t.Errorf("7d window not found in rows:\n%s", rowText)
	}
}

// TestRecoveredActualEngineStyleNoWindowFact: real evaluate engine produces
// AlertRecovered without a window fact; card must report status recovery without
// fabricating arbitrary 5h quota-refresh claims.
func TestRecoveredActualEngineStyleNoWindowFact(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, loc)
	reset := now.Add(2 * time.Hour)
	cred := domain.Credential{Key: "cx1", Provider: domain.ProviderClaude, Alias: "claude-main", ShortID: "cb31"}
	u5h := 10.0
	snap := domain.QuotaSnapshot{
		Credential: cred,
		Windows: []domain.QuotaWindow{
			{Name: "claude/five_hour", Label: "5小时", Scope: domain.ScopeAccount, UsedPercent: &u5h, ResetAt: &reset},
		},
		Source: domain.SourceCPA, Confidence: domain.ConfidenceReported,
		FetchedAt: now, LastSuccessAt: now, OK: true,
	}
	rep := &domain.Report{
		GeneratedAt: now,
		Providers: []domain.ProviderReport{
			{
				Provider:  domain.ProviderClaude,
				Total:     1,
				Snapshots: []domain.QuotaSnapshot{snap},
			},
		},
	}
	alert := domain.Alert{
		Kind:       domain.AlertRecovered,
		Severity:   domain.SeverityInfo,
		Evidence:   domain.EvidenceConfirmed,
		Credential: cred,
		Title:      "观测状态已恢复",
		Detail:     "Claude main 已恢复以下观测：额度读取。",
		OccurredAt: now,
	}
	msg := domain.Message{Kind: string(domain.AlertRecovered), Alerts: []domain.Alert{alert}, Report: rep}
	r := New(config.ToneCasual).WithLocation(loc)
	card := r.Card(msg)
	text := jsonText2(t, card)

	if strings.Contains(text, "已刷新") {
		t.Errorf("recovered card fabricated a quota-refresh claim:\n%s", text)
	}
	if strings.Contains(text, "5小时 ·") || strings.Contains(text, "5h ·") {
		t.Errorf("recovered card reported arbitrary window reset:\n%s", text)
	}
	rowText := alertRowMarkdown(t, card)
	if !strings.Contains(rowText, "已恢复") {
		t.Errorf("recovered row missing status recovery text:\n%s", rowText)
	}
}

// TestCooldownLimitedCollapsesToOneRow: cooldown episodes crossing account
// and model scopes must collapse to one account row.
func TestCooldownLimitedCollapsesToOneRow(t *testing.T) {
	loc := time.UTC
	gen := time.Date(2026, 10, 9, 9, 0, 0, 0, loc)
	cred := domain.Credential{Key: "cx1", Provider: domain.ProviderCodex, Alias: "codex-main", ShortID: "cb31"}
	far := gen.Add(2 * time.Hour).UTC().Format(time.RFC3339)

	limited := func(scope domain.QuotaScope, id string) domain.Alert {
		return domain.Alert{
			Kind: domain.AlertRateLimited, Severity: domain.SeverityWarn, Evidence: domain.EvidenceConfirmed,
			Credential: cred, Scope: scope, ScopeID: id, OccurredAt: gen,
			Facts: []string{"状态码: 429", "恢复时间: " + far, "持续: 0m"},
		}
	}
	alerts := []domain.Alert{
		limited(domain.ScopeAccount, ""),
		limited(domain.ScopeModel, "gpt-5"),
		limited(domain.ScopeModel, "claude-sonnet-4-5"),
	}
	msg := domain.Message{Kind: string(domain.AlertRateLimited), Alerts: alerts}
	r := New(config.ToneCasual).WithLocation(loc)
	card := r.Card(msg)

	if n := len(findElements(card, "interactive_container")); n != 1 {
		t.Errorf("cooldown limited alerts should collapse to 1 row, got %d", n)
	}
}

// TestOptionBAlertDualWindows asserts that an alert card for a credential with BOTH
// 5h and 7d in the same snapshot renders Option B (dual window facts with triggering window
// visually emphasized) while never mixing accounts or scopes.
func TestOptionBAlertDualWindows(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, loc)
	reset5h := now.Add(2 * time.Hour)
	reset7d := now.Add(4 * 24 * time.Hour)
	cred := domain.Credential{Key: "cx1", Provider: domain.ProviderClaude, Alias: "claude-External0.2", ShortID: "cb31"}
	u5h := 93.5 // remaining 6.5% (triggers notice/threshold)
	u7d := 60.0 // remaining 40.0%

	snap := domain.QuotaSnapshot{
		Credential: cred,
		Windows: []domain.QuotaWindow{
			{Name: "claude/five_hour", Label: "5小时", Scope: domain.ScopeAccount, UsedPercent: &u5h, ResetAt: &reset5h},
			{Name: "claude/seven_day", Label: "7天", Scope: domain.ScopeAccount, UsedPercent: &u7d, ResetAt: &reset7d},
		},
		Source: domain.SourceCPA, Confidence: domain.ConfidenceReported,
		FetchedAt: now, LastSuccessAt: now, OK: true,
	}
	rep := &domain.Report{
		GeneratedAt: now,
		Providers: []domain.ProviderReport{
			{
				Provider:  domain.ProviderClaude,
				Total:     1,
				Snapshots: []domain.QuotaSnapshot{snap},
			},
		},
	}
	alert := domain.Alert{
		Kind: domain.AlertQuotaThreshold, Severity: domain.SeverityWarn, Evidence: domain.EvidenceConfirmed,
		Credential: cred, Scope: domain.ScopeAccount, OccurredAt: now,
		Facts: []string{"窗口: 5小时"},
	}
	msg := domain.Message{Kind: string(domain.AlertQuotaThreshold), Alerts: []domain.Alert{alert}, Report: rep}
	r := New(config.ToneCasual).WithLocation(loc)

	// 1. Full card
	card := r.Card(msg)
	rowText := alertRowMarkdown(t, card)

	// Must assert structure contains the paired column_set (not just text) with flex_mode: flow
	pairedSets := findElements(card, "column_set")
	var foundDualSet bool
	for _, cs := range pairedSets {
		if fm, ok := cs["flex_mode"].(string); ok && fm == "flow" {
			if cols, ok := cs["columns"].([]any); ok && len(cols) == 2 {
				foundDualSet = true
				break
			}
		}
	}
	if !foundDualSet {
		t.Fatalf("expected Option B paired column_set with flex_mode: flow and 2 columns, not found in card:\n%s", jsonText2(t, card))
	}

	// Must show both 5h (triggering) and 7d (secondary) facts
	if !strings.Contains(rowText, "5h") || !strings.Contains(rowText, "6.5%") {
		t.Errorf("expected triggering 5h window fact in row, got:\n%s", rowText)
	}
	if !strings.Contains(rowText, "7d") || !strings.Contains(rowText, "40.0%") {
		t.Errorf("expected secondary 7d window fact in row, got:\n%s", rowText)
	}
	if !strings.Contains(rowText, "触发") {
		t.Errorf("expected triggering tag pill in row, got:\n%s", rowText)
	}
	if !strings.Contains(rowText, "10-09 11:00 刷新") || !strings.Contains(rowText, "10-13 09:00 刷新") {
		t.Errorf("expected refresh times for both 5h and 7d windows in row, got:\n%s", rowText)
	}

	// Check total card components <= 200 budget
	body := card["body"].(map[string]any)
	cardEls := body["elements"].([]any)
	if total := countElements(cardEls); total > 200 {
		t.Errorf("card element count %d exceeds Feishu limit 200", total)
	}

	// 2. Fallback simple card
	cardSimple := r.CardSimple(msg)
	for _, risky := range []string{"interactive_container", "collapsible_panel", "chart"} {
		if n := len(findElements(cardSimple, risky)); n != 0 {
			t.Errorf("CardSimple must not contain %s, found %d", risky, n)
		}
	}
	simpleJSON := jsonText2(t, cardSimple)
	if !strings.Contains(simpleJSON, "6.5%") || !strings.Contains(simpleJSON, "40.0%") {
		t.Errorf("CardSimple must preserve dual window facts:\n%s", simpleJSON)
	}
	// CardSimple must also preserve the side-by-side column_set with flow
	var simpleDualSet bool
	for _, cs := range findElements(cardSimple, "column_set") {
		if fm, ok := cs["flex_mode"].(string); ok && fm == "flow" {
			if cols, ok := cs["columns"].([]any); ok && len(cols) == 2 {
				simpleDualSet = true
				break
			}
		}
	}
	if !simpleDualSet {
		t.Errorf("CardSimple must also preserve dual column_set with flex_mode: flow:\n%s", simpleJSON)
	}
	simpleBody := cardSimple["body"].(map[string]any)
	simpleEls := simpleBody["elements"].([]any)
	if total := countElements(simpleEls); total > 200 {
		t.Errorf("CardSimple element count %d exceeds Feishu limit 200", total)
	}

	// 3. Different scope isolation: Gemini group window must NOT pair with Claude/GPT group window
	gemGroup := "group-gemini"
	cgGroup := "group-claudegpt"
	uGem5h := 91.0
	uCG7d := 50.0
	mixedSnap := domain.QuotaSnapshot{
		Credential: cred,
		Windows: []domain.QuotaWindow{
			{Name: "gem/5h", Label: "Gemini 5h", Scope: domain.ScopeGroup, ScopeID: gemGroup, UsedPercent: &uGem5h, ResetAt: &reset5h},
			{Name: "cg/7d", Label: "Claude/GPT 7d", Scope: domain.ScopeGroup, ScopeID: cgGroup, UsedPercent: &uCG7d, ResetAt: &reset7d},
		},
		Source: domain.SourceCPA, Confidence: domain.ConfidenceReported,
		FetchedAt: now, LastSuccessAt: now, OK: true,
	}
	mixedRep := &domain.Report{
		GeneratedAt: now,
		Providers: []domain.ProviderReport{
			{Provider: domain.ProviderClaude, Total: 1, Snapshots: []domain.QuotaSnapshot{mixedSnap}},
		},
	}
	gemAlert := domain.Alert{
		Kind: domain.AlertQuotaThreshold, Severity: domain.SeverityWarn, Evidence: domain.EvidenceConfirmed,
		Credential: cred, Scope: domain.ScopeGroup, ScopeID: gemGroup, OccurredAt: now,
		Facts: []string{"窗口: Gemini 5h"},
	}
	gemCard := r.Card(domain.Message{Kind: string(domain.AlertQuotaThreshold), Alerts: []domain.Alert{gemAlert}, Report: mixedRep})
	gemRow := alertRowMarkdown(t, gemCard)
	// Because cg/7d is in a different group, they must NOT pair as companion windows
	if strings.Contains(gemRow, "Claude/GPT") {
		t.Errorf("different scope groups must not be paired in single Option B row:\n%s", gemRow)
	}

	// 4. Single-window only: Codex with only 7d window
	codexSnap := domain.QuotaSnapshot{
		Credential: domain.Credential{Key: "cx2", Provider: domain.ProviderCodex, Alias: "codex-sub"},
		Windows: []domain.QuotaWindow{
			{Name: "codex/7d", Label: "7天", Scope: domain.ScopeAccount, UsedPercent: &u7d, ResetAt: &reset7d},
		},
		Source: domain.SourceCPA, Confidence: domain.ConfidenceReported,
		FetchedAt: now, LastSuccessAt: now, OK: true,
	}
	codexRep := &domain.Report{
		GeneratedAt: now,
		Providers: []domain.ProviderReport{
			{Provider: domain.ProviderCodex, Total: 1, Snapshots: []domain.QuotaSnapshot{codexSnap}},
		},
	}
	codexAlert := domain.Alert{
		Kind: domain.AlertQuotaThreshold, Severity: domain.SeverityWarn, Evidence: domain.EvidenceConfirmed,
		Credential: codexSnap.Credential, Scope: domain.ScopeAccount, OccurredAt: now,
		Facts: []string{"窗口: 7天"},
	}
	codexCard := r.Card(domain.Message{Kind: string(domain.AlertQuotaThreshold), Alerts: []domain.Alert{codexAlert}, Report: codexRep})
	codexText := jsonText2(t, codexCard)
	if strings.Contains(codexText, "5h") || strings.Contains(codexText, "5小时") {
		t.Errorf("single window alert must not fabricate 5h window:\n%s", codexText)
	}

	// 5. Real 5h window with nil UsedPercent: companion to triggering 7d window
	nil5hSnap := domain.QuotaSnapshot{
		Credential: domain.Credential{Key: "cx3", Provider: domain.ProviderClaude, Alias: "claude-nil5h"},
		Windows: []domain.QuotaWindow{
			{Name: "claude/5h", Label: "5小时", Scope: domain.ScopeAccount, UsedPercent: nil},
			{Name: "claude/7d", Label: "7天", Scope: domain.ScopeAccount, UsedPercent: &u7d, ResetAt: &reset7d},
		},
		Source: domain.SourceCPA, Confidence: domain.ConfidenceReported,
		FetchedAt: now, LastSuccessAt: now, OK: true,
	}
	nil5hRep := &domain.Report{
		GeneratedAt: now,
		Providers: []domain.ProviderReport{
			{Provider: domain.ProviderClaude, Total: 1, Snapshots: []domain.QuotaSnapshot{nil5hSnap}},
		},
	}
	nil5hAlert := domain.Alert{
		Kind: domain.AlertQuotaThreshold, Severity: domain.SeverityWarn, Evidence: domain.EvidenceConfirmed,
		Credential: nil5hSnap.Credential, Scope: domain.ScopeAccount, OccurredAt: now,
		Facts: []string{"窗口: 7天"},
	}
	nil5hCard := r.Card(domain.Message{Kind: string(domain.AlertQuotaThreshold), Alerts: []domain.Alert{nil5hAlert}, Report: nil5hRep})
	nil5hText := jsonText2(t, nil5hCard)
	if !strings.Contains(nil5hText, "5h") || !strings.Contains(nil5hText, "未上报") {
		t.Errorf("real 5h window with nil UsedPercent must show 5h and 未上报:\n%s", nil5hText)
	}
}


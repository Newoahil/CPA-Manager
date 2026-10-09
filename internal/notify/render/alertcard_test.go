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

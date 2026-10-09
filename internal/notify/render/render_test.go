package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
)

const (
	secretCookie = "SUPERSECRET_COOKIE_abc123"
	secretToken  = "Bearer sk-live-xyz789"
	secretMgmt   = "mgmt-key-999888"
)

func fixture() domain.Message {
	used92 := 92.5
	reset := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	fetched := time.Date(2026, 3, 13, 17, 0, 0, 0, time.UTC)

	codexCred := domain.Credential{Key: "codex-1", Provider: domain.ProviderCodex, Alias: "主号", ShortID: "a1b2"}
	ollamaCred := domain.Credential{Key: "ollama-1", Provider: domain.ProviderOllama, Alias: "备用", ShortID: "z9y8"}

	codexSnap := domain.QuotaSnapshot{
		Credential: codexCred,
		Windows: []domain.QuotaWindow{
			// Account scope: a real Codex rate-limit window declares it, and
			// without it the renderer correctly refuses to grade the channel.
			{Name: "5h", Scope: domain.ScopeAccount, UsedPercent: &used92, ResetAt: &reset},
		},
		Source:        domain.SourceCPA,
		Confidence:    domain.ConfidenceReported,
		FetchedAt:     fetched,
		LastSuccessAt: fetched,
		OK:            true,
		// A failure reason must never be rendered verbatim into a card or text.
		Err: "upstream said " + secretToken,
	}
	ollamaSnap := domain.QuotaSnapshot{
		Credential:    ollamaCred,
		Windows:       []domain.QuotaWindow{{Name: "5h"}},
		Source:        domain.SourceOllamaWeb,
		Confidence:    domain.ConfidenceUnknown,
		FetchedAt:     fetched,
		LastSuccessAt: fetched,
		OK:            true,
	}

	report := domain.Report{
		GeneratedAt: fetched,
		Providers: []domain.ProviderReport{
			{
				Provider:   domain.ProviderCodex,
				Healthy:    1,
				Total:      2,
				WorstState: domain.StateWarning,
				BestWindows: []domain.QuotaWindow{
					{Name: "5h", Scope: domain.ScopeAccount, UsedPercent: &used92, ResetAt: &reset},
				},
				Snapshots: []domain.QuotaSnapshot{codexSnap},
			},
			{
				Provider:    domain.ProviderOllama,
				Healthy:     1,
				Total:       1,
				WorstState:  domain.StateHealthy,
				BestWindows: []domain.QuotaWindow{{Name: "5h"}}, // UsedPercent nil
				Snapshots:   []domain.QuotaSnapshot{ollamaSnap},
			},
		},
		Recommendations: []domain.Recommendation{
			{Provider: domain.ProviderCodex, Direction: domain.DirectionEaseOff, Reason: "接近阈值"},
			{Provider: domain.ProviderOllama, Direction: domain.DirectionUseMore, Reason: "余量充足"},
		},
	}

	alerts := []domain.Alert{
		{
			Kind:       domain.AlertQuotaThreshold,
			Severity:   domain.SeverityWarn,
			Evidence:   domain.EvidenceConfirmed,
			Credential: codexCred,
			Title:      "Codex 5h 窗口接近上限",
			Detail:     "已用 92.5%，阈值 90%。",
			Facts:      []string{"5h 窗口已用 92.5%（阈值 90%）", "数据来源 cpa-v8，获取 2026-03-13 17:00"},
			Advice:     "切换其它 Codex 凭证，或等 5h 窗口刷新。",
			OccurredAt: fetched,
		},
		{
			Kind:       domain.AlertCredential,
			Severity:   domain.SeverityUrgent,
			Evidence:   domain.EvidenceSuspected,
			Credential: ollamaCred,
			Title:      "Ollama 凭证疑似失效",
			Detail:     "连续两次采集失败。",
			Facts:      []string{"连续失败 2 次", "数据来源 ollama-web，获取 2026-03-13 17:00"},
			Advice:     "让该凭证的负责人重新登录 Ollama Cloud。",
			OccurredAt: fetched,
		},
	}

	return domain.Message{
		Title:  "额度日报",
		Kind:   "daily",
		Report: &report,
		Alerts: alerts,
	}
}

func TestBothTonesKeepEvidence(t *testing.T) {
	msg := fixture()
	wantSubstrings := []string{
		"92.5%",       // exact number
		"5h",          // window name
		"03-14 09:00", // reset time
		"cpa-v8",      // data source
		"确认",          // confirmed evidence
		"疑似",          // suspected evidence
		"2026-03-13 17:00",
	}
	for _, tone := range []string{config.ToneCasual, config.ToneFormal} {
		r := New(tone).WithLocation(time.UTC)
		text := r.Text(msg)
		for _, want := range wantSubstrings {
			if !strings.Contains(text, want) {
				t.Errorf("tone %s: text missing %q\n---\n%s", tone, want, text)
			}
		}
		card, err := json.Marshal(r.Card(msg))
		if err != nil {
			t.Fatalf("card marshal: %v", err)
		}
		cardStr := string(card)
		// The card speaks the remaining caliber: 92.5% used is 7.5% remaining.
		// An alert card keeps the decisive facts — window, remaining share,
		// reset time, data time — and a suspected diagnosis stays labelled
		// "疑似". The data source and the confirmed-evidence label are kept in
		// the text channel; on the card they were noise around the one line
		// that mattered.
		for _, want := range []string{"7.5%", "5h", "03-14 09:00", "疑似", "2026-03-13 17:00"} {
			if !strings.Contains(cardStr, want) {
				t.Errorf("tone %s: card missing %q\n---\n%s", tone, want, cardStr)
			}
		}
	}
}

func TestTonesDiffer(t *testing.T) {
	msg := fixture()
	casual := New(config.ToneCasual).WithLocation(time.UTC).Text(msg)
	formal := New(config.ToneFormal).WithLocation(time.UTC).Text(msg)
	if casual == formal {
		t.Fatal("casual and formal output should differ")
	}
	if !strings.Contains(casual, "省着点用") {
		t.Errorf("casual conclusion missing team voice:\n%s", casual)
	}
	if !strings.Contains(formal, "用量偏高") {
		t.Errorf("formal conclusion missing neutral wording:\n%s", formal)
	}
	// Both tones keep the exact number that justifies the instruction.
	for _, out := range []string{casual, formal} {
		if !strings.Contains(out, "92.5%") {
			t.Errorf("conclusion dropped the number behind the advice:\n%s", out)
		}
	}
}

// TestConclusionCarriesNoDisclaimers: the verdict line answers "what do I do".
// Coverage limits and evidence caveats belong on the provider's own line;
// inside the verdict they make it unreadable without making it more true.
func TestConclusionCarriesNoDisclaimers(t *testing.T) {
	low := 20.0
	scoped := domain.QuotaWindow{Name: "g", Label: "Gemini Models · 周", Scope: domain.ScopeGroup, ScopeID: "grp", UsedPercent: &low}
	rep := domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderAntigravity, Total: 1,
		States: map[string]domain.CredentialState{"k": domain.StateLimited},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "k", Alias: "ag"}, OK: true,
			Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported,
			Windows: []domain.QuotaWindow{scoped},
		}},
	}}}
	out := New(config.ToneCasual).WithLocation(time.UTC).Text(domain.Message{Report: &rep})

	conclusion := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "结论：") {
			conclusion = line
		}
	}
	if conclusion == "" {
		t.Fatalf("no conclusion line:\n%s", out)
	}
	for _, banned := range []string{"未覆盖", "不代表", "scope", "覆盖：", "证据"} {
		if strings.Contains(conclusion, banned) {
			t.Errorf("conclusion carries a disclaimer %q: %q", banned, conclusion)
		}
	}
	if !strings.Contains(conclusion, "Antigravity") {
		t.Errorf("a channel with headroom on every observed scope must be recommended: %q", conclusion)
	}
	// ...and the caveat must still exist, on the provider's line.
	if !strings.Contains(out, "未覆盖全账号") {
		t.Errorf("coverage caveat disappeared entirely:\n%s", out)
	}
}

// exhaustedReport builds a provider whose credentials are all out of quota,
// each blocked by a different window.
func exhaustedReport() domain.Report {
	full := 100.0
	weekly := time.Date(2026, 10, 6, 4, 4, 0, 0, time.UTC)
	fiveHour := time.Date(2026, 9, 30, 13, 4, 0, 0, time.UTC)
	sooner := time.Date(2026, 10, 3, 17, 0, 0, 0, time.UTC)
	return domain.Report{
		GeneratedAt: time.Date(2026, 9, 30, 9, 48, 0, 0, time.UTC),
		Providers: []domain.ProviderReport{{
			Provider: domain.ProviderCodex, Total: 2,
			WorstState: domain.StateExhausted,
			States:     map[string]domain.CredentialState{"a": domain.StateExhausted, "b": domain.StateExhausted},
			Snapshots: []domain.QuotaSnapshot{
				{
					Credential: domain.Credential{Key: "a", Alias: "acct-a"}, OK: true,
					Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported,
					Windows: []domain.QuotaWindow{
						{Name: "w", Label: "账号 · 周窗口", Scope: domain.ScopeAccount, UsedPercent: &full, ResetAt: &weekly},
						{Name: "h", Label: "账号 · 5小时窗口", Scope: domain.ScopeAccount, UsedPercent: &full, ResetAt: &fiveHour},
					},
				},
				{
					Credential: domain.Credential{Key: "b", Alias: "acct-b"}, OK: true,
					Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported,
					Windows: []domain.QuotaWindow{
						{Name: "w", Label: "账号 · 周窗口", Scope: domain.ScopeAccount, UsedPercent: &full, ResetAt: &sooner},
					},
				},
			},
		}},
	}
}

// TestExhaustedConclusionStatesRecoveryTime: "don't use this" is only half an
// answer. The reader also needs to know when it comes back.
//
// A credential is usable again once its LAST blocking window has reset, and the
// channel is usable again once its FIRST credential is: acct-a is blocked until
// 10-06 04:04 (not 09-30 13:04, its other window), acct-b until 10-03 17:00, so
// the channel recovers at 10-03 17:00.
func TestExhaustedConclusionStatesRecoveryTime(t *testing.T) {
	rep := exhaustedReport()
	out := New(config.ToneCasual).WithLocation(time.UTC).Text(domain.Message{Report: &rep})
	if !strings.Contains(out, "最早 10-03 17:00 恢复") {
		t.Errorf("conclusion missing recovery time:\n%s", out)
	}
	if strings.Contains(out, "最早 09-30 13:04 恢复") {
		t.Errorf("recovery time taken from a non-blocking window:\n%s", out)
	}
	if !strings.Contains(out, "先别用了") {
		t.Errorf("conclusion does not say what to do:\n%s", out)
	}
	// Nothing else is usable, so there is no "switch to" clause to invent.
	if strings.Contains(out, "改用") {
		t.Errorf("invented an alternative that does not exist:\n%s", out)
	}
}

// TestEveryTimestampUsesTheConfiguredZone is the regression for the
// eight-hour disagreement seen in production: the conclusion converted its
// recovery time into the configured zone while the detail line printed the
// upstream's own wall clock, so the same reset appeared as both "10-04 01:00"
// and "10-03 17:00" in one message.
func TestEveryTimestampUsesTheConfiguredZone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	// 2026-10-03T17:00Z is 2026-10-04 01:00 in Asia/Shanghai.
	reset := time.Date(2026, 10, 3, 17, 0, 0, 0, time.UTC)
	full := 100.0
	rep := domain.Report{
		GeneratedAt: time.Date(2026, 9, 30, 2, 39, 0, 0, time.UTC), // 10:39 +08
		Providers: []domain.ProviderReport{{
			Provider: domain.ProviderCodex, Total: 1,
			WorstState: domain.StateExhausted,
			States:     map[string]domain.CredentialState{"k": domain.StateExhausted},
			Snapshots: []domain.QuotaSnapshot{{
				Credential: domain.Credential{Key: "k", Alias: "acct"}, OK: true,
				Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported,
				LastSuccessAt: time.Date(2026, 9, 30, 2, 39, 0, 0, time.UTC),
				Windows: []domain.QuotaWindow{{
					Name: "w", Label: "账号 · 周窗口", Scope: domain.ScopeAccount,
					UsedPercent: &full, ResetAt: &reset,
				}},
			}},
			BestWindows: []domain.QuotaWindow{{
				Name: "w", Label: "账号 · 周窗口", Scope: domain.ScopeAccount,
				UsedPercent: &full, ResetAt: &reset,
			}},
		}},
	}
	r := New(config.ToneCasual).WithLocation(loc)
	msg := domain.Message{Report: &rep, Detailed: true}

	card, err := json.Marshal(r.Card(msg))
	if err != nil {
		t.Fatalf("card marshal: %v", err)
	}
	for name, out := range map[string]string{"text": r.Text(msg), "card": string(card)} {
		// The conclusion's recovery time and the detail's reset time are the
		// same instant and must read identically.
		if !strings.Contains(out, "最早 10-04 01:00 恢复") {
			t.Errorf("%s: conclusion not in the configured zone:\n%s", name, out)
		}
		if !strings.Contains(out, "10-04 01:00 刷新") {
			t.Errorf("%s: window reset not in the configured zone:\n%s", name, out)
		}
		if strings.Contains(out, "10-03 17:00") {
			t.Errorf("%s: raw UTC wall clock reached the reader:\n%s", name, out)
		}
		// The report header follows the same rule.
		if !strings.Contains(out, "2026-09-30 10:39") {
			t.Errorf("%s: header time not in the configured zone:\n%s", name, out)
		}
		if strings.Contains(out, "2026-09-30 02:39") {
			t.Errorf("%s: header printed the upstream wall clock:\n%s", name, out)
		}
	}
	// The card displays the REMAINING caliber: 100% used -> 0.0% remaining,
	// while the threshold that fired is still the used percentage.
	// The block shows it as the large share on the right.
	if !strings.Contains(string(card), `\u003cfont color='red'\u003e0.0%\u003c/font\u003e`) {
		t.Errorf("card does not render the remaining caliber:\n%s", string(card))
	}

	// windowLine is the other formatting path; it must agree.
	if got := r.windowLine(rep.Providers[0].BestWindows[0]); !strings.Contains(got, "10-04 01:00") {
		t.Errorf("windowLine bypassed the zone: %q", got)
	}
}

// TestPlanAndExtraUsageShownOnlyWhenReported: both are optional upstream data.
// Present means shown; absent means absent, never a "未知套餐" placeholder or a
// zeroed budget.
func TestPlanAndExtraUsageShownOnlyWhenReported(t *testing.T) {
	low := 10.0
	base := func() domain.Report {
		return domain.Report{
			GeneratedAt: time.Date(2026, 9, 30, 9, 48, 0, 0, time.UTC),
			Providers: []domain.ProviderReport{{
				Provider: domain.ProviderClaude, Total: 1,
				States: map[string]domain.CredentialState{"k": domain.StateHealthy},
				Snapshots: []domain.QuotaSnapshot{{
					Credential: domain.Credential{Key: "k", Alias: "main"}, OK: true,
					Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported,
					Windows: []domain.QuotaWindow{{Name: "claude/five_hour", Label: "账号 · 5小时", Scope: domain.ScopeAccount, UsedPercent: &low}},
				}},
			}},
		}
	}
	r := New(config.ToneCasual).WithLocation(time.UTC)

	bare := base()
	out := r.Text(domain.Message{Report: &bare})
	for _, banned := range []string{"套餐", "额外用量", "未知", "credits"} {
		if strings.Contains(out, banned) {
			t.Errorf("unreported field rendered as %q:\n%s", banned, out)
		}
	}

	rich := base()
	credits, limit, util := 12.5, 100.0, 12.5
	rich.Providers[0].Snapshots[0].Plan = "plan_max"
	rich.Providers[0].Snapshots[0].ExtraUsage = &domain.ExtraUsage{
		Enabled: true, UsedCredits: &credits, MonthlyLimit: &limit, UsedPercent: &util,
	}
	out = r.Text(domain.Message{Report: &rich})
	for _, want := range []string{"套餐 plan_max", "额外用量", "已用 12.5 / 上限 100 credits", "12.5%"} {
		if !strings.Contains(out, want) {
			t.Errorf("reported field missing %q:\n%s", want, out)
		}
	}
	// Credits are the upstream's own unit; no currency exists in the payload.
	for _, banned := range []string{"$", "美元", "USD", "￥"} {
		if strings.Contains(out, banned) {
			t.Errorf("invented a currency %q:\n%s", banned, out)
		}
	}

	// A disabled budget is not rendered even though the block exists.
	off := base()
	off.Providers[0].Snapshots[0].ExtraUsage = &domain.ExtraUsage{Enabled: false, UsedCredits: &credits}
	if got := r.Text(domain.Message{Report: &off}); strings.Contains(got, "额外用量") {
		t.Errorf("disabled budget rendered:\n%s", got)
	}
}

// TestLimitReachedMarkerSurvivesADisagreeingPercentage: the marker and the
// percentage are independent signals. Silently siding with the number would
// hide a real block.
func TestLimitReachedMarkerSurvivesADisagreeingPercentage(t *testing.T) {
	partial := 42.0
	rep := domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: 1,
		States: map[string]domain.CredentialState{"k": domain.StateWarning},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "k", Alias: "acct"}, OK: true,
			Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported,
			Windows: []domain.QuotaWindow{{
				Name: "w", Label: "账号 · 周窗口", Scope: domain.ScopeAccount,
				UsedPercent: &partial, LimitReached: true,
			}},
		}},
	}}}
	out := New(config.ToneCasual).WithLocation(time.UTC).Text(domain.Message{Report: &rep})
	if !strings.Contains(out, "42.0%") {
		t.Errorf("reported percentage lost:\n%s", out)
	}
	if !strings.Contains(out, "上游标记已达上限") {
		t.Errorf("upstream limit marker lost:\n%s", out)
	}
}

// TestUnknownScopeBlocksAVerdict: an undeclared applicability is never rounded
// into "fine" or "exhausted"; we say we cannot decide, and why.
func TestUnknownScopeBlocksAVerdict(t *testing.T) {
	low := 5.0
	rep := domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderGeminiCLI, Total: 1,
		States: map[string]domain.CredentialState{"k": domain.StateLimited},
		Snapshots: []domain.QuotaSnapshot{{
			Credential: domain.Credential{Key: "k", Alias: "g"}, OK: true,
			Source: domain.SourceCPA, Confidence: domain.ConfidenceReported,
			Windows: []domain.QuotaWindow{{Name: "mystery", UsedPercent: &low}},
		}},
	}}}
	out := New(config.ToneCasual).WithLocation(time.UTC).Text(domain.Message{Report: &rep})
	if !strings.Contains(out, "暂时给不出建议") || !strings.Contains(out, "未申明适用范围") {
		t.Errorf("unknown scope did not produce an explicit no-verdict:\n%s", out)
	}
	for _, banned := range []string{"优先用", "改用", "余量充足"} {
		if strings.Contains(out, banned) {
			t.Errorf("unknown scope was rounded into a recommendation (%q):\n%s", banned, out)
		}
	}
}

func TestSecretsNeverRendered(t *testing.T) {
	msg := fixture()
	for _, tone := range []string{config.ToneCasual, config.ToneFormal} {
		r := New(tone).WithLocation(time.UTC)

		text := r.Text(msg)
		card, err := json.Marshal(r.Card(msg))
		if err != nil {
			t.Fatalf("card marshal: %v", err)
		}
		for _, secret := range []string{secretCookie, secretToken, secretMgmt} {
			if strings.Contains(text, secret) {
				t.Errorf("tone %s: secret leaked into text: %s", tone, secret)
			}
			if strings.Contains(string(card), secret) {
				t.Errorf("tone %s: secret leaked into card: %s", tone, secret)
			}
		}
		// Raw upstream data must not be present at all.
		if strings.Contains(text, "raw") || strings.Contains(string(card), "management_key") {
			t.Errorf("tone %s: raw upstream data leaked", tone)
		}
	}
}

func TestDegradedAndFreshnessRendered(t *testing.T) {
	msg := fixture()
	failedAt := time.Date(2026, 3, 13, 18, 0, 0, 0, time.UTC)
	lastOK := time.Date(2026, 3, 12, 9, 30, 0, 0, time.UTC)
	// Mark the provider degraded with a stale snapshot whose last success is
	// older than the failed attempt.
	msg.Report.Degraded = true
	msg.Report.Notes = []string{"部分渠道采集失败"}
	msg.Report.Providers[0].Error = "control plane unreachable"
	msg.Report.Providers[0].Snapshots[0].OK = false
	msg.Report.Providers[0].Snapshots[0].Stale = true
	msg.Report.Providers[0].Snapshots[0].FetchedAt = failedAt
	msg.Report.Providers[0].Snapshots[0].LastSuccessAt = lastOK

	r := New(config.ToneCasual).WithLocation(time.UTC)
	text := r.Text(msg)
	for _, want := range []string{"降级", "部分渠道采集失败", "旧值", "2026-03-12 09:30"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	// The failed attempt's timestamp must not be presented as the data time.
	if strings.Contains(text, "最后成功 2026-03-13 18:00") {
		t.Errorf("failed attempt time shown as last success:\n%s", text)
	}

	card, err := json.Marshal(r.Card(msg))
	if err != nil {
		t.Fatalf("card marshal: %v", err)
	}
	cardStr := string(card)
	// An alert card keeps its alert and says the data behind it is partial.
	for _, want := range []string{"降级", "旧值", "2026-03-12 09:30"} {
		if !strings.Contains(cardStr, want) {
			t.Errorf("alert card missing %q:\n%s", want, cardStr)
		}
	}
	// A degraded query is an error card: the failure first, the old data only
	// in the fold, still labelled as old with its own last success.
	query := msg
	query.Alerts = nil
	qb, _ := json.Marshal(r.Card(query))
	for _, want := range []string{"实时采集失败", "旧值", "2026-03-12 09:30"} {
		if !strings.Contains(string(qb), want) {
			t.Errorf("query card missing %q:\n%s", want, string(qb))
		}
	}
}

// TestStaleCredentialUsesItsOwnLastSuccess is the regression for the
// misattributed freshness seen in production: a credential that never produced
// a reading was shown with a sibling credential's "最后成功" time, which made a
// never-measured account look like it had succeeded minutes ago.
func TestStaleCredentialUsesItsOwnLastSuccess(t *testing.T) {
	used := 100.0
	okAt := time.Date(2026, 9, 30, 9, 48, 0, 0, time.UTC)
	good := domain.Credential{Key: "good", Provider: domain.ProviderCodex, Alias: "good", ShortID: "cb31"}
	never := domain.Credential{Key: "never", Provider: domain.ProviderCodex, Alias: "never", ShortID: "ad3d"}
	rep := domain.Report{
		GeneratedAt: okAt,
		Providers: []domain.ProviderReport{{
			Provider: domain.ProviderCodex, Total: 2,
			States: map[string]domain.CredentialState{"good": domain.StateExhausted, "never": domain.StateInvalid},
			Snapshots: []domain.QuotaSnapshot{
				{Credential: good, OK: true, Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported,
					LastSuccessAt: okAt,
					Windows:       []domain.QuotaWindow{{Name: "codex/rate_limit/primary_window", Label: "账号 · 主额度窗口", Scope: domain.ScopeAccount, UsedPercent: &used}}},
				{Credential: never, Source: domain.SourceCPAV0, Confidence: domain.ConfidenceUnknown, Failure: domain.FailureAuth},
			},
		}},
	}
	out := New(config.ToneCasual).WithLocation(time.UTC).Text(domain.Message{Report: &rep})
	if !strings.Contains(out, "never  凭证失效") {
		t.Errorf("invalid credential not named with its verdict:\n%s", out)
	}
	if !strings.Contains(out, "最后成功 从未成功") {
		t.Errorf("a never-successful credential must not borrow a sibling's success time:\n%s", out)
	}
	if strings.Contains(out, "never  凭证失效  依据：认证被上游拒绝  最后成功 2026-09-30 09:48") {
		t.Errorf("provider-wide last success leaked onto a credential that never succeeded:\n%s", out)
	}
}

func TestEstimatedConfidenceLabelled(t *testing.T) {
	msg := fixture()
	msg.Report.Providers[0].Snapshots[0].Confidence = domain.ConfidenceEstimated
	r := New(config.ToneFormal).WithLocation(time.UTC)
	if !strings.Contains(r.Text(msg), "估算") {
		t.Errorf("estimated confidence not labelled:\n%s", r.Text(msg))
	}
}

func TestCardsHaveSingleReadOnlyRefreshButton(t *testing.T) {
	r := New(config.ToneCasual).WithLocation(time.UTC)
	// An alert card has no refresh: it would replace the alert with a query.
	if n := countBehaviors(r.Card(fixture()), "callback"); n != 0 && len(fixture().Alerts) > 0 {
		t.Errorf("alert card carries %d refresh callbacks, want 0", n)
	}
	query := fixture()
	query.Alerts = nil
	card := r.Card(query)

	body, ok := card["body"].(map[string]any)
	if !ok {
		t.Fatal("card has no body")
	}
	elements, ok := body["elements"].([]any)
	if !ok {
		t.Fatal("card body has no elements")
	}

	buttons := 0
	for _, el := range elements {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		// The refresh button sits inside a column_set, the layout Feishu
		// requires now that the bare "action" element is gone.
		cols, ok := m["columns"].([]any)
		if !ok {
			continue
		}
		for _, c := range cols {
			cm, _ := c.(map[string]any)
			for _, sub := range asAnySlice(cm["elements"]) {
				sm, _ := sub.(map[string]any)
				if sm["tag"] != "button" {
					continue
				}
				behaviors, _ := sm["behaviors"].([]any)
				b0, _ := behaviors[0].(map[string]any)
				value, ok := b0["value"].(map[string]any)
				if !ok {
					t.Fatalf("behaviors[0].value must be an object, got %T", b0["value"])
				}
				if value["action"] != RefreshAction {
					t.Errorf("button action = %v, want %q", value["action"], RefreshAction)
				}
				buttons++
			}
		}
	}
	if buttons < 1 {
		t.Errorf("card should have at least the refresh button, got %d", buttons)
	}

	// The refresh payload is the only callback action, and it is read-only.
	if RefreshAction != "refresh_quota" {
		t.Errorf("RefreshAction = %q, want refresh_quota", RefreshAction)
	}
}

func asAnySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func TestAdviceComesFromAlertNotRenderer(t *testing.T) {
	msg := fixture()
	r := New(config.ToneCasual).WithLocation(time.UTC)
	text := r.Text(msg)
	for _, a := range msg.Alerts {
		if !strings.Contains(text, a.Advice) {
			t.Errorf("advice %q missing from text", a.Advice)
		}
	}
}

func TestUnknownPercentageNotRenderedAsZero(t *testing.T) {
	msg := fixture()
	r := New(config.ToneCasual).WithLocation(time.UTC)
	text := r.Text(msg)
	// The Ollama window has a nil UsedPercent; we must not show it as 0.0%.
	if strings.Contains(text, "0.0%") {
		t.Errorf("nil utilization rendered as 0.0%%:\n%s", text)
	}
}

func TestTitleEnvOverride(t *testing.T) {
	t.Setenv("NOTIFY_TITLE_DAILY", "自定义标题")
	text := New(config.ToneCasual).Text(domain.Message{Kind: "daily", Body: "x"})
	if !strings.Contains(text, "自定义标题") {
		t.Errorf("env title override not applied:\n%s", text)
	}
}

func TestQueryKindTitle(t *testing.T) {
	text := New(config.ToneCasual).Text(domain.Message{Kind: KindQuery, Report: &domain.Report{}})
	if !strings.Contains(text, "额度查询") {
		t.Errorf("query title missing:\n%s", text)
	}
}

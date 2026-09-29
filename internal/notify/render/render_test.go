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
			{Name: "5h", UsedPercent: &used92, ResetAt: &reset},
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
					{Name: "5h", UsedPercent: &used92, ResetAt: &reset},
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
			Advice:     "切换其它 Codex 凭证，或等 5h 窗口重置。",
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
		for _, want := range wantSubstrings {
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
	if !strings.Contains(casual, "先省着点用") {
		t.Errorf("casual conclusion missing team voice:\n%s", casual)
	}
	if !strings.Contains(formal, "用量偏高") {
		t.Errorf("formal conclusion missing neutral wording:\n%s", formal)
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
	for _, want := range []string{"降级", "部分渠道采集失败", "数据已过期", "2026-03-12 09:30"} {
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
	for _, want := range []string{"降级", "数据已过期", "2026-03-12 09:30"} {
		if !strings.Contains(cardStr, want) {
			t.Errorf("card missing %q:\n%s", want, cardStr)
		}
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
	card := r.Card(fixture())

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
		if !ok || m["tag"] != "button" {
			continue
		}
		buttons++
		behaviors, _ := m["behaviors"].([]any)
		if len(behaviors) != 1 {
			t.Fatalf("button should have exactly one behavior, got %d", len(behaviors))
		}
		b0, _ := behaviors[0].(map[string]any)
		value, ok := b0["value"].(map[string]any)
		if !ok {
			t.Fatalf("behaviors[0].value must be an object, got %T", b0["value"])
		}
		if value["action"] != RefreshAction {
			t.Errorf("button action = %v, want %q", value["action"], RefreshAction)
		}
	}
	if buttons != 1 {
		t.Errorf("card should have exactly one button, got %d", buttons)
	}

	// The refresh payload is the only action, and it is read-only.
	if RefreshAction != "refresh_quota" {
		t.Errorf("RefreshAction = %q, want refresh_quota", RefreshAction)
	}
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

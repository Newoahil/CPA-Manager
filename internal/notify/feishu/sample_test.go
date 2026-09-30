package feishu

import (
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// This file reconstructs the exact production report that produced the bad
// Feishu reply and renders it through the current pipeline. It is both a
// golden-ish readability check and the end-to-end assertion for the fixes:
// no internal paths, no cross-channel advice, no "0/3", one line per healthy
// channel, and the invalid credential named explicitly.

func pct(v float64) *float64 { return &v }

func at(mm, dd, hh, mi int) *time.Time {
	t := time.Date(2026, time.Month(mm), dd, hh, mi, 0, 0, time.UTC)
	return &t
}

// productionReport mirrors the 8-credential CPA v0 run from the incident.
func productionReport() domain.Report {
	gen := time.Date(2026, 9, 30, 9, 48, 0, 0, time.UTC)

	agWindow := func(used float64, reset *time.Time) domain.QuotaWindow {
		return domain.QuotaWindow{
			Name:  "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1",
			Label: "Gemini Models · 周", Scope: domain.ScopeGroup,
			ScopeID: "antigravity/groups/Gemini%20Models#1", UsedPercent: pct(used), ResetAt: reset,
		}
	}
	agSnap := func(key, alias, short string, used float64) domain.QuotaSnapshot {
		return domain.QuotaSnapshot{
			Credential: domain.Credential{Key: key, Provider: domain.ProviderAntigravity, Alias: alias, ShortID: short},
			Windows: []domain.QuotaWindow{
				agWindow(used, at(10, 6, 3, 55)),
				{Name: "antigravity/groups/Gemini%20Models#1/buckets/gemini-5h/five_hour#1",
					Label: "Gemini Models · gemini-5h · five hour", Scope: domain.ScopeGroup,
					ScopeID: "antigravity/groups/Gemini%20Models#1", UsedPercent: pct(used / 3), ResetAt: at(9, 30, 14, 0)},
			},
			Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported,
			FetchedAt: gen, LastSuccessAt: gen, OK: true,
		}
	}

	antigravity := domain.ProviderReport{
		Provider: domain.ProviderAntigravity, Total: 3, Healthy: 0, Limited: 3,
		WorstState: domain.StateLimited,
		States: map[string]domain.CredentialState{
			"ag1": domain.StateLimited, "ag2": domain.StateLimited, "ag3": domain.StateLimited,
		},
		Snapshots: []domain.QuotaSnapshot{
			agSnap("ag1", "antigravity-hongwane3", "ade0", 2.4),
			agSnap("ag2", "antigravity-lxy", "77b1", 35.0),
			agSnap("ag3", "antigravity-tuzi", "9c20", 0.0),
		},
	}

	claude := domain.ProviderReport{
		Provider: domain.ProviderClaude, Total: 2, Healthy: 1, Normal: 1, Limited: 1,
		WorstState: domain.StateLimited,
		States:     map[string]domain.CredentialState{"cl1": domain.StateHealthy, "cl2": domain.StateLimited},
		Snapshots: []domain.QuotaSnapshot{
			{
				Credential: domain.Credential{Key: "cl1", Provider: domain.ProviderClaude, Alias: "claude-main", ShortID: "9f10"},
				Windows: []domain.QuotaWindow{
					{Name: "claude/five_hour", Label: "账号 · 5小时", Scope: domain.ScopeAccount, UsedPercent: pct(17.0), ResetAt: at(9, 30, 14, 0)},
					{Name: "claude/seven_day", Label: "账号 · 7天", Scope: domain.ScopeAccount, UsedPercent: pct(41.0), ResetAt: at(10, 4, 9, 0)},
				},
				// extra_usage ships inside the same usage response.
				ExtraUsage: &domain.ExtraUsage{Enabled: true, UsedCredits: pct(12.5), MonthlyLimit: pct(100), UsedPercent: pct(12.5)},
				Source:     domain.SourceCPAV0, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen, OK: true,
			},
			{
				Credential: domain.Credential{Key: "cl2", Provider: domain.ProviderClaude, Alias: "claude-External", ShortID: "da5c"},
				Windows: []domain.QuotaWindow{
					{Name: "claude/limits/weekly_scoped/fable#1", Label: "fable 模型 · 周", Scope: domain.ScopeModel, ScopeID: "fable", UsedPercent: pct(7.0), ResetAt: at(10, 1, 21, 0)},
				},
				Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen, OK: true,
			},
		},
	}

	codex := domain.ProviderReport{
		Provider: domain.ProviderCodex, Total: 3, Healthy: 0, Abnormal: 3,
		WorstState: domain.StateInvalid,
		States: map[string]domain.CredentialState{
			"cx1": domain.StateExhausted, "cx2": domain.StateExhausted, "cx3": domain.StateInvalid,
		},
		Snapshots: []domain.QuotaSnapshot{
			{
				Credential: domain.Credential{Key: "cx1", Provider: domain.ProviderCodex, Alias: "codex-vinsprite78", ShortID: "cb31"},
				Windows: []domain.QuotaWindow{
					// limit_window_seconds is now read, so the period is stated
					// rather than guessed from the primary/secondary ordering.
					{Name: "codex/rate_limit/primary_window", Label: "账号 · 周窗口", Scope: domain.ScopeAccount,
						WindowSeconds: 604800, LimitReached: true, UsedPercent: pct(100.0), ResetAt: at(10, 6, 4, 4)},
					{Name: "codex/rate_limit/secondary_window", Label: "账号 · 5小时窗口", Scope: domain.ScopeAccount,
						WindowSeconds: 18000, LimitReached: true, UsedPercent: pct(100.0), ResetAt: at(9, 30, 13, 4)},
				},
				Plan:   "plus",
				Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen, OK: true,
			},
			{
				Credential: domain.Credential{Key: "cx2", Provider: domain.ProviderCodex, Alias: "codex-vintechg1", ShortID: "e1ae"},
				Windows: []domain.QuotaWindow{
					{Name: "codex/rate_limit/primary_window", Label: "账号 · 周窗口", Scope: domain.ScopeAccount,
						WindowSeconds: 604800, LimitReached: true, UsedPercent: pct(100.0), ResetAt: at(10, 3, 17, 0)},
				},
				Plan:   "pro",
				Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported, FetchedAt: gen, LastSuccessAt: gen, OK: true,
			},
			{
				Credential: domain.Credential{Key: "cx3", Provider: domain.ProviderCodex, Alias: "codex-design", ShortID: "ad3d"},
				Source:     domain.SourceCPAV0, Confidence: domain.ConfidenceUnknown, FetchedAt: gen,
				Failure: domain.FailureAuth,
			},
		},
	}

	return domain.Report{
		GeneratedAt: gen,
		Providers:   []domain.ProviderReport{antigravity, claude, codex},
		Recommendations: []domain.Recommendation{
			{Provider: domain.ProviderAntigravity, Direction: domain.DirectionSteady,
				Reason: "antigravity-hongwane3 · ade0、antigravity-lxy · 77b1、antigravity-tuzi · 9c20：仍有可用 scope（1 个），不代表整个 provider 充足"},
			{Provider: domain.ProviderClaude, Direction: domain.DirectionUseMore,
				Reason: "所选单个凭证的 账号 · 7天 窗口已用 41%，该凭证已报告窗口余量充足，可继续使用。"},
			{Provider: domain.ProviderCodex, Direction: domain.DirectionReauth,
				Reason: "失效凭证：codex-design · ad3d（认证被上游拒绝）；建议先前往 CPA 重新完成 OAuth 登录。其余凭证的额度读数不受影响。"},
		},
	}
}

// TestProductionSampleReply renders the incident report and asserts every
// defect from it is gone. The full output is logged so it can be eyeballed.
func TestProductionSampleReply(t *testing.T) {
	b, _ := newTestBot(t, &fakeRefresher{})
	b.cfg.PollInterval = 15 * time.Minute
	rep := productionReport()
	now := rep.GeneratedAt.Add(20 * time.Second)

	live := b.buildReply(intent{kind: intentStatus}, rep, freshnessOf(rep, false, b.cfg.PollInterval, now))
	t.Logf("\n=== 整体查询（实时）===\n%s", live)

	cached := b.buildReply(intent{kind: intentStatus}, rep, freshnessOf(rep, true, b.cfg.PollInterval, rep.GeneratedAt.Add(2*time.Minute)))
	t.Logf("\n=== 整体查询（限频回落到缓存，仍在 PollInterval 内）===\n%s", cached)

	expired := b.buildReply(intent{kind: intentStatus}, rep, freshnessOf(rep, true, b.cfg.PollInterval, rep.GeneratedAt.Add(3*time.Hour)))
	t.Logf("\n=== 整体查询（取不到新数据且缓存已过期）===\n%s", expired)

	single := b.buildReply(intent{kind: intentProvider, provider: domain.ProviderCodex}, rep, freshnessOf(rep, false, b.cfg.PollInterval, now))
	t.Logf("\n=== 单渠道查询 @我 codex ===\n%s", single)

	// D: no internal identifiers anywhere in the reader-facing text.
	for _, out := range []string{live, cached, expired, single} {
		for _, banned := range []string{"antigravity/groups", "%20", "/buckets/", "weekly#1", "claude/limits", "codex/rate_limit", "group:", "model:"} {
			if strings.Contains(out, banned) {
				t.Errorf("internal identifier %q leaked:\n%s", banned, out)
			}
		}
	}

	// B: Ollama's cookie remediation must not appear on a Codex report.
	if strings.Contains(live, "__Secure-session") || strings.Contains(live, "cookie") {
		t.Errorf("Ollama-specific advice leaked into a Codex/Claude/Antigravity report:\n%s", live)
	}

	// E: no binary healthy count, and a channel with headroom is not "all broken".
	if strings.Contains(live, "0/3") || strings.Contains(live, "0/2") || strings.Contains(live, "个凭证）") {
		t.Errorf("binary healthy/total counting survived:\n%s", live)
	}
	if !strings.Contains(live, "3 个号 · 3 个局部额度") {
		t.Errorf("Antigravity not counted as limited-but-usable:\n%s", live)
	}

	// A: the invalid credential is named, and the exhausted ones are not
	// described as invalid.
	if !strings.Contains(live, "codex-design · ad3d  凭证失效") {
		t.Errorf("invalid credential not identified:\n%s", live)
	}
	if !strings.Contains(live, "2 个已用满") || !strings.Contains(live, "1 个凭证失效") {
		t.Errorf("Codex breakdown does not separate exhaustion from invalidity:\n%s", live)
	}
	for _, spent := range []string{"codex-vinsprite78 · cb31  凭证失效", "codex-vintechg1 · e1ae  凭证失效"} {
		if strings.Contains(live, spent) {
			t.Errorf("an exhausted credential was labelled invalid:\n%s", live)
		}
	}

	// F: healthy/limited channels fold to one line; only Codex expands.
	agLines := 0
	for _, line := range strings.Split(live, "\n") {
		if strings.HasPrefix(line, "Antigravity") || (strings.HasPrefix(line, "  · antigravity") || strings.HasPrefix(line, "  · 建议：antigravity")) {
			agLines++
		}
	}
	if agLines != 1 {
		t.Errorf("Antigravity should fold to exactly one line, got %d:\n%s", agLines, live)
	}
	if n := strings.Count(live, "仍有可用 scope"); n > 1 {
		t.Errorf("advice repeated %d times:\n%s", n, live)
	}

	// Evidence survives the fold.
	for _, want := range []string{"100.0%", "10-06 04:04", "10-03 17:00", "cpa-v0", "已上报", "最后成功 从未成功"} {
		if !strings.Contains(live, want) {
			t.Errorf("evidence %q lost:\n%s", want, live)
		}
	}

	// Conclusion answers what to do, and carries no disclaimers.
	conclusion := ""
	for _, line := range strings.Split(live, "\n") {
		if strings.HasPrefix(line, "结论：") {
			conclusion = line
		}
	}
	if !strings.Contains(conclusion, "Codex 先别用了") || !strings.Contains(conclusion, "改用 Claude 或 Antigravity") {
		t.Errorf("conclusion does not say what to do: %q", conclusion)
	}
	if !strings.Contains(conclusion, "最早 10-03 17:00 恢复") {
		t.Errorf("exhausted channel has no recovery time: %q", conclusion)
	}
	for _, banned := range []string{"未覆盖", "不代表", "scope", "已测", "证据"} {
		if strings.Contains(conclusion, banned) {
			t.Errorf("conclusion carries a disclaimer %q: %q", banned, conclusion)
		}
	}
	// ...and the caveat it dropped still exists, on Antigravity's own line.
	if !strings.Contains(live, "未覆盖全账号") {
		t.Errorf("coverage caveat disappeared entirely:\n%s", live)
	}

	// Newly parsed data is actually shown.
	for _, want := range []string{"账号 · 周窗口", "套餐 plus/pro", "额外用量", "已用 12.5 / 上限 100 credits"} {
		if !strings.Contains(live, want) {
			t.Errorf("newly parsed field %q not rendered:\n%s", want, live)
		}
	}
	if strings.Contains(live, "主额度窗口") {
		t.Errorf("structural fallback used even though the window length was reported:\n%s", live)
	}
	// The 5-hour window is named from its reported length in the detail view.
	if !strings.Contains(single, "账号 · 5小时窗口") {
		t.Errorf("single-channel view lost the 5-hour window:\n%s", single)
	}

	// C: the three freshness wordings.
	if !strings.Contains(live, "· 实时") || strings.Contains(live, "采集失败") {
		t.Errorf("live answer mislabelled:\n%s", live)
	}
	if !strings.Contains(cached, "本次未实时刷新") || strings.Contains(cached, "⚠️ 未能取到新数据") {
		t.Errorf("still-current cache raised a false alarm:\n%s", cached)
	}
	if !strings.Contains(expired, "未能取到新数据") || !strings.Contains(expired, "可能已过期") {
		t.Errorf("genuinely expired cache not warned about:\n%s", expired)
	}

	// F: single-channel query expands every window of that channel only.
	if strings.Contains(single, "Antigravity") || strings.Contains(single, "Claude") {
		t.Errorf("single-channel reply leaked other channels:\n%s", single)
	}
}

// TestProductionSampleCard renders the same report as a card so the folding
// rules can be checked on the visual surface too.
func TestProductionSampleCard(t *testing.T) {
	b, _ := newTestBot(t, &fakeRefresher{})
	rep := productionReport()
	card := b.renderer.Card(domain.Message{Kind: "query", Report: &rep, Freshness: "实时"})
	body, _ := card["body"].(map[string]any)
	elements, _ := body["elements"].([]any)
	var text strings.Builder
	for _, el := range elements {
		if m, ok := el.(map[string]any); ok {
			if c, ok := m["content"].(string); ok {
				text.WriteString(c + "\n")
			}
		}
	}
	t.Logf("\n=== 卡片正文 ===\n%s", text.String())
	for _, banned := range []string{"antigravity/groups", "%20", "__Secure-session", "0/3"} {
		if strings.Contains(text.String(), banned) {
			t.Errorf("card leaked %q:\n%s", banned, text.String())
		}
	}
	if !strings.Contains(text.String(), "codex-design · ad3d  凭证失效") {
		t.Errorf("card does not name the invalid credential:\n%s", text.String())
	}
}

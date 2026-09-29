package config

import (
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// baseEnv sets the minimum required environment for Load to succeed.
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CPA_BASE_URL", "http://cpa:8317")
	t.Setenv("CPA_MANAGEMENT_KEY", "k")
	t.Setenv("TZ_NAME", "UTC")
	// Clear anything an ambient environment might inject.
	for _, k := range []string{
		"QUOTA_THRESHOLDS_JSON", "QUOTA_THRESHOLD_NOTICE", "QUOTA_THRESHOLD_WARN",
		"QUOTA_THRESHOLD_URGENT", "OLLAMA_ACCOUNTS_JSON", "POLL_INTERVAL",
		"REFRESH_MIN_INTERVAL", "FEISHU_ENABLED", "ANOMALY_FAILURE_RATE",
		"CPA_API_VERSION", "CPA_QUOTA_STRATEGY", "ANTIGRAVITY_QUOTA_PROFILE",
		"CPA_CONTEXT_OVERRIDES_JSON",
	} {
		t.Setenv(k, "")
	}
}

func TestProviderOverrideWhitelist(t *testing.T) {
	for _, name := range []string{"codex", "claude", "gemini-cli", "antigravity", "ollama", "cluade", "gemini", "Codex", "unknown"} {
		t.Run(name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("QUOTA_THRESHOLDS_JSON", `{"`+name+`":{"notice":80}}`)
			_, err := Load()
			valid := name == "codex" || name == "claude" || name == "gemini-cli" || name == "antigravity" || name == "ollama"
			if (err == nil) != valid {
				t.Fatalf("provider %q err=%v", name, err)
			}
		})
	}
}

func TestLoadContextOverrides(t *testing.T) {
	baseEnv(t)
	t.Setenv("CPA_CONTEXT_OVERRIDES_JSON", `{"antigravity/0123456789abcdef":{"project_id":"verified-project"}}`)
	cfg, err := Load()
	if err != nil || cfg.CPAContextOverrides["antigravity/0123456789abcdef"].ProjectID != "verified-project" {
		t.Fatal(err)
	}
	t.Setenv("CPA_CONTEXT_OVERRIDES_JSON", `{"antigravity/0123456789abcdef":{"token":"secret"}}`)
	if _, err := Load(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid override accepted or leaked")
	}
}

func TestLoadDefaults(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultThresholds != (Thresholds{Notice: 90, Warn: 95, Urgent: 100}) {
		t.Errorf("defaults = %+v", cfg.DefaultThresholds)
	}
	if cfg.RefreshMinInterval != DefaultRefreshMinInterval {
		t.Errorf("refresh interval = %v", cfg.RefreshMinInterval)
	}
	if cfg.CPAAPIVersion != "auto" || cfg.CPAQuotaStrategy != "auto" || cfg.AntigravityQuotaProfile != "current" {
		t.Fatal("wrong compatibility defaults")
	}
}

// TestPartialOverrideKeepsDefaults is the core regression: a partial override
// must merge, not zero the missing fields.
func TestPartialOverrideKeepsDefaults(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"codex":{"notice":80}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.ThresholdsFor(domain.ProviderCodex)
	want := Thresholds{Notice: 80, Warn: 95, Urgent: 100}
	if got != want {
		t.Fatalf("merged thresholds = %+v, want %+v (missing fields must keep defaults)", got, want)
	}
}

func TestOverrideMergesSomeFields(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"ollama":{"notice":70,"warn":88}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.ThresholdsFor(domain.ProviderOllama)
	want := Thresholds{Notice: 70, Warn: 88, Urgent: 100}
	if got != want {
		t.Fatalf("merged thresholds = %+v, want %+v", got, want)
	}
}

func TestInvalidThresholdOrderingRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"codex":{"notice":95,"warn":80}}`)
	if _, err := Load(); err == nil {
		t.Fatal("expected error for notice > warn")
	}
}

func TestThresholdOutOfRangeRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"codex":{"urgent":150}}`)
	if _, err := Load(); err == nil {
		t.Fatal("expected error for urgent > 100")
	}
}

func TestNegativeThresholdRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_THRESHOLD_NOTICE", "-1")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for negative default threshold")
	}
}

func TestDuplicateOllamaNamesRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("OLLAMA_ACCOUNTS_JSON", `[
		{"name":"work","secure_session":"a"},
		{"name":"work","secure_session":"b"}
	]`)
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for duplicate Ollama account names")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate, got: %v", err)
	}
}

func TestInvalidEnvValueIsErrorNotFallback(t *testing.T) {
	cases := []struct {
		key, value string
	}{
		{"POLL_INTERVAL", "not-a-duration"},
		{"CPA_TIMEOUT", "banana"},
		{"STALE_AFTER_FAILURES", "many"},
		{"ANOMALY_FAILURE_RATE", "high"},
		{"FEISHU_ENABLED", "yes-please"},
		{"REFRESH_MIN_INTERVAL", "0s"},
		{"CPA_API_VERSION", "v7"},
		{"CPA_QUOTA_STRATEGY", "guess"},
		{"ANTIGRAVITY_QUOTA_PROFILE", "fallback"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			baseEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s=%q, got nil (silent fallback)", tc.key, tc.value)
			}
		})
	}
}

func TestValidEnvValuesParse(t *testing.T) {
	baseEnv(t)
	t.Setenv("POLL_INTERVAL", "30m")
	t.Setenv("REFRESH_MIN_INTERVAL", "45s")
	t.Setenv("FEISHU_ENABLED", "true")
	t.Setenv("FEISHU_APP_ID", "id")
	t.Setenv("FEISHU_APP_SECRET", "secret")
	t.Setenv("FEISHU_CHAT_ID", "chat")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollInterval != 30*time.Minute || cfg.RefreshMinInterval != 45*time.Second {
		t.Errorf("intervals = %v / %v", cfg.PollInterval, cfg.RefreshMinInterval)
	}
	if !cfg.FeishuEnabled {
		t.Error("feishu should be enabled")
	}
}

func TestUnknownProviderOverrideRejected(t *testing.T) {
	baseEnv(t)
	// A provider key with a bad ordering is caught even if it is not one of the
	// built-in providers: better to reject than store an unusable value.
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"unknown":{"notice":99,"warn":1}}`)
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid unknown-provider override")
	}
}

func TestCompatibilityOptionsAndGeminiThreshold(t *testing.T) {
	baseEnv(t)
	t.Setenv("CPA_API_VERSION", "v0")
	t.Setenv("CPA_QUOTA_STRATEGY", "proxy")
	t.Setenv("ANTIGRAVITY_QUOTA_PROFILE", "legacy")
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"gemini-cli":{"notice":80}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CPAAPIVersion != "v0" || cfg.CPAQuotaStrategy != "proxy" || cfg.AntigravityQuotaProfile != "legacy" || cfg.ThresholdsFor(domain.ProviderGeminiCLI).Notice != 80 {
		t.Fatal("compatibility options not wired")
	}
}

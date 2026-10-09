package domain

import "testing"

func TestTrimProviderPrefix(t *testing.T) {
	cases := []struct {
		name     string
		alias    string
		provider ProviderKind
		want     string
	}{
		{"trims claude prefix", "claude-External0.2", ProviderClaude, "External0.2"},
		{"trims codex prefix", "codex-design", ProviderCodex, "design"},
		{"trims antigravity prefix", "antigravity-hongwane3", ProviderAntigravity, "hongwane3"},
		{"trims gemini-cli prefix", "gemini-cli-work", ProviderGeminiCLI, "work"},
		{"trims ollama prefix", "ollama-backup", ProviderOllama, "backup"},
		// Case-insensitive on the prefix, preserving the remainder's own case.
		{"case-insensitive prefix", "Claude-External", ProviderClaude, "External"},
		{"case-insensitive prefix codex", "CODEX-main", ProviderCodex, "main"},
		// A different provider's prefix is left alone.
		{"other provider prefix untouched", "codex-main", ProviderClaude, "codex-main"},
		{"no prefix unchanged", "External0.2", ProviderClaude, "External0.2"},
		// Prefix only, or prefix with no remainder, is not stripped to "".
		{"prefix only unchanged", "claude-", ProviderClaude, "claude-"},
		{"bare prefix unchanged", "codex-", ProviderCodex, "codex-"},
		{"unknown provider unchanged", "claude-x", ProviderKind("future"), "claude-x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TrimProviderPrefix(tc.alias, tc.provider); got != tc.want {
				t.Errorf("TrimProviderPrefix(%q, %s) = %q, want %q", tc.alias, tc.provider, got, tc.want)
			}
		})
	}
	// Credential.Name uses the same trimming, and falls back to Label when there
	// is no alias.
	c := Credential{Alias: "claude-External0.2", Provider: ProviderClaude}
	if got := c.Name(); got != "External0.2" {
		t.Errorf("Credential.Name() = %q, want External0.2", got)
	}
}

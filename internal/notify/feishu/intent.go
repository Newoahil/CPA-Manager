package feishu

import (
	"strings"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// intent is the parsed meaning of an @Bot message. Parsing is rule-based on
// purpose: no language model is involved, so the bot's answers stay predictable
// and cheap.
type intent struct {
	kind     intentKind
	provider domain.ProviderKind // when kind == intentProvider
}

type intentKind int

const (
	intentHelp     intentKind = iota // usage hint
	intentUnknown                    // unrecognized: reply with a short hint
	intentStatus                     // overall status
	intentAbnormal                   // only the abnormal items
	intentProvider                   // filtered to one provider
)

// providerVocab maps accepted aliases to provider kinds, in a fixed priority
// order. Order matters when one query names several providers: without it the
// map iteration would be nondeterministic.
var providerVocab = []providerAlias{
	{"codex", domain.ProviderCodex},
	{"claude", domain.ProviderClaude},
	{"gemini-cli", domain.ProviderGeminiCLI},
	{"gemini", domain.ProviderGeminiCLI},
	{"antigravity", domain.ProviderAntigravity},
	{"anti", domain.ProviderAntigravity},
	{"ollama", domain.ProviderOllama},
}

type providerAlias struct {
	alias string
	kind  domain.ProviderKind
}

var helpWords = []string{"帮助", "help", "怎么用", "用法", "指令"}

var statusWords = []string{"额度", "quota", "状态", "status", "用量", "余额", "余量", "情况", "怎么样"}

var abnormalWords = []string{"异常", "告警", "警告", "问题", "故障", "掉线", "失效", "alerts", "alert", "abnormal", "warning"}

// parseIntent classifies an @Bot message. The caller has already stripped the
// @mention token, so text is the human query.
func parseIntent(text string) intent {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return intent{kind: intentStatus}
	}
	for _, w := range helpWords {
		if matchWord(t, w) {
			return intent{kind: intentHelp}
		}
	}
	for _, w := range abnormalWords {
		if matchWord(t, w) {
			return intent{kind: intentAbnormal}
		}
	}
	// Provider filter before generic status words so "codex 额度" stays scoped.
	for _, alias := range providerVocab {
		if matchWord(t, alias.alias) {
			return intent{kind: intentProvider, provider: alias.kind}
		}
	}
	for _, w := range statusWords {
		if matchWord(t, w) {
			return intent{kind: intentStatus}
		}
	}
	return intent{kind: intentUnknown}
}

// matchWord matches CJK words by substring (they have no latin word boundaries)
// and latin words on a boundary, so "quota" does not fire inside "quotax".
func matchWord(text, word string) bool {
	if isASCII(word) {
		return containsToken(text, word)
	}
	return strings.Contains(text, word)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// containsToken reports whether token appears in text bounded by non-letter
// characters, so "quota" does not match inside another latin word.
func containsToken(text, token string) bool {
	idx := 0
	for {
		i := strings.Index(text[idx:], token)
		if i < 0 {
			return false
		}
		start := idx + i
		end := start + len(token)
		if boundary(text, start-1) && boundary(text, end) {
			return true
		}
		idx = start + 1
		if idx >= len(text) {
			return false
		}
	}
}

// boundary reports whether the byte at position i (outside the string counts as
// a boundary) is not a latin letter.
func boundary(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return true
	}
	c := s[i]
	return !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'))
}

// stripMentions removes the "@_user_N" placeholders Feishu injects into message
// text, so they do not pollute intent matching.
func stripMentions(text string) string {
	fields := strings.Fields(text)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if strings.HasPrefix(f, "@_") {
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

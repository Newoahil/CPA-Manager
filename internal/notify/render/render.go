// Package render turns a domain.Message into channel-agnostic output: plain
// text for webhooks and logs, and a Feishu interactive-card structure.
//
// It is deliberately pure: it only reads the fields it is given and never
// touches upstream payloads. Two rules are enforced here rather than trusted
// to callers:
//
//   - Everything precise (numbers, window names, reset times, data source,
//     fetch time, evidence level) survives in both tones. The tone changes the
//     voice, not the facts.
//   - Secrets never reach the output. Credentials are printed through
//     domain.Credential.Label and a raw upstream error (Snapshot.Err) is never
//     rendered verbatim, so a token or cookie that leaked into a collector
//     cannot escape through a card.
//   - Freshness is never implied. A stale snapshot, a degraded report and an
//     estimated confidence are all labelled, and the "last success" time comes
//     from LastSuccessAt, not from the failed attempt's timestamp.
package render

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// KindQuery marks an on-demand status reply (the @Bot query path).
const KindQuery = "query"

// RefreshAction is the value carried by the card's only button. It is a
// read-only re-collection, never a state mutation.
const RefreshAction = "refresh_quota"

// refreshValue is the callback payload. Feishu requires behaviors[].value to be
// an object, not a string.
var refreshValue = map[string]any{"action": RefreshAction}

// Renderer renders messages in one tone.
//
// It is safe for concurrent use: it holds only immutable configuration. The
// time zone is only used to display times; the values themselves are never
// rewritten, so callers whose config carries a specific location may use
// WithLocation.
type Renderer struct {
	tone   string
	cpaURL string
	loc    *time.Location
}

// WithLocation sets the location used to format timestamps. A nil location is
// ignored so the default (time.Local) is kept.
func (r *Renderer) WithLocation(loc *time.Location) *Renderer {
	if loc != nil {
		r.loc = loc
	}
	return r
}

// Default titles. They are intentionally usable as-is; env vars only override.
var defaultTitles = map[string]string{
	string(domain.AlertQuotaThreshold): "额度提醒",
	string(domain.AlertQuotaExhausted): "额度告急",
	string(domain.AlertCredential):     "凭证失效",
	string(domain.AlertSuspect):        "异常疑似",
	string(domain.AlertStale):          "数据过期",
	string(domain.AlertRecovered):      "额度恢复",
	string(domain.AlertQuotaReset):     "额度重置",
	string(domain.AlertBootstrap):      "额度日报",
	KindQuery:                          "额度查询",
}

// New builds a renderer for the given tone (config.ToneCasual or
// config.ToneFormal; anything else falls back to casual).
//
// Title templates can be overridden without touching code:
//
//	NOTIFY_TITLE              generic fallback template
//	NOTIFY_TITLE_<KIND>       per-kind override, kind upper-cased
//	                          e.g. NOTIFY_TITLE_QUOTA_THRESHOLD=配额预警
//	NOTIFY_CPA_PAGE_URL       optional CPA admin link shown in advice
func New(tone string) *Renderer {
	r := &Renderer{
		tone: tone,
		loc:  time.Local,
	}
	return r
}

// cpaPageURL is read per render so configuration can change without a rebuild
// and is simply absent when unset.
func (r *Renderer) cpaPageURL() string {
	return strings.TrimSpace(os.Getenv("NOTIFY_CPA_PAGE_URL"))
}

// Tone returns the active tone.
func (r *Renderer) Tone() string {
	if r.tone == config.ToneFormal {
		return config.ToneFormal
	}
	return config.ToneCasual
}

// Title exposes the resolved title so other channels (the webhook projection)
// can reuse exactly the same wording instead of re-implementing the override
// rules.
func (r *Renderer) Title(msg domain.Message) string { return r.titleFor(msg) }

// titleFor resolves the card/message title: per-kind env override, then generic
// env override, then the message's own title, then the built-in default.
func (r *Renderer) titleFor(msg domain.Message) string {
	kind := msg.Kind
	if kind == "" {
		kind = string(domain.AlertBootstrap)
	}
	if v := strings.TrimSpace(os.Getenv("NOTIFY_TITLE_" + strings.ToUpper(kind))); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("NOTIFY_TITLE")); v != "" {
		return v
	}
	if strings.TrimSpace(msg.Title) != "" {
		return msg.Title
	}
	if t, ok := defaultTitles[kind]; ok {
		return t
	}
	return "额度通知"
}

func (r *Renderer) formal() bool { return r.tone == config.ToneFormal }

func (r *Renderer) displayName(p domain.ProviderKind) string {
	switch p {
	case domain.ProviderCodex:
		return "Codex"
	case domain.ProviderClaude:
		return "Claude"
	case domain.ProviderAntigravity:
		return "Antigravity"
	case domain.ProviderGeminiCLI:
		return "Gemini CLI"
	case domain.ProviderOllama:
		return "Ollama"
	default:
		if p == "" {
			return "未知"
		}
		return string(p)
	}
}

func stateLabel(s domain.CredentialState) string {
	switch s {
	case domain.StateHealthy:
		return "正常"
	case domain.StateLimited:
		return "范围受限（局部额度）"
	case domain.StateNotice:
		return "注意"
	case domain.StateWarning:
		return "告警"
	case domain.StateExhausted:
		return "耗尽"
	case domain.StateInvalid:
		return "失效"
	case domain.StateSuspect:
		return "疑似异常"
	case domain.StateStale:
		return "数据过期"
	case domain.StateUnknown:
		return "未知"
	default:
		if s == "" {
			return "未知"
		}
		return string(s)
	}
}

func evidenceLabel(e domain.EvidenceLevel) string {
	switch e {
	case domain.EvidenceConfirmed:
		return "确认"
	case domain.EvidenceSuspected:
		return "疑似"
	default:
		return "未知"
	}
}

func severityLabel(s domain.Severity) string {
	switch s {
	case domain.SeverityUrgent:
		return "紧急"
	case domain.SeverityWarn:
		return "警告"
	case domain.SeverityInfo:
		return "提示"
	default:
		if s == "" {
			return "提示"
		}
		return string(s)
	}
}

// location is the display time zone. Values are never rewritten; only the
// rendering of an instant changes.
func (r *Renderer) location() *time.Location {
	if r.loc == nil {
		return time.Local
	}
	return r.loc
}

func (r *Renderer) formatTime(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	return t.In(r.location()).Format("2006-01-02 15:04")
}

// formatShort is the compact instant used inside a line. Like every other
// timestamp it is converted into the configured zone first: a wall clock with
// no zone is only readable if it is the reader's own wall clock.
func (r *Renderer) formatShort(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	return t.In(r.location()).Format("01-02 15:04")
}

func percent(v *float64) string {
	if v == nil {
		return "未上报"
	}
	return fmt.Sprintf("%.1f%%", *v)
}

// resetText is a Renderer method rather than a package function precisely so
// it cannot be called without a time zone. The previous package-level version
// printed the upstream's own wall clock, which made a window reset appear
// eight hours away from the recovery time quoted in the same message.
func (r *Renderer) resetText(w domain.QuotaWindow) string {
	if w.ResetAt != nil {
		return r.formatShort(*w.ResetAt)
	}
	if w.ResetText != "" {
		return w.ResetText
	}
	return "未上报"
}

// windowLine renders one window with its exact evidence.
//
// It uses the display label and the readable scope: the internal Name/ScopeID
// (e.g. "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1")
// are dedup and persistence keys, not text for a person.
func (r *Renderer) windowLine(w domain.QuotaWindow) string {
	return fmt.Sprintf("[%s] %s 已用 %s（重置 %s）", w.ScopeText(), w.DisplayLabel(), percent(w.UsedPercent), r.resetText(w))
}

// sourcesAndFetched aggregates provenance and freshness across a provider's
// snapshots.
//
// The returned time is the most recent *successful* observation, not the last
// attempt: a failed fetch's FetchedAt records when we failed, so showing it as
// the data time would present stale numbers as fresh.
func (r *Renderer) sourcesAndFetched(p domain.ProviderReport, fallback time.Time) (string, time.Time) {
	seen := map[string]bool{}
	var sources []string
	latest := time.Time{}
	for _, s := range p.Snapshots {
		if s.Source != "" && !seen[string(s.Source)] {
			seen[string(s.Source)] = true
			sources = append(sources, string(s.Source))
		}
		t := s.LastSuccessAt
		if t.IsZero() && s.OK {
			t = s.FetchedAt
		}
		if t.After(latest) {
			latest = t
		}
	}
	if len(sources) == 0 {
		sources = []string{"未知"}
	}
	if latest.IsZero() {
		latest = fallback
	}
	return strings.Join(sources, "+"), latest
}

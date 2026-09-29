// Package domain holds the shared vocabulary of the CPA quota watcher.
//
// Every other package depends on these types and must not redefine them.
package domain

import "time"

// ProviderKind identifies an upstream family tracked by the watcher.
type ProviderKind string

const (
	ProviderCodex       ProviderKind = "codex"
	ProviderClaude      ProviderKind = "claude"
	ProviderAntigravity ProviderKind = "antigravity"
	ProviderGeminiCLI   ProviderKind = "gemini-cli"
	ProviderOllama      ProviderKind = "ollama"
)

// Source records where a quota number came from.
type Source string

const (
	// SourceCPA is the official CPA v8 management API.
	SourceCPA   Source = "cpa-v8"
	SourceCPAV0 Source = "cpa-v0"
	// SourceOllamaWeb is the unofficial ollama.com settings page scrape.
	SourceOllamaWeb Source = "ollama-web"
)

// Confidence separates authoritative numbers from derived ones.
//
// ConfidenceReported values came straight from an upstream response.
// ConfidenceEstimated values were derived locally and must be labelled as such
// in every user-facing surface. ConfidenceUnknown means we have no number at
// all: never render it as 0% or 100%.
type Confidence string

const (
	ConfidenceReported  Confidence = "reported"
	ConfidenceEstimated Confidence = "estimated"
	ConfidenceUnknown   Confidence = "unknown"
)

// EvidenceLevel grades how firmly a diagnosis is supported.
type EvidenceLevel string

const (
	EvidenceConfirmed EvidenceLevel = "confirmed"
	EvidenceSuspected EvidenceLevel = "suspected"
	EvidenceUnknown   EvidenceLevel = "unknown"
)

// Credential is one tracked account/credential of a provider.
//
// Key is the stable identity used for state transitions and dedup. It must be
// stable across restarts and must never contain a secret or a directly
// reusable upstream handle: it is persisted to the state file and rendered
// into JSON APIs, so it has to be an opaque derivation (e.g. a hash of the
// auth index), not the auth index itself.
type Credential struct {
	Key      string       `json:"key"`
	Provider ProviderKind `json:"provider"`
	Alias    string       `json:"alias"`
	ShortID  string       `json:"short_id"`
	// AuthIndex is the live CPA handle. It is an internal transport field:
	// never serialise it, never render it, never persist it.
	AuthIndex string `json:"-"`
	Status    string `json:"status,omitempty"`
	Disabled  bool   `json:"disabled"`
	// Unavailable mirrors the CPA runtime flag, not our own judgement.
	Unavailable bool `json:"unavailable"`
}

// Label renders a credential for humans without leaking email or secrets.
func (c Credential) Label() string {
	switch {
	case c.Alias != "" && c.ShortID != "":
		return c.Alias + " · " + c.ShortID
	case c.Alias != "":
		return c.Alias
	case c.ShortID != "":
		return string(c.Provider) + " · " + c.ShortID
	default:
		return string(c.Provider)
	}
}

// QuotaScope is the explicitly reported applicability of a quota. Missing or
// unrecognised values mean unknown, never account. Names cannot establish scope.
type QuotaScope string

const (
	ScopeAccount QuotaScope = "account"
	ScopeModel   QuotaScope = "model"
	ScopeGroup   QuotaScope = "group"
	ScopeUnknown QuotaScope = "unknown"
)

func (s QuotaScope) Normalized() QuotaScope {
	switch s {
	case ScopeAccount, ScopeModel, ScopeGroup:
		return s
	default:
		return ScopeUnknown
	}
}

// QuotaWindow is one rolling usage window reported by an upstream.
//
// UsedPercent and ResetAt are pointers on purpose: nil means "not reported",
// which is different from zero.
type QuotaWindow struct {
	Scope QuotaScope `json:"scope"`
	// ScopeID is a safe model/group identifier, never an account ID or secret.
	// Account is local to the enclosing credential and needs no ScopeID.
	ScopeID     string     `json:"scope_id,omitempty"`
	Name        string     `json:"name"`
	UsedPercent *float64   `json:"used_percent,omitempty"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
	ResetText   string     `json:"reset_text,omitempty"`
}

func (w QuotaWindow) ScopeLabel() string {
	s := string(w.Scope.Normalized())
	if w.ScopeID != "" {
		s += ":" + w.ScopeID
	}
	return s
}

// FailureKind classifies why a fetch failed.
//
// Attribution depends on this being decided by the collector, which sees the
// HTTP status and error shape, instead of being guessed later from error text.
// A transport timeout and an exhausted quota must never collapse into the same
// string match.
type FailureKind string

const (
	FailureNone FailureKind = ""
	// FailureAuth is confirmed credential rejection (upstream 401/invalid
	// grant/revoked token/expired session cookie).
	FailureAuth FailureKind = "auth"
	// FailureQuota is a confirmed quota rejection reported as an error.
	FailureQuota FailureKind = "quota"
	// FailureTransport is a timeout, connection or upstream-gateway failure.
	// It is never evidence about the credential itself.
	FailureTransport FailureKind = "transport"
	// FailureUnsupported means the provider has no quota source at all.
	FailureUnsupported FailureKind = "unsupported"
	// FailureParse means we reached the upstream but could not understand the
	// payload. It is a defect signal for us, not a verdict on the credential.
	FailureParse FailureKind = "parse"
	// FailureControlPlane is a failure of CPA itself (bad management key,
	// management API unreachable). It must be attributed to the control plane,
	// never to an individual credential.
	FailureControlPlane FailureKind = "control_plane"
)

// QuotaSnapshot is one collection attempt for one credential.
//
// OK=true requires at least one usable quota window. A response we could parse
// but that carried no quota numbers is a failure, not a healthy reading:
// otherwise an upstream redesign would look like a permanently healthy account.
type QuotaSnapshot struct {
	Credential Credential    `json:"credential"`
	Windows    []QuotaWindow `json:"windows,omitempty"`
	Plan       string        `json:"plan,omitempty"`
	Balance    string        `json:"balance,omitempty"`
	Source     Source        `json:"source"`
	Confidence Confidence    `json:"confidence"`
	FetchedAt  time.Time     `json:"fetched_at"`
	OK         bool          `json:"ok"`
	Failure    FailureKind   `json:"failure,omitempty"`
	// FailureScope is required to attribute a quota rejection to the whole
	// credential. Unscoped rejections cannot prove account exhaustion.
	FailureScope   QuotaScope `json:"failure_scope,omitempty"`
	FailureScopeID string     `json:"failure_scope_id,omitempty"`
	Err            string     `json:"err,omitempty"`
	// Stale marks windows carried over from an older successful read so the
	// UI and cards can never present them as current.
	Stale bool `json:"stale,omitempty"`
	// LastSuccessAt is when these numbers were actually observed.
	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
}

// WorstUsedPercent returns the highest reported usage across windows.
// ok is false when no window reported a number.
func (s QuotaSnapshot) WorstUsedPercent() (value float64, window string, ok bool) {
	for _, w := range s.Windows {
		if w.UsedPercent == nil {
			continue
		}
		if !ok || *w.UsedPercent > value {
			value, window, ok = *w.UsedPercent, w.Name, true
		}
	}
	return value, window, ok
}

// CredentialState is the evaluated health of one credential.
type CredentialState string

const (
	StateHealthy CredentialState = "healthy"
	// StateLimited means only scoped capacity is known or a local scope is
	// constrained. It is not account health, account exhaustion or a parse error.
	StateLimited   CredentialState = "limited"
	StateNotice    CredentialState = "notice"
	StateWarning   CredentialState = "warning"
	StateExhausted CredentialState = "exhausted"
	// StateInvalid means the credential is confirmed broken (auth error).
	StateInvalid CredentialState = "invalid"
	// StateSuspect means repeated failures without a confirmed cause.
	StateSuspect CredentialState = "suspect"
	// StateStale means quota could not be read for several cycles.
	StateStale CredentialState = "stale"
	// StateUnknown means we never got a usable reading.
	StateUnknown CredentialState = "unknown"
)

// Severity drives notification urgency.
type Severity string

const (
	SeverityInfo   Severity = "info"
	SeverityWarn   Severity = "warn"
	SeverityUrgent Severity = "urgent"
)

// AlertKind enumerates the notification triggers agreed for the MVP.
type AlertKind string

const (
	AlertQuotaThreshold AlertKind = "quota_threshold"
	AlertQuotaExhausted AlertKind = "quota_exhausted"
	AlertCredential     AlertKind = "credential_invalid"
	AlertSuspect        AlertKind = "suspect_anomaly"
	AlertStale          AlertKind = "stale_data"
	AlertRecovered      AlertKind = "recovered"
	AlertQuotaReset     AlertKind = "quota_reset"
	AlertBootstrap      AlertKind = "bootstrap_summary"
)

// Alert is one emitted notification event.
type Alert struct {
	Scope      QuotaScope    `json:"scope,omitempty"`
	ScopeID    string        `json:"scope_id,omitempty"`
	Kind       AlertKind     `json:"kind"`
	Severity   Severity      `json:"severity"`
	Evidence   EvidenceLevel `json:"evidence"`
	Credential Credential    `json:"credential"`
	Title      string        `json:"title"`
	Detail     string        `json:"detail"`
	// Facts are short evidence lines shown verbatim to the reader.
	Facts []string `json:"facts,omitempty"`
	// Advice is the fixed remediation mapping; it never executes anything.
	Advice     string    `json:"advice,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Direction is the capacity advice for a provider.
type Direction string

const (
	DirectionUseMore Direction = "use_more"
	DirectionEaseOff Direction = "ease_off"
	DirectionSteady  Direction = "steady"
	DirectionReauth  Direction = "reauth"
)

// Recommendation is provider-level advice; it never names agent configs.
type Recommendation struct {
	Provider  ProviderKind `json:"provider"`
	Direction Direction    `json:"direction"`
	Reason    string       `json:"reason"`
}

// ProviderReport aggregates one provider for the summary card and status page.
//
// Healthy/Total are credential counts. We deliberately do not average usage
// percentages across accounts, because separate accounts are not additive.
type ProviderReport struct {
	Provider   ProviderKind    `json:"provider"`
	Healthy    int             `json:"healthy"`
	Total      int             `json:"total"`
	WorstState CredentialState `json:"worst_state"`
	// BestWindows describes the safest single credential, not a cross-account
	// merge: mixing windows from different accounts can invent a headroom that
	// no real credential has.
	BestWindows []QuotaWindow              `json:"best_windows,omitempty"`
	Snapshots   []QuotaSnapshot            `json:"snapshots,omitempty"`
	States      map[string]CredentialState `json:"states,omitempty"`
	// Error is a provider-wide collection failure (e.g. the credential list
	// could not be read). Individual credential failures stay in Snapshots.
	Error string `json:"error,omitempty"`
}

// Report is the full evaluated picture at one point in time.
type Report struct {
	GeneratedAt     time.Time        `json:"generated_at"`
	Providers       []ProviderReport `json:"providers"`
	Recommendations []Recommendation `json:"recommendations"`
	// Holiday describes the calendar context used for recommendations.
	Holiday HolidayContext `json:"holiday"`
	// Degraded means part of this picture is missing or stale, so the report
	// must not be presented as a complete healthy view.
	Degraded bool `json:"degraded,omitempty"`
	// Notes carry degradation reasons in reader-facing wording.
	Notes []string `json:"notes,omitempty"`
}

// HolidayContext is the calendar input to capacity advice.
type HolidayContext struct {
	Date           string `json:"date"`
	IsWorkday      bool   `json:"is_workday"`
	IsHoliday      bool   `json:"is_holiday"`
	Label          string `json:"label,omitempty"`
	DaysToNextWork int    `json:"days_to_next_workday"`
	// HolidayRunLength counts consecutive non-working days starting today.
	HolidayRunLength int `json:"holiday_run_length"`
}

// Message is a channel-agnostic outbound notification.
type Message struct {
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	Alerts []Alert `json:"alerts,omitempty"`
	Report *Report `json:"report,omitempty"`
	Kind   string  `json:"kind"`
}

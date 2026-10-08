// Package quota builds credential-scoped upstream quota requests and parses
// their responses. It performs no I/O and never accepts an upstream token.
package quota

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

var (
	ErrUnsupported    = errors.New("unsupported quota provider or profile")
	ErrMissingContext = errors.New("required quota account or project context is missing")
)

type Context struct {
	Provider  string
	AccountID string
	ProjectID string
	Profile   string
}

type Request struct {
	Method string
	URL    string
	Header map[string]string
	Data   string
}

type Result struct {
	Windows []domain.QuotaWindow
	Plan    string
	Balance string
	// ExtraUsage is the pay-as-you-go budget when the same response carries
	// one. It is never fetched separately: this package makes one request.
	ExtraUsage *domain.ExtraUsage
	Confidence domain.Confidence
	Failure    domain.FailureKind
	Err        string
	// Code is a short, displayable error code derived from the upstream HTTP
	// status (e.g. "401"). It is a status number, never response body text, so
	// it cannot leak a token or a payload.
	Code string
}

func profile(c Context) (string, error) {
	p := c.Profile
	if p == "" {
		p = "current"
	}
	switch c.Provider {
	case "codex", "claude", "gemini-cli":
		if p == "current" {
			return p, nil
		}
	case "antigravity":
		if p == "current" || p == "legacy" {
			return p, nil
		}
	}
	return "", ErrUnsupported
}

func safeContext(s string) bool {
	if strings.TrimSpace(s) == "" || strings.Contains(s, "$TOKEN$") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Build uses a single fixed upstream endpoint per profile. The management
// transport must supply auth_index separately and substitute the header token.
func Build(c Context) (Request, error) {
	p, err := profile(c)
	if err != nil {
		return Request{}, err
	}
	r := Request{Method: "GET", Header: map[string]string{
		"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json",
	}}
	switch c.Provider {
	case "codex":
		if !safeContext(c.AccountID) {
			return Request{}, ErrMissingContext
		}
		r.URL = "https://chatgpt.com/backend-api/wham/usage"
		r.Header["Chatgpt-Account-Id"] = strings.TrimSpace(c.AccountID)
		r.Header["User-Agent"] = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
	case "claude":
		r.URL = "https://api.anthropic.com/api/oauth/usage"
		r.Header["anthropic-beta"] = "oauth-2025-04-20"
	case "gemini-cli", "antigravity":
		if !safeContext(c.ProjectID) {
			return Request{}, ErrMissingContext
		}
		r.Method = "POST"
		body, _ := json.Marshal(struct {
			Project string `json:"project"`
		}{strings.TrimSpace(c.ProjectID)})
		r.Data = string(body)
		if c.Provider == "gemini-cli" {
			r.URL = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
		} else {
			// The first endpoint used by the pinned official management frontend.
			r.URL = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
			r.Header["User-Agent"] = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"
			if p == "legacy" {
				r.URL = "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"
				r.Header["User-Agent"] = "antigravity/1.11.5 windows/amd64"
			}
		}
	}
	return r, nil
}

func failure(kind domain.FailureKind, message string) Result {
	return Result{Confidence: domain.ConfidenceUnknown, Failure: kind, Err: message}
}

// failureStatus is a failure that also carries the upstream HTTP status as a
// displayable code. Only the status number reaches here, so the code is always
// a bounded digit string and never upstream text.
func failureStatus(kind domain.FailureKind, status int, message string) Result {
	r := failure(kind, message)
	r.Code = statusCode(status)
	return r
}

// statusCode renders a displayable error code, or "" when there is nothing
// honest to show. A transport failure (status <= 0) has no code: it never
// reached an upstream, and inventing "0" would be noise.
func statusCode(status int) string {
	if status < 100 || status > 599 {
		return ""
	}
	return strconv.Itoa(status)
}

// Parse receives the UPSTREAM status/body, not the CPA management envelope.
// status <= 0 denotes a transport failure (including timeout); no response body
// or context value is ever included in errors. Partial parses are discarded.
func Parse(c Context, status int, body []byte, now time.Time) Result {
	p, err := profile(c)
	if err != nil {
		return failure(domain.FailureUnsupported, ErrUnsupported.Error())
	}
	switch {
	case status == 401:
		return failureStatus(domain.FailureAuth, status, "upstream credential rejected")
	case status == 403:
		return failureStatus(domain.FailureTransport, status, "upstream access forbidden")
	case status == 429:
		// Never a verdict (not exhausted, not invalid), but the Code "429" is
		// kept so surfaces can show the snapshot as rate limited.
		return failureStatus(domain.FailureTransport, status, "upstream rate limited")
	case status <= 0 || status == 408 || status >= 500:
		// A transport failure (status <= 0) carries no code; the others do.
		return failureStatus(domain.FailureTransport, status, "upstream request failed or timed out")
	case status < 200 || status >= 300:
		return failureStatus(domain.FailureTransport, status, "upstream request unsuccessful")
	}
	if _, err := Build(c); err != nil {
		return failure(domain.FailureParse, "required quota context unavailable")
	}
	root, err := object(body)
	if err != nil {
		return failure(domain.FailureParse, "invalid upstream quota response")
	}
	// Never infer exhaustion from an error string or an unvetted error code.
	if v, ok := root["error"]; ok && !isNull(v) {
		return failure(domain.FailureParse, "upstream quota response contains an error")
	}
	var windows []domain.QuotaWindow
	var extra *domain.ExtraUsage
	switch c.Provider {
	case "codex":
		windows, err = parseCodex(root, now)
	case "claude":
		windows, err = parseClaude(root, now)
		if err == nil {
			extra, err = parseClaudeExtraUsage(root)
		}
	case "gemini-cli":
		windows, err = parseGemini(root, now)
	case "antigravity":
		if p == "legacy" {
			windows, err = parseAntigravityLegacy(root, now)
		} else {
			windows, err = parseAntigravity(root, now)
		}
	}
	if err != nil || len(windows) == 0 {
		return failure(domain.FailureParse, "upstream quota schema or numeric fields invalid")
	}
	r := Result{Windows: windows, ExtraUsage: extra, Confidence: domain.ConfidenceReported}
	if c.Provider == "codex" {
		// plan_type is the only subscription signal inside this response.
		// Claude's plan lives behind a separate profile request and Antigravity's
		// behind a separate subscription request, so neither is available here.
		r.Plan = text(root["plan_type"])
	}
	return r
}

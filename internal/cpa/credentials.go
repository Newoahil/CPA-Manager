package cpa

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/quota"
)

type credentialFile struct {
	AuthIndex     string          `json:"auth_index"`
	Name          string          `json:"name"`
	Provider      string          `json:"provider"`
	Status        string          `json:"status"`
	Disabled      bool            `json:"disabled"`
	Unavailable   bool            `json:"unavailable"`
	IDToken       json.RawMessage `json:"id_token"`
	AccountID     json.RawMessage `json:"chatgpt_account_id"`
	ProjectID     json.RawMessage `json:"project_id"`
	Account       json.RawMessage `json:"account"`
	AccountType   string          `json:"account_type"`
	SupportsQuota json.RawMessage `json:"supports_quota"`
	QuotaProvider json.RawMessage `json:"quota_provider"`
	QuotaProbe    json.RawMessage `json:"quota_probe"`
	// Cooldown data is optional runtime telemetry. It is kept raw so a
	// malformed value can be dropped on its own (see parseNextRetryAfter and
	// parseCooldowns) instead of failing the whole credential list.
	//
	// status_message is deliberately NOT declared here: it can carry verbatim
	// upstream response text and must never be read, stored, logged or rendered.
	NextRetryAfter json.RawMessage `json:"next_retry_after"`
	Cooldowns      json.RawMessage `json:"cooldowns"`
}

// maxCooldownText bounds every string copied out of a cooldown entry.
const maxCooldownText = 128

// parseNextRetryAfter reads an RFC3339 string. Anything else (absent, null,
// wrong type, unparseable) yields nil: the data is dropped, not an error.
func parseNextRetryAfter(raw json.RawMessage) *time.Time {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return parseRFC3339(s)
}

func parseRFC3339(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func cooldownText(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > maxCooldownText {
		r = r[:maxCooldownText]
	}
	return string(r)
}

// parseCooldowns decodes CPA's cooldown array. null/absent yields nil. A
// malformed array drops all cooldown data; a malformed entry drops only that
// entry. Only whitelisted scalar fields are copied.
func parseCooldowns(raw json.RawMessage) []domain.Cooldown {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []domain.Cooldown
	for _, item := range items {
		var e struct {
			Scope      string          `json:"scope"`
			ModelKey   string          `json:"model_key"`
			Reason     string          `json:"reason"`
			RetryAt    json.RawMessage `json:"retry_at"`
			HTTPStatus json.RawMessage `json:"http_status"`
		}
		if string(item) == "null" || json.Unmarshal(item, &e) != nil {
			continue
		}
		c := domain.Cooldown{
			Scope:    strings.ToLower(cooldownText(e.Scope)),
			ModelKey: cooldownText(e.ModelKey),
			Reason:   cooldownText(e.Reason),
		}
		var at string
		if len(e.RetryAt) > 0 && json.Unmarshal(e.RetryAt, &at) == nil {
			c.RetryAt = parseRFC3339(at)
		}
		var status int
		if len(e.HTTPStatus) > 0 && json.Unmarshal(e.HTTPStatus, &status) == nil && status >= 400 && status <= 599 {
			c.HTTPStatus = status
		}
		out = append(out, c)
	}
	return out
}

type credentialContext struct {
	provider          domain.ProviderKind
	quota             quota.Context
	contextInvalid    bool
	capabilityInvalid bool
	supportsQuota     bool
	quotaProvider     string
	probe             bool
}

type credentialPage struct {
	Files    []credentialFile `json:"files"`
	Page     *int             `json:"page"`
	PageSize *int             `json:"page_size"`
	Total    *int             `json:"total"`
	HasMore  *bool            `json:"has_more"`
}

func decodePage(body []byte) (credentialPage, error) {
	var p credentialPage
	if json.Unmarshal(body, &p) != nil || p.Files == nil {
		return p, errors.New("cpa: invalid credential list schema")
	}
	var envelope struct {
		Files []json.RawMessage `json:"files"`
	}
	_ = json.Unmarshal(body, &envelope)
	for i, raw := range envelope.Files {
		var entry map[string]json.RawMessage
		if json.Unmarshal(raw, &entry) != nil || entry == nil {
			return p, errors.New("cpa: invalid credential entry")
		}
		f := p.Files[i]
		if strings.TrimSpace(f.Name) == "" && (strings.TrimSpace(f.AuthIndex) == "" || strings.TrimSpace(f.Provider) == "") {
			return p, errors.New("cpa: unrecognized credential entry")
		}
		for _, field := range []string{"auth_index", "name", "provider", "status", "account_type"} {
			if v, ok := entry[field]; ok && string(v) == "null" {
				return p, errors.New("cpa: invalid credential field")
			}
		}
		for _, field := range []string{"disabled", "unavailable"} {
			if v, ok := entry[field]; ok && string(v) == "null" {
				return p, errors.New("cpa: invalid credential flag")
			}
		}
	}
	if (p.Page != nil && *p.Page < 1) || (p.PageSize != nil && *p.PageSize < 1) || (p.Total != nil && *p.Total < 0) {
		return p, errors.New("cpa: invalid credential pagination")
	}
	return p, nil
}

func (c *Client) list(ctx context.Context, selected string) ([]domain.Credential, map[string]credentialContext, string, error) {
	version := selected
	if version == "" {
		version = c.opts.APIVersion
	}
	auto := version == "auto"
	if auto {
		version = "v8"
	}
	path := "/credentials?page=1&page_size=100"
	if version == "v0" {
		path = "/auth-files"
	}
	body, status, err := c.do(ctx, version, http.MethodGet, path, nil)
	if err != nil {
		return nil, nil, "", err
	}
	if auto && status == http.StatusNotFound {
		version, path = "v0", "/auth-files"
		body, status, err = c.do(ctx, version, http.MethodGet, path, nil)
		if err != nil {
			return nil, nil, "", err
		}
	}
	if status != http.StatusOK {
		return nil, nil, "", statusError(status)
	}
	var out []domain.Credential
	contexts := make(map[string]credentialContext)
	seen := make(map[string]bool)
	seenPages := make(map[[32]byte]bool)
	aliases := make(map[string]map[string]bool)
	finish := func() ([]domain.Credential, map[string]credentialContext, string, error) {
		if err := c.applyOverrides(contexts, aliases); err != nil {
			return nil, nil, "", err
		}
		return out, contexts, version, nil
	}
	for page := 1; page <= 1000; page++ {
		p, err := decodePage(body)
		if err != nil {
			return nil, nil, "", err
		}
		if version == "v8" && (len(p.Files) > 100 || (p.PageSize != nil && *p.PageSize > 100)) {
			return nil, nil, "", errors.New("cpa: credential page size exceeds bound")
		}
		encoded, _ := json.Marshal(p.Files)
		fingerprint := sha256.Sum256(encoded)
		if seenPages[fingerprint] {
			return nil, nil, "", errors.New("cpa: repeated credential page")
		}
		seenPages[fingerprint] = true
		if version == "v8" && p.Page != nil && *p.Page != page {
			return nil, nil, "", errors.New("cpa: unexpected credential page")
		}
		for _, f := range p.Files {
			rawProvider := strings.TrimSpace(f.Provider)
			// Pinned old fork AccountInfo emits account_type=oauth for email
			// metadata; API-key entries emit api_key. Never alias generic gemini.
			if version == "v0" && mapProvider(f.Provider) == "gemini" && f.AccountType == "oauth" {
				f.Provider = string(domain.ProviderGeminiCLI)
			}
			cred := toCredential(f)
			if cred.AuthIndex != "" {
				if seen[cred.AuthIndex] {
					return nil, nil, "", errors.New("cpa: repeated credential handle in list")
				}
				seen[cred.AuthIndex] = true
				contexts[cred.AuthIndex] = c.contextFor(f)
				for _, key := range []string{cred.Key, opaqueKey(rawProvider, cred.AuthIndex)} {
					if aliases[key] == nil {
						aliases[key] = make(map[string]bool)
					}
					aliases[key][cred.AuthIndex] = true
				}
			}
			out = append(out, cred)
		}
		if version == "v0" {
			return finish()
		}
		more := false
		if p.HasMore != nil {
			more = *p.HasMore
		}
		if p.Total != nil {
			if len(out) > *p.Total {
				return nil, nil, "", errors.New("cpa: inconsistent credential total")
			}
			remaining := len(out) < *p.Total
			if p.HasMore != nil && more != remaining {
				return nil, nil, "", errors.New("cpa: inconsistent credential pagination")
			}
			more = remaining
		} else if p.HasMore == nil && p.PageSize != nil {
			more = len(p.Files) == *p.PageSize
		}
		if !more {
			return finish()
		}
		if len(p.Files) == 0 {
			return nil, nil, "", errors.New("cpa: credential pagination made no progress")
		}
		size := len(p.Files)
		if p.PageSize != nil {
			size = *p.PageSize
		}
		if size > 100 {
			return nil, nil, "", errors.New("cpa: credential page size exceeds bound")
		}
		if page == 1000 {
			break
		}
		body, status, err = c.do(ctx, version, http.MethodGet, fmt.Sprintf("/credentials?page=%d&page_size=%d", page+1, size), nil)
		if err != nil {
			return nil, nil, "", err
		}
		if status != http.StatusOK {
			return nil, nil, "", statusError(status)
		}
	}
	return nil, nil, "", errors.New("cpa: credential pagination limit exceeded")
}

func fieldText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return strings.TrimSpace(s), true
}

func mergeContext(a, b string) (string, bool) {
	if a != "" && b != "" && a != b {
		return "", false
	}
	if a != "" {
		return a, true
	}
	return b, true
}

func (c *Client) contextFor(f credentialFile) credentialContext {
	p := mapProvider(f.Provider)
	cc := credentialContext{provider: p, quota: quota.Context{Provider: string(p), Profile: "current"}}
	if p == domain.ProviderAntigravity {
		cc.quota.Profile = c.opts.AntigravityProfile
	}
	if p == domain.ProviderCodex {
		direct, ok := fieldText(f.AccountID)
		cc.contextInvalid = !ok
		var claims map[string]json.RawMessage
		if len(f.IDToken) > 0 && string(f.IDToken) != "null" {
			if json.Unmarshal(f.IDToken, &claims) != nil || claims == nil {
				cc.contextInvalid = true
			}
		}
		claim, ok := fieldText(claims["chatgpt_account_id"])
		cc.contextInvalid = cc.contextInvalid || !ok
		cc.quota.AccountID, ok = mergeContext(direct, claim)
		cc.contextInvalid = cc.contextInvalid || !ok
	}
	if p == domain.ProviderGeminiCLI || p == domain.ProviderAntigravity {
		project, ok := fieldText(f.ProjectID)
		cc.contextInvalid = !ok
		if p == domain.ProviderGeminiCLI {
			account, valid := fieldText(f.Account)
			cc.contextInvalid = cc.contextInvalid || !valid
			// Only the documented email (project) account representation qualifies.
			legacy := ""
			if match := legacyProjectAccount.FindStringSubmatch(account); len(match) == 2 {
				legacy = match[1]
			}
			project, ok = mergeContext(project, legacy)
			cc.contextInvalid = cc.contextInvalid || !ok
		}
		cc.quota.ProjectID = project
	}
	if len(f.SupportsQuota) > 0 {
		var b *bool
		if json.Unmarshal(f.SupportsQuota, &b) != nil || b == nil {
			cc.capabilityInvalid = true
		} else {
			cc.supportsQuota = *b
		}
	}
	var ok bool
	cc.quotaProvider, ok = fieldText(f.QuotaProvider)
	cc.capabilityInvalid = cc.capabilityInvalid || !ok
	if len(f.QuotaProbe) > 0 && string(f.QuotaProbe) != "null" {
		var probe map[string]json.RawMessage
		if json.Unmarshal(f.QuotaProbe, &probe) != nil || len(probe) == 0 {
			cc.capabilityInvalid = true
		} else {
			cc.probe = true
		}
	}
	return cc
}

var legacyProjectAccount = regexp.MustCompile(`^[^@\s()]+@[^@\s()]+ \(([A-Za-z0-9_.-]+)\)$`)

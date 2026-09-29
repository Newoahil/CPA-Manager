package cpa

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// ContextOverride supplies only non-token routing context, held in memory.
type ContextOverride struct {
	AccountID string `json:"account_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

var overrideKey = regexp.MustCompile(`^[A-Za-z0-9_-]+/[0-9a-f]{16}$`)

func safeContext(s string) bool {
	return s == strings.TrimSpace(s) && !strings.Contains(s, "$TOKEN$") && !strings.ContainsFunc(s, unicode.IsControl)
}

func validOverride(key string, v ContextOverride) bool {
	return overrideKey.MatchString(key) && (v.AccountID != "" || v.ProjectID != "") && safeContext(v.AccountID) && safeContext(v.ProjectID)
}

// ParseContextOverrides rejects unknown/duplicate fields and never echoes input.
func ParseContextOverrides(raw string) (map[string]ContextOverride, error) {
	out := make(map[string]ContextOverride)
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	bad := errors.New("invalid CPA_CONTEXT_OVERRIDES_JSON: expected unique opaque Keys with account_id/project_id strings")
	d := json.NewDecoder(strings.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, bad
	}
	for d.More() {
		tok, err = d.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, bad
		}
		if _, exists := out[key]; exists {
			return nil, bad
		}
		tok, err = d.Token()
		if err != nil || tok != json.Delim('{') {
			return nil, bad
		}
		fields := make(map[string]string)
		for d.More() {
			tok, err = d.Token()
			name, ok := tok.(string)
			if err != nil || !ok || (name != "account_id" && name != "project_id") {
				return nil, bad
			}
			if _, exists := fields[name]; exists {
				return nil, bad
			}
			tok, err = d.Token()
			value, ok := tok.(string)
			if err != nil || !ok || value == "" {
				return nil, bad
			}
			fields[name] = value
		}
		if tok, err = d.Token(); err != nil || tok != json.Delim('}') {
			return nil, bad
		}
		v := ContextOverride{AccountID: fields["account_id"], ProjectID: fields["project_id"]}
		if !validOverride(key, v) {
			return nil, bad
		}
		out[key] = v
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') {
		return nil, bad
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, bad
	}
	return out, nil
}

// Bind only after the complete list is known. A historical opaque Key is an
// alias, never a display name/email/short ID. Multiple matches are an error.
func (c *Client) applyOverrides(contexts map[string]credentialContext, aliases map[string]map[string]bool) error {
	for key, ov := range c.opts.ContextOverrides {
		matches := aliases[key]
		if len(matches) != 1 {
			return errors.New("cpa: context override Key missing or ambiguous")
		}
		for index := range matches {
			cc, ok := contexts[index]
			if !ok {
				return errors.New("cpa: context override has no addressable credential")
			}
			if (ov.AccountID != "" && cc.provider != domain.ProviderCodex) ||
				(ov.ProjectID != "" && cc.provider != domain.ProviderGeminiCLI && cc.provider != domain.ProviderAntigravity) {
				return errors.New("cpa: context override fields do not match credential provider")
			}
			var valid bool
			cc.quota.AccountID, valid = mergeContext(cc.quota.AccountID, ov.AccountID)
			cc.contextInvalid = cc.contextInvalid || !valid
			cc.quota.ProjectID, valid = mergeContext(cc.quota.ProjectID, ov.ProjectID)
			cc.contextInvalid = cc.contextInvalid || !valid
			if cc.contextInvalid {
				return errors.New("cpa: context override conflicts with credential metadata")
			}
			contexts[index] = cc
		}
	}
	return nil
}

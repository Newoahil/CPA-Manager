package quota

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

type fields map[string]json.RawMessage

// Occurrence suffixes preserve duplicate identities without making unique
// model/group names depend on unrelated array insertions or reordering.
type names map[string]int

func (n names) take(base string) string {
	n[base]++
	return base + "#" + strconv.Itoa(n[base])
}

var errShape = errors.New("invalid quota schema")

func isNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }

// Reject duplicate object keys, including nested keys, rather than trusting
// encoding/json's last-value-wins behaviour for contradictory quota numbers.
func object(raw []byte) (fields, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := checkJSON(d); err != nil {
		return nil, errShape
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errShape
	}
	var out fields
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, errShape
	}
	return out, nil
}

func checkJSON(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return errShape
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return errShape
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return errShape
			}
			seen[s] = true
			if err := checkJSON(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := checkJSON(d); err != nil {
				return err
			}
		}
	default:
		return errShape
	}
	_, err = d.Token()
	return err
}

func array(raw json.RawMessage) ([]json.RawMessage, error) {
	var out []json.RawMessage
	if json.Unmarshal(raw, &out) != nil || len(out) == 0 {
		return nil, errShape
	}
	return out, nil
}

func text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func number(raw json.RawMessage, max float64) (float64, error) {
	raw = bytes.TrimSpace(raw)
	// JSON numbers only. Numeric strings/percent strings are not this contract.
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, errShape
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > max {
		return 0, errShape
	}
	return value, nil
}

func window(name string, f fields, valueKey string, fraction bool, now time.Time, codex bool) (domain.QuotaWindow, error) {
	max := float64(100)
	if fraction {
		max = 1
	}
	n, err := number(f[valueKey], max)
	if err != nil {
		return domain.QuotaWindow{}, err
	}
	if fraction {
		n = (1 - n) * 100
	}
	w := domain.QuotaWindow{Name: name, UsedPercent: &n}
	if codex {
		// Absolute reset is Unix seconds, never inferred from digit count.
		if raw, ok := f["reset_at"]; ok && !isNull(raw) {
			n, err := number(raw, 253402300799)
			if err != nil || math.Trunc(n) != n {
				return domain.QuotaWindow{}, errShape
			}
			t := time.Unix(int64(n), 0).UTC()
			w.ResetAt = &t
		} else if raw, ok := f["reset_after_seconds"]; ok && !isNull(raw) {
			// Bound before duration conversion to avoid overflow.
			n, err := number(raw, float64(math.MaxInt64/int64(time.Second)))
			if err != nil || math.Trunc(n) != n || now.IsZero() {
				return domain.QuotaWindow{}, errShape
			}
			t := now.Add(time.Duration(int64(n)) * time.Second).UTC()
			if t.Year() < 1 || t.Year() > 9999 {
				return domain.QuotaWindow{}, errShape
			}
			w.ResetAt = &t
			w.ResetText = "derived from reset_after_seconds=" + strconv.FormatInt(int64(n), 10)
		}
	} else {
		key := "resetTime"
		if valueKey == "utilization" || valueKey == "percent" {
			key = "resets_at"
		}
		if raw, ok := f[key]; ok && !isNull(raw) {
			s := text(raw)
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return domain.QuotaWindow{}, errShape
			}
			t = t.UTC()
			w.ResetAt = &t
		}
	}
	return w, nil
}

func parseCodex(root fields, now time.Time) ([]domain.QuotaWindow, error) {
	var out []domain.QuotaWindow
	add := func(raw json.RawMessage, prefix string, primaryRequired bool, scope domain.QuotaScope, scopeID string) error {
		f, err := object(raw)
		if err != nil {
			return err
		}
		start := len(out)
		for _, key := range []string{"primary_window", "secondary_window"} {
			raw, ok := f[key]
			if !ok || isNull(raw) {
				if primaryRequired && key == "primary_window" {
					return errShape
				}
				continue
			}
			wf, err := object(raw)
			if err != nil {
				return err
			}
			w, err := window(prefix+"/"+key, wf, "used_percent", false, now, true)
			w.Scope, w.ScopeID = scope, scopeID
			if err != nil {
				return err
			}
			out = append(out, w)
		}
		if len(out) == start {
			return errShape
		}
		return nil
	}
	if err := add(root["rate_limit"], "codex/rate_limit", true, domain.ScopeAccount, ""); err != nil {
		return nil, err
	}
	if raw, ok := root["code_review_rate_limit"]; ok && !isNull(raw) {
		if err := add(raw, "codex/code_review_rate_limit", false, domain.ScopeGroup, "code_review"); err != nil {
			return nil, err
		}
	}
	if raw, ok := root["additional_rate_limits"]; ok && !isNull(raw) {
		var entries []json.RawMessage
		if json.Unmarshal(raw, &entries) != nil {
			return nil, errShape
		}
		identities := names{}
		for _, raw := range entries {
			f, err := object(raw)
			if err != nil {
				return nil, err
			}
			name := text(f["limit_name"])
			if name == "" {
				name = text(f["metered_feature"])
			}
			id := identities.take("codex/additional/" + url.PathEscape(name))
			if err := add(f["rate_limit"], id, false, domain.ScopeGroup, id); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func parseClaude(root fields, now time.Time) ([]domain.QuotaWindow, error) {
	var out []domain.QuotaWindow
	for _, key := range []string{"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet", "seven_day_cowork", "iguana_necktie"} {
		raw, ok := root[key]
		// Null optional windows are explicitly not offered for this account.
		if !ok || isNull(raw) {
			continue
		}
		f, err := object(raw)
		if err != nil {
			return nil, err
		}
		w, err := window("claude/"+key, f, "utilization", false, now, false)
		w.Scope, w.ScopeID = domain.ScopeGroup, key
		switch key {
		case "five_hour", "seven_day":
			w.Scope, w.ScopeID = domain.ScopeAccount, ""
		case "seven_day_opus":
			w.ScopeID = "opus"
		case "seven_day_sonnet":
			w.ScopeID = "sonnet"
		}
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if raw, ok := root["limits"]; ok && !isNull(raw) {
		var limits []json.RawMessage
		if json.Unmarshal(raw, &limits) != nil {
			return nil, errShape
		}
		identities := names{}
		for _, raw := range limits {
			f, err := object(raw)
			if err != nil {
				return nil, err
			}
			if text(f["kind"]) != "weekly_scoped" {
				continue
			}
			scope, err := object(f["scope"])
			if err != nil {
				return nil, err
			}
			model, err := object(scope["model"])
			if err != nil {
				return nil, err
			}
			name := strings.ToLower(text(model["display_name"]))
			if name != "fable" && name != "fable 5" {
				continue
			}
			w, err := window(identities.take("claude/limits/weekly_scoped/"+url.PathEscape(name)), f, "percent", false, now, false)
			w.Scope, w.ScopeID = domain.ScopeModel, url.PathEscape(name)
			if err != nil {
				return nil, err
			}
			out = append(out, w)
		}
	}
	return out, nil
}

func parseGemini(root fields, now time.Time) ([]domain.QuotaWindow, error) {
	entries, err := array(root["buckets"])
	if err != nil {
		return nil, err
	}
	var out []domain.QuotaWindow
	identities := names{}
	for _, raw := range entries {
		f, err := object(raw)
		if err != nil || text(f["modelId"]) == "" {
			return nil, errShape
		}
		name := identities.take("gemini-cli/models/" + url.PathEscape(text(f["modelId"])) + "/tokens/" + url.PathEscape(text(f["tokenType"])))
		w, err := window(name, f, "remainingFraction", true, now, false)
		w.Scope, w.ScopeID = domain.ScopeModel, url.PathEscape(text(f["modelId"]))
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

func parseAntigravity(root fields, now time.Time) ([]domain.QuotaWindow, error) {
	groups, err := array(root["groups"])
	if err != nil {
		return nil, err
	}
	var out []domain.QuotaWindow
	groupNames := names{}
	for _, raw := range groups {
		g, err := object(raw)
		if err != nil {
			return nil, err
		}
		buckets, err := array(g["buckets"])
		if err != nil {
			return nil, err
		}
		groupName := groupNames.take("antigravity/groups/" + url.PathEscape(text(g["displayName"])))
		bucketNames := names{}
		for _, raw := range buckets {
			f, err := object(raw)
			if err != nil {
				return nil, err
			}
			name := bucketNames.take(groupName + "/buckets/" + url.PathEscape(text(f["bucketId"])) + "/" + url.PathEscape(text(f["window"])))
			w, err := window(name, f, "remainingFraction", true, now, false)
			w.Scope, w.ScopeID = domain.ScopeGroup, groupName
			if err != nil {
				return nil, err
			}
			out = append(out, w)
		}
	}
	return out, nil
}

func parseAntigravityLegacy(root fields, now time.Time) ([]domain.QuotaWindow, error) {
	models, err := object(root["models"])
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(models))
	for key := range models {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []domain.QuotaWindow
	for _, key := range keys {
		m, err := object(models[key])
		if err != nil {
			return nil, err
		}
		// This endpoint is also a model catalogue. Entries not declaring
		// quotaInfo are not quota buckets, but declared malformed quota fails.
		raw, ok := m["quotaInfo"]
		if !ok {
			continue
		}
		f, err := object(raw)
		if err != nil {
			return nil, err
		}
		w, err := window("antigravity/models/"+url.PathEscape(key), f, "remainingFraction", true, now, false)
		w.Scope, w.ScopeID = domain.ScopeModel, url.PathEscape(key)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

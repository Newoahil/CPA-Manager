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

// optionalNumber reads a declared-optional numeric field.
//
// Absent or null means "not offered" and yields ok=false with no error. A
// declared but malformed value is an error, matching the existing rule that a
// malformed declared numeric field fails the whole result rather than being
// silently dropped.
func optionalNumber(f fields, key string, max float64) (float64, bool, error) {
	raw, present := f[key]
	if !present || isNull(raw) {
		return 0, false, nil
	}
	v, err := number(raw, max)
	if err != nil {
		return 0, false, errShape
	}
	return v, true, nil
}

// optionalFlag reads a declared-optional boolean. Only real JSON booleans are
// accepted; a string or number in this position is a schema change, not a
// value we may reinterpret.
func optionalFlag(f fields, key string) (bool, bool, error) {
	raw, present := f[key]
	if !present || isNull(raw) {
		return false, false, nil
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return false, false, errShape
	}
	return b, true, nil
}

// optionalText reads a declared-optional string. A non-string in this position
// is a schema change and fails.
func optionalText(f fields, key string) (string, bool, error) {
	raw, present := f[key]
	if !present || isNull(raw) {
		return "", false, nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false, errShape
	}
	s = strings.TrimSpace(s)
	return s, s != "", nil
}

// maxWindowSeconds bounds a reported rolling-window length at one year. A
// larger value is not a window we can describe and is treated as malformed.
const maxWindowSeconds = 366 * 24 * 60 * 60

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
		// The upstream states the rolling-window length explicitly. Reading it
		// is the only honest way to name the period: primary/secondary is an
		// ordering, not a duration, and the same account can report a weekly
		// or a monthly secondary window.
		secs, ok, err := optionalNumber(f, "limit_window_seconds", maxWindowSeconds)
		if err != nil || (ok && (math.Trunc(secs) != secs || secs <= 0)) {
			return domain.QuotaWindow{}, errShape
		}
		if ok {
			w.WindowSeconds = int64(secs)
		}
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

// Structural fallback names, used only when the upstream did not report the
// window length. They describe position, not a period, because primary/
// secondary is an ordering: the same account can report a weekly or a monthly
// secondary window.
var codexWindowLabels = map[string]string{
	"primary_window":   "主额度窗口",
	"secondary_window": "次额度窗口",
}

// Window lengths the upstream uses, per the pinned frontend's classifier.
const (
	fiveHourSeconds = 5 * 60 * 60
	weekSeconds     = 7 * 24 * 60 * 60
	minMonthSeconds = 28 * 24 * 60 * 60
	maxMonthSeconds = 31 * 24 * 60 * 60
)

// windowPeriodName turns a reported window length into a reader-facing period.
//
// The exact 5-hour and 7-day lengths and the 28..31-day month band are the
// upstream's own classification. Anything else is converted arithmetically
// rather than forced into one of those buckets, so a future 3-day window reads
// as "3天窗口" instead of being mislabelled weekly.
func windowPeriodName(seconds int64) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds == fiveHourSeconds:
		return "5小时窗口"
	case seconds == weekSeconds:
		return "周窗口"
	case seconds >= minMonthSeconds && seconds <= maxMonthSeconds:
		return "月窗口"
	case seconds%(24*60*60) == 0:
		return strconv.FormatInt(seconds/(24*60*60), 10) + "天窗口"
	case seconds%(60*60) == 0:
		return strconv.FormatInt(seconds/(60*60), 10) + "小时窗口"
	case seconds%60 == 0:
		return strconv.FormatInt(seconds/60, 10) + "分钟窗口"
	default:
		return strconv.FormatInt(seconds, 10) + "秒窗口"
	}
}

func parseCodex(root fields, now time.Time) ([]domain.QuotaWindow, error) {
	var out []domain.QuotaWindow
	add := func(raw json.RawMessage, prefix, group string, primaryRequired bool, scope domain.QuotaScope, scopeID string) error {
		f, err := object(raw)
		if err != nil {
			return err
		}
		// allowed=false and limit_reached=true are the upstream's explicit
		// "at the limit" markers. They are recorded as a flag and never turned
		// into a percentage: a marker is not a measurement, and inventing 100%
		// from one would fabricate evidence the response does not contain.
		allowed, hasAllowed, err := optionalFlag(f, "allowed")
		if err != nil {
			return err
		}
		reached, hasReached, err := optionalFlag(f, "limit_reached")
		if err != nil {
			return err
		}
		limitReached := (hasReached && reached) || (hasAllowed && !allowed)
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
			w.LimitReached = limitReached
			period := windowPeriodName(w.WindowSeconds)
			if period == "" {
				period = codexWindowLabels[key]
			}
			w.Label = group + " · " + period
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
	if err := add(root["rate_limit"], "codex/rate_limit", "账号", true, domain.ScopeAccount, ""); err != nil {
		return nil, err
	}
	if raw, ok := root["code_review_rate_limit"]; ok && !isNull(raw) {
		if err := add(raw, "codex/code_review_rate_limit", "代码评审", false, domain.ScopeGroup, "code_review"); err != nil {
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
			group := name
			if group == "" {
				group = "其它额度"
			}
			if err := add(f["rate_limit"], id, group, false, domain.ScopeGroup, id); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// claudeWindowLabels names the fixed Claude windows for readers. The key
// itself states the period, so no duration is invented here.
var claudeWindowLabels = map[string]string{
	"five_hour":            "账号 · 5小时",
	"seven_day":            "账号 · 7天",
	"seven_day_oauth_apps": "OAuth 应用 · 7天",
	"seven_day_opus":       "Opus 模型 · 7天",
	"seven_day_sonnet":     "Sonnet 模型 · 7天",
	"seven_day_cowork":     "Cowork · 7天",
}

// parseClaudeExtraUsage reads the pay-as-you-go budget that ships inside the
// same usage response.
//
// The amounts are "credits"; the payload names no currency, so nothing is
// converted and no money symbol is ever attached. A declared but malformed
// block fails the whole result, consistent with every other declared field.
// An absent block simply means the account has no extra-usage budget.
func parseClaudeExtraUsage(root fields) (*domain.ExtraUsage, error) {
	raw, present := root["extra_usage"]
	if !present || isNull(raw) {
		return nil, nil
	}
	f, err := object(raw)
	if err != nil {
		return nil, err
	}
	enabled, _, err := optionalFlag(f, "is_enabled")
	if err != nil {
		return nil, err
	}
	out := &domain.ExtraUsage{Enabled: enabled}
	// Credit amounts are unbounded by any percentage rule, so they only have
	// to be finite and non-negative; number() already enforces that.
	if v, ok, err := optionalNumber(f, "used_credits", math.MaxFloat64); err != nil {
		return nil, err
	} else if ok {
		out.UsedCredits = &v
	}
	if v, ok, err := optionalNumber(f, "monthly_limit", math.MaxFloat64); err != nil {
		return nil, err
	} else if ok {
		out.MonthlyLimit = &v
	}
	// utilization is a percentage on the same 0..100 scale as the windows.
	if v, ok, err := optionalNumber(f, "utilization", 100); err != nil {
		return nil, err
	} else if ok {
		out.UsedPercent = &v
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
		w.Label = claudeWindowLabels[key]
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
			w.Label = name + " 模型 · 周"
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
		w.Label = text(f["modelId"]) + " 模型"
		if tokenType := text(f["tokenType"]); tokenType != "" {
			w.Label += " · " + tokenType
		}
		if err != nil {
			return nil, err
		}
		// remainingAmount is an upstream string whose unit is the tokenType
		// already named in the label. It is carried verbatim: converting it
		// would require a unit the payload never states.
		amount, _, aerr := optionalText(f, "remainingAmount")
		if aerr != nil {
			return nil, aerr
		}
		w.RemainingAmount = amount
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
		display := text(g["displayName"])
		groupName := groupNames.take("antigravity/groups/" + url.PathEscape(display))
		bucketNames := names{}
		for _, raw := range buckets {
			f, err := object(raw)
			if err != nil {
				return nil, err
			}
			bucketID, windowKey := text(f["bucketId"]), text(f["window"])
			name := bucketNames.take(groupName + "/buckets/" + url.PathEscape(bucketID) + "/" + url.PathEscape(windowKey))
			w, err := window(name, f, "remainingFraction", true, now, false)
			w.Scope, w.ScopeID = domain.ScopeGroup, groupName
			// The bucket carries its own display name upstream; preferring it
			// over the slug keeps the label in the vendor's own words.
			bucketDisplay, _, derr := optionalText(f, "displayName")
			if derr != nil {
				return nil, derr
			}
			w.Label = antigravityLabel(display, firstNonEmpty(bucketDisplay, bucketID), windowKey)
			if err != nil {
				return nil, err
			}
			out = append(out, w)
		}
	}
	return out, nil
}

// windowPeriodLabels translates the upstream window token. The accepted
// spellings mirror the pinned frontend's own table; unrecognised tokens are
// passed through verbatim rather than guessed at.
var windowPeriodLabels = map[string]string{
	"hourly": "小时", "daily": "日", "monthly": "月",
	"weekly": "周", "week": "周",
	"5h": "5小时", "five-hour": "5小时", "five_hour": "5小时",
}

// antigravityLabel keeps the upstream's own display name and appends the
// period. The bucket name is only added when it says something the window
// token does not already say, so "Gemini Models · gemini-weekly · 周"
// collapses to "Gemini Models · 周".
func antigravityLabel(display, bucket, windowKey string) string {
	parts := make([]string, 0, 3)
	if display = strings.TrimSpace(display); display != "" {
		parts = append(parts, display)
	}
	bucket = strings.TrimSpace(bucket)
	windowKey = strings.TrimSpace(windowKey)
	if bucket != "" && (windowKey == "" || !strings.Contains(strings.ToLower(bucket), strings.ToLower(windowKey))) {
		parts = append(parts, bucket)
	}
	if windowKey != "" {
		period := windowPeriodLabels[strings.ToLower(windowKey)]
		if period == "" {
			period = windowKey
		}
		parts = append(parts, period)
	}
	return strings.Join(parts, " · ")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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
		w.Label = key + " 模型"
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

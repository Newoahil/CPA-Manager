// Package web serves the read-only status surface.
//
// It is intentionally write-free: there is no configuration form, no action
// endpoint and no mutation of CPA state. It also never renders credentials,
// cookies, tokens, management keys or email addresses.
//
// Safety is structural, not cosmetic: both the page and the JSON API project
// the report through an explicit allow-list DTO (label, provider, state,
// windows, source, confidence, freshness, last-success time, advice). The
// domain structures are never serialised directly, so fields such as
// AuthIndex (json:"-") or any future upstream payload cannot leak by accident.
// A light redaction pass still runs over the free-text fields as defence in
// depth.
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// NewHandler returns the read-only HTTP handler.
//
// version is embedded in the page footer for operators; refresher supplies the
// latest evaluated report. The handler only ever reads from it.
func NewHandler(refresher domain.QuotaRefresher, version string) http.Handler {
	h := &handler{refresher: refresher, version: version}
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handleIndex)
	mux.HandleFunc("/api/status", h.handleStatus)
	mux.HandleFunc("/healthz", h.handleHealthz)
	return mux
}

type handler struct {
	refresher domain.QuotaRefresher
	version   string
}

func (h *handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (h *handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	report, ok := h.currentReport(r.Context())
	if !ok {
		// No cycle has succeeded yet: say so explicitly rather than emit a
		// zero-valued report that reads like "everything is 0%".
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(statusDTO{Available: false, Message: "尚无评估结果"})
		return
	}
	_ = json.NewEncoder(w).Encode(buildStatus(report))
}

func (h *handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	report, ok := h.currentReport(r.Context())
	data := pageData{Version: h.version, Status: statusDTO{Available: ok}}
	if ok {
		data.Status = buildStatus(report)
	}

	// Render into a buffer first: once a partial page has been written, an
	// execution error can only produce a half response. Buffering lets us
	// return a clean 500 instead.
	var buf bytes.Buffer
	if err := indexTemplate.Execute(&buf, data); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// currentReport prefers the last evaluated report and never blocks on upstreams.
func (h *handler) currentReport(ctx context.Context) (domain.Report, bool) {
	_ = ctx
	if h.refresher == nil {
		return domain.Report{}, false
	}
	return h.refresher.LastReport()
}

// ---------------------------------------------------------------------------
// Allow-list view models (shared by the page and the JSON API)
// ---------------------------------------------------------------------------

// statusDTO is the only shape either surface exposes.
type statusDTO struct {
	Available       bool                `json:"available"`
	Message         string              `json:"message,omitempty"`
	GeneratedAt     string              `json:"generated_at,omitempty"`
	Degraded        bool                `json:"degraded"`
	Notes           []string            `json:"notes,omitempty"`
	Holiday         holidayDTO          `json:"holiday"`
	Providers       []providerDTO       `json:"providers"`
	Recommendations []recommendationDTO `json:"recommendations"`
}

type holidayDTO struct {
	Date             string `json:"date"`
	IsWorkday        bool   `json:"is_workday"`
	IsHoliday        bool   `json:"is_holiday"`
	Label            string `json:"label,omitempty"`
	DaysToNextWork   int    `json:"days_to_next_workday"`
	HolidayRunLength int    `json:"holiday_run_length"`
}

type providerDTO struct {
	Provider string `json:"provider"`
	Healthy  int    `json:"healthy"`
	Total    int    `json:"total"`
	// Normal/Limited/Abnormal is the display counting. Healthy stays for
	// compatibility but is not shown on its own: a credential measured only at
	// model/group scope is never "healthy", so "0 / 3" misread three usable
	// accounts as three broken ones.
	Normal          int             `json:"normal"`
	Limited         int             `json:"limited"`
	Abnormal        int             `json:"abnormal"`
	WorstState      string          `json:"worst_state"`
	WorstStateLabel string          `json:"worst_state_label"`
	StateClass      string          `json:"-"`
	Error           string          `json:"error,omitempty"`
	BestWindows     []windowDTO     `json:"best_windows,omitempty"`
	Credentials     []credentialDTO `json:"credentials"`
	Advice          string          `json:"advice,omitempty"`
	Direction       string          `json:"direction,omitempty"`
}

type credentialDTO struct {
	Label          string             `json:"label"`
	Provider       string             `json:"provider"`
	State          string             `json:"state"`
	StateLabel     string             `json:"state_label"`
	StateClass     string             `json:"-"`
	Failure        domain.FailureKind `json:"failure,omitempty"`
	Source         string             `json:"source"`
	Confidence     string             `json:"confidence"`
	ConfidenceText string             `json:"confidence_text,omitempty"`
	Freshness      string             `json:"freshness"`
	FetchedAt      string             `json:"fetched_at,omitempty"`
	LastSuccessAt  string             `json:"last_success_at,omitempty"`
	Stale          bool               `json:"stale"`
	StaleNote      string             `json:"stale_note,omitempty"`
	// Plan is the upstream-reported subscription tier, empty when the response
	// carried none. No placeholder is substituted.
	Plan       string         `json:"plan,omitempty"`
	ExtraUsage *extraUsageDTO `json:"extra_usage,omitempty"`
	Windows    []windowDTO    `json:"windows,omitempty"`
}

type windowDTO struct {
	Scope   domain.QuotaScope `json:"scope"`
	ScopeID string            `json:"scope_id,omitempty"`
	Name    string            `json:"name"`
	// Label and ScopeText are the reader-facing forms. Name/ScopeID stay in
	// the JSON for machine consumers but are no longer what the page prints.
	Label       string   `json:"label"`
	ScopeText   string   `json:"scope_text"`
	UsedPercent *float64 `json:"used_percent,omitempty"`
	UsedText    string   `json:"used_text"`
	ResetAt     string   `json:"reset_at,omitempty"`
	ResetText   string   `json:"reset_text,omitempty"`
	// WindowSeconds is the upstream-reported rolling window length; zero means
	// it was not reported and no period may be inferred.
	WindowSeconds int64 `json:"window_seconds,omitempty"`
	// LimitReached is the upstream's own marker, independent of UsedPercent.
	LimitReached    bool   `json:"limit_reached,omitempty"`
	RemainingAmount string `json:"remaining_amount,omitempty"`
	Stale           bool   `json:"stale"`
}

// extraUsageDTO is pay-as-you-go spend in the upstream's own credit unit. The
// payload names no currency, so neither does this.
type extraUsageDTO struct {
	Enabled      bool     `json:"enabled"`
	UsedCredits  *float64 `json:"used_credits,omitempty"`
	MonthlyLimit *float64 `json:"monthly_limit,omitempty"`
	UsedPercent  *float64 `json:"used_percent,omitempty"`
	Text         string   `json:"text,omitempty"`
}

type recommendationDTO struct {
	Provider  string `json:"provider"`
	Direction string `json:"direction"`
	Reason    string `json:"reason"`
}

func buildStatus(report domain.Report) statusDTO {
	out := statusDTO{
		Available:   true,
		GeneratedAt: report.GeneratedAt.Format(time.RFC3339),
		Degraded:    report.Degraded,
		Holiday: holidayDTO{
			Date:             redact(report.Holiday.Date),
			IsWorkday:        report.Holiday.IsWorkday,
			IsHoliday:        report.Holiday.IsHoliday,
			Label:            redact(report.Holiday.Label),
			DaysToNextWork:   report.Holiday.DaysToNextWork,
			HolidayRunLength: report.Holiday.HolidayRunLength,
		},
	}
	for _, n := range report.Notes {
		out.Notes = append(out.Notes, redact(n))
	}

	advice := map[domain.ProviderKind]domain.Recommendation{}
	for _, rec := range report.Recommendations {
		advice[rec.Provider] = rec
		out.Recommendations = append(out.Recommendations, recommendationDTO{
			Provider:  string(rec.Provider),
			Direction: string(rec.Direction),
			Reason:    redact(rec.Reason),
		})
	}

	ordered := append([]domain.ProviderReport(nil), report.Providers...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Provider < ordered[j].Provider })

	for _, pr := range ordered {
		pd := providerDTO{
			Provider:   string(pr.Provider),
			Healthy:    pr.Healthy,
			Total:      pr.Total,
			Normal:     pr.Normal,
			Limited:    pr.Limited,
			Abnormal:   pr.Abnormal,
			WorstState: string(pr.WorstState),
			Error:      redact(pr.Error),
		}
		if pd.WorstState == "" {
			pd.WorstState = string(domain.StateUnknown)
		}
		pd.WorstStateLabel, pd.StateClass = statePresentation(pd.WorstState)
		for _, w := range pr.BestWindows {
			pd.BestWindows = append(pd.BestWindows, toWindowDTO(w, false))
		}
		if rec, ok := advice[pr.Provider]; ok {
			pd.Advice = redact(rec.Reason)
			pd.Direction = string(rec.Direction)
		}
		for _, s := range pr.Snapshots {
			state := string(pr.States[s.Credential.Key])
			if state == "" {
				state = string(domain.StateUnknown)
			}
			// A stale snapshot's FetchedAt records when we failed; showing it
			// as the data time would present old numbers as fresh. Use the
			// real last-success time instead.
			dataTime := s.FetchedAt
			if s.Stale && !s.LastSuccessAt.IsZero() {
				dataTime = s.LastSuccessAt
			}
			cd := credentialDTO{
				Label:          redact(s.Credential.Label()),
				Provider:       string(s.Credential.Provider),
				State:          state,
				Source:         string(s.Source),
				Confidence:     confidenceName(s.Confidence),
				ConfidenceText: confidenceText(s.Confidence),
				Freshness:      freshnessName(s.Stale),
				FetchedAt:      formatTime(dataTime),
				LastSuccessAt:  formatTime(s.LastSuccessAt),
				Stale:          s.Stale,
			}
			if s.Stale {
				cd.StaleNote = staleNote(s.LastSuccessAt)
			}
			cd.Plan = redact(strings.TrimSpace(s.Plan))
			if s.ExtraUsage.Reportable() && !s.Stale {
				cd.ExtraUsage = toExtraUsageDTO(*s.ExtraUsage)
			}
			cd.StateLabel, cd.StateClass = statePresentation(cd.State)
			cd.Failure = s.Failure
			if s.Failure == domain.FailureUnsupported {
				cd.StateLabel += " · unsupported（额度能力不可用）"
				if !s.Stale {
					cd.Freshness = "unsupported"
				}
			}
			for _, w := range s.Windows {
				cd.Windows = append(cd.Windows, toWindowDTO(w, s.Stale))
			}
			pd.Credentials = append(pd.Credentials, cd)
		}
		out.Providers = append(out.Providers, pd)
	}
	return out
}

// Reuse the existing notice badge style for limited coverage; no new layout or
// CSS is needed, and the machine-readable state remains "limited".
func statePresentation(s string) (label, class string) {
	if s == string(domain.StateLimited) {
		return "limited · 范围受限（局部额度）", "notice"
	}
	return s, s
}

// toExtraUsageDTO renders the budget in the upstream's own credit unit.
func toExtraUsageDTO(e domain.ExtraUsage) *extraUsageDTO {
	d := &extraUsageDTO{
		Enabled: e.Enabled, UsedCredits: e.UsedCredits,
		MonthlyLimit: e.MonthlyLimit, UsedPercent: e.UsedPercent,
	}
	var parts []string
	switch {
	case e.UsedCredits != nil && e.MonthlyLimit != nil:
		parts = append(parts, "已用 "+trimAmount(*e.UsedCredits)+" / 上限 "+trimAmount(*e.MonthlyLimit)+" credits")
	case e.UsedCredits != nil:
		parts = append(parts, "已用 "+trimAmount(*e.UsedCredits)+" credits")
	case e.MonthlyLimit != nil:
		parts = append(parts, "月度上限 "+trimAmount(*e.MonthlyLimit)+" credits")
	}
	if e.UsedPercent != nil {
		parts = append(parts, trimPercent(*e.UsedPercent))
	}
	d.Text = strings.Join(parts, " · ")
	return d
}

func toWindowDTO(w domain.QuotaWindow, stale bool) windowDTO {
	d := windowDTO{
		Scope: w.Scope.Normalized(), ScopeID: redact(w.ScopeID),
		Name:            redact(w.Name),
		Label:           redact(w.DisplayLabel()),
		ScopeText:       redact(w.ScopeText()),
		ResetText:       redact(w.ResetText),
		WindowSeconds:   w.WindowSeconds,
		LimitReached:    w.LimitReached,
		RemainingAmount: redact(w.RemainingAmount),
		Stale:           stale,
	}
	if w.UsedPercent == nil {
		// Unknown, explicitly not zero.
		d.UsedText = "未知"
	} else {
		v := *w.UsedPercent
		d.UsedPercent = &v
		d.UsedText = trimPercent(v)
	}
	if w.ResetAt != nil {
		d.ResetAt = w.ResetAt.Format(time.RFC3339)
	}
	return d
}

func confidenceName(c domain.Confidence) string {
	switch c {
	case domain.ConfidenceReported, domain.ConfidenceEstimated, domain.ConfidenceUnknown:
		return string(c)
	default:
		return string(domain.ConfidenceUnknown)
	}
}

// confidenceText makes the estimated label explicit for the reader.
func confidenceText(c domain.Confidence) string {
	switch c {
	case domain.ConfidenceEstimated:
		return "估算"
	case domain.ConfidenceReported:
		return "已上报"
	default:
		return "未知"
	}
}

func freshnessName(stale bool) string {
	if stale {
		return "stale"
	}
	return "fresh"
}

func staleNote(lastSuccess time.Time) string {
	if lastSuccess.IsZero() {
		return "数据已过期（最后成功：从未成功）"
	}
	return "数据已过期（最后成功：" + lastSuccess.Format(time.RFC3339) + "）"
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func trimPercent(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", v), "0"), ".") + "%"
}

// trimAmount prints a credit amount without inventing precision or a currency.
func trimAmount(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ---------------------------------------------------------------------------
// Redaction (defence in depth on top of the allow-list projection)
// ---------------------------------------------------------------------------

var (
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// Long opaque tokens: 24+ chars of base64/hex-ish characters. Cookie and
	// session values fall in here.
	secretRE = regexp.MustCompile(`[A-Za-z0-9_\-\.]{24,}`)
)

// redact strips emails and opaque secrets from any user-visible string. The
// status page must be safe to screenshot.
func redact(s string) string {
	s = emailRE.ReplaceAllString(s, "[redacted-email]")
	s = secretRE.ReplaceAllString(s, "[redacted-secret]")
	return s
}

// ---------------------------------------------------------------------------
// View wrapper and template
// ---------------------------------------------------------------------------

type pageData struct {
	Version string
	Status  statusDTO
}

// ---------------------------------------------------------------------------
// Self-contained template (inline CSS, no external resources, no JS)
// ---------------------------------------------------------------------------

var indexTemplate = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CPA 额度状态</title>
<style>
:root { color-scheme: light dark; }
body { font-family: system-ui, -apple-system, "Segoe UI", sans-serif; margin: 1.5rem; line-height: 1.5; }
h1 { font-size: 1.4rem; }
.meta { color: #666; font-size: .85rem; margin-bottom: 1rem; }
.card { border: 1px solid #ccc; border-radius: 8px; padding: 1rem; margin-bottom: 1rem; }
.card h2 { margin: 0 0 .5rem; font-size: 1.1rem; }
.state { display: inline-block; padding: .1rem .5rem; border-radius: 4px; font-size: .8rem; color: #fff; }
.healthy { background: #2e7d32; }
.notice { background: #f9a825; color: #222; }
.warning { background: #ef6c00; }
.exhausted, .invalid { background: #c62828; }
.suspect, .stale { background: #6a1b9a; }
.unknown { background: #757575; }
table { border-collapse: collapse; width: 100%; margin: .5rem 0; }
th, td { border: 1px solid #ddd; padding: .3rem .5rem; text-align: left; font-size: .9rem; }
.stale-note { background: #fff3cd; border: 1px solid #ffe08a; color: #664d03; padding: .5rem; border-radius: 6px; }
.advice { background: #eef; border-left: 3px solid #66c; padding: .5rem; margin-top: .5rem; }
.empty { color: #888; }
footer { margin-top: 2rem; color: #888; font-size: .8rem; }
</style>
</head>
<body>
<h1>CPA 额度状态（只读）</h1>
<div class="meta">版本 {{.Version}}{{if .Status.Available}} · 生成时间 {{.Status.GeneratedAt}}{{end}}</div>
{{if not .Status.Available}}
<p class="empty">尚无评估结果，请等待首次采集完成。</p>
{{else}}
{{if .Status.Degraded}}<div class="stale-note">报告降级：{{range .Status.Notes}}{{.}} {{end}}</div>{{end}}
<p>节假日：{{.Status.Holiday.Date}} · {{.Status.Holiday.Label}} · 距离下个工作日 {{.Status.Holiday.DaysToNextWork}} 天 · 连续非工作日 {{.Status.Holiday.HolidayRunLength}} 天</p>
{{range .Status.Providers}}
<div class="card">
<h2>{{.Provider}} <span class="state {{.StateClass}}">{{.WorstStateLabel}}</span></h2>
<p>正常 {{.Normal}} · 受限 {{.Limited}} · 异常 {{.Abnormal}} · 共 {{.Total}}</p>
{{if .Error}}<div class="stale-note">采集故障：{{.Error}}</div>{{end}}
{{if .BestWindows}}<p>最佳窗口（该凭证各窗口用量）：{{range .BestWindows}}{{.Label}} {{.UsedText}}（重置 {{if .ResetAt}}{{.ResetAt}}{{else}}未知{{end}}） {{end}}</p>{{end}}
<table>
<thead><tr><th>凭证</th><th>状态</th><th>窗口</th><th>适用范围</th><th>已用</th><th>重置时间</th><th>来源</th><th>获取时间</th></tr></thead>
<tbody>
{{range .Credentials}}
{{$cred := .}}
{{if .Stale}}<tr><td colspan="8" class="stale-note">{{.StaleNote}}</td></tr>{{end}}
{{if .ExtraUsage}}<tr><td colspan="8">额外用量：{{.ExtraUsage.Text}}</td></tr>{{end}}
{{range .Windows}}
<tr>
<td>{{$cred.Label}}{{if $cred.Plan}} · 套餐 {{$cred.Plan}}{{end}}</td>
<td><span class="state {{$cred.StateClass}}">{{$cred.StateLabel}}</span></td>
<td>{{.Label}}</td>
<td>{{.ScopeText}}</td>
<td>{{if .Stale}}<em>{{.UsedText}}（陈旧）</em>{{else}}{{.UsedText}}{{end}}{{if .RemainingAmount}} · 剩余 {{.RemainingAmount}}{{end}}{{if .LimitReached}} · 上游标记已达上限{{end}}</td>
<td>{{if .ResetAt}}{{.ResetAt}}{{else if .ResetText}}{{.ResetText}}{{else}}未知{{end}}</td>
<td>{{$cred.Source}}（{{$cred.ConfidenceText}}）</td>
<td>{{$cred.FetchedAt}}</td>
</tr>
{{end}}
{{end}}
</tbody>
</table>
{{if .Advice}}<div class="advice">建议（{{.Direction}}）：{{.Advice}}</div>{{end}}
<p class="empty">不展示 cookie、token、管理密钥或邮箱；不提供任何写操作。</p>
</div>
{{end}}
{{end}}
<footer>只读状态页 · 不包含配置表单 · 无外部资源</footer>
</body>
</html>
`))

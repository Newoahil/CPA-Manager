package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// sensitive is injected into every field the page could plausibly render. The
// tests assert none of it survives into a response body.
const (
	secretCookie = "__Secure-session=abcdefghijklmnopqrstuvwxyz0123456789"
	secretToken  = "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload.signature"
	secretEmail  = "operator@example.com"
	secretManage = "cpa-management-key-super-secret-value-1234567890"
)

// fakeRefresher is a read-only domain.QuotaRefresher stub.
type fakeRefresher struct {
	report domain.Report
	ok     bool
}

func (f *fakeRefresher) RefreshNow(context.Context) (domain.Report, error) {
	return f.report, nil
}
func (f *fakeRefresher) LastReport() (domain.Report, bool) { return f.report, f.ok }

func reportWithSecrets() domain.Report {
	used5h := 40.0
	used7d := 96.5
	reset := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fetched := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	return domain.Report{
		GeneratedAt: fetched,
		Degraded:    true,
		Notes:       []string{"采集失败：" + secretEmail},
		Holiday: domain.HolidayContext{
			Date:             "2026-05-30",
			IsWorkday:        false,
			IsHoliday:        true,
			Label:            "周末 " + secretEmail,
			DaysToNextWork:   2,
			HolidayRunLength: 2,
		},
		Providers: []domain.ProviderReport{
			{
				Provider:   domain.ProviderCodex,
				Healthy:    0,
				Total:      1,
				WorstState: domain.StateWarning,
				BestWindows: []domain.QuotaWindow{
					{Name: "5h", UsedPercent: &used5h, ResetAt: &reset},
					{Name: "7d", UsedPercent: &used7d, ResetAt: &reset},
				},
				States: map[string]domain.CredentialState{"codex:key-" + secretCookie: domain.StateWarning},
				Snapshots: []domain.QuotaSnapshot{
					{
						Credential: domain.Credential{
							Key:      "codex:key-" + secretCookie,
							Provider: domain.ProviderCodex,
							Alias:    secretEmail,
							ShortID:  "abc123",
							Status:   secretToken,
						},
						Windows: []domain.QuotaWindow{
							{Name: "5h", UsedPercent: &used5h, ResetAt: &reset, ResetText: "3小时后重置"},
							{Name: "7d", UsedPercent: &used7d, ResetAt: &reset, ResetText: "7天后重置"},
						},
						Plan:       secretManage,
						Source:     domain.SourceCPA,
						Confidence: domain.ConfidenceReported,
						FetchedAt:  fetched,
						OK:         true,
					},
				},
			},
		},
		Recommendations: []domain.Recommendation{
			{Provider: domain.ProviderCodex, Direction: domain.DirectionEaseOff, Reason: "7d 窗口已用 96.5% " + secretEmail},
		},
	}
}

func assertNoSecrets(t *testing.T, body string) {
	t.Helper()
	for _, s := range []string{secretCookie, secretToken, secretEmail, secretManage, "abcdefghijklmnopqrst"} {
		if strings.Contains(body, s) {
			t.Errorf("response leaked sensitive string %q", s)
		}
	}
}

// TestRoutesStatusCodes covers all three routes with a populated report.
func TestRoutesStatusCodes(t *testing.T) {
	h := NewHandler(&fakeRefresher{report: reportWithSecrets(), ok: true}, "test-1")

	cases := []struct {
		path string
		want int
	}{
		{"/", http.StatusOK},
		{"/api/status", http.StatusOK},
		{"/healthz", http.StatusOK},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}

// TestNoSecretsInResponses is the core safety assertion for every route.
func TestNoSecretsInResponses(t *testing.T) {
	h := NewHandler(&fakeRefresher{report: reportWithSecrets(), ok: true}, "test-1")
	for _, path := range []string{"/", "/api/status", "/healthz", "/unknown"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assertNoSecrets(t, rec.Body.String())
	}
}

// TestNoWriteSurface asserts the page and API reject non-read methods.
func TestNoWriteSurface(t *testing.T) {
	h := NewHandler(&fakeRefresher{report: reportWithSecrets(), ok: true}, "test-1")
	for _, path := range []string{"/", "/api/status", "/healthz"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			req := httptest.NewRequest(method, path, strings.NewReader("x=1"))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", method, path, rec.Code)
			}
		}
	}
}

// TestNoRefresherYet covers the pre-first-cycle state: API must not fabricate a
// zero-valued report.
func TestNoRefresherYet(t *testing.T) {
	h := NewHandler(&fakeRefresher{ok: false}, "test-1")

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if payload["available"] != false {
		t.Errorf("available = %v, want false", payload["available"])
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "尚无评估结果") {
		t.Error("empty page should state that no evaluation exists yet")
	}
}

// TestRowsRenderFullyForMultipleWindows is the render-failure regression: the
// inner range must bind the credential explicitly, so every window row appears
// with the credential label and state.
func TestRowsRenderFullyForMultipleWindows(t *testing.T) {
	h := NewHandler(&fakeRefresher{report: reportWithSecrets(), ok: true}, "test-1")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "render failed") {
		t.Fatalf("page reported a render failure:\n%s", body)
	}
	if strings.Contains(strings.ToLower(body), "can't evaluate field") {
		t.Fatalf("template execution error leaked into the body:\n%s", body)
	}
	// Both windows must appear as complete rows: label + state + window + source.
	for _, want := range []string{"abc123", "warning", "5h", "7d", "cpa-v8"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q:\n%s", want, body)
		}
	}
	// A plainly reported number carries no confidence label: "已上报" was on
	// every row and meant nothing to the reader.
	if strings.Contains(body, "已上报") {
		t.Errorf("internal confidence jargon rendered on the page:\n%s", body)
	}
	// The credential label (short id survives) must appear once per window row.
	if n := strings.Count(body, "abc123"); n < 2 {
		t.Errorf("credential label rendered %d times, want at least one per window", n)
	}
}

// TestDegradedNotesAndConfidenceRendered covers fix #9's surface requirements.
func TestDegradedNotesAndConfidenceRendered(t *testing.T) {
	rep := reportWithSecrets()
	report := domain.Report{Providers: rep.Providers, Degraded: true, Notes: []string{"额度数据已过期"}}
	h := NewHandler(&fakeRefresher{report: report, ok: true}, "test-1")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"降级", "额度数据已过期"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// Evidence quality is not dropped, only reworded: a plainly reported number
	// gets no label, while estimated and unreadable ones still say so.
	if strings.Contains(body, "已上报") {
		t.Errorf("internal confidence jargon rendered on the page:\n%s", body)
	}
	if got := confidenceNote(domain.ConfidenceEstimated); got != "估算值" {
		t.Errorf("estimated confidence lost its warning: %q", got)
	}
	if got := confidenceNote(domain.ConfidenceUnknown); got != "取不到数据" {
		t.Errorf("unknown confidence lost its warning: %q", got)
	}
	if got := confidenceNote(domain.ConfidenceReported); got != "" {
		t.Errorf("reported confidence should be silent, got %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var payload statusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("status JSON: %v", err)
	}
	if !payload.Degraded || len(payload.Notes) == 0 {
		t.Errorf("degraded/notes missing from API: %+v", payload)
	}
	if payload.Providers[0].Credentials[0].ConfidenceText == "" {
		t.Error("confidence text missing from API credential DTO")
	}
}

// TestEstimatedConfidenceLabelled ensures an estimated value is explicitly
// marked as 估算.
func TestEstimatedConfidenceLabelled(t *testing.T) {
	rep := reportWithSecrets()
	rep.Providers[0].Snapshots[0].Confidence = domain.ConfidenceEstimated
	h := NewHandler(&fakeRefresher{report: rep, ok: true}, "test-1")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "估算") {
		t.Error("estimated confidence must be labelled 估算")
	}
}

// TestUnknownRenderedNotAsZero verifies a nil percentage is shown as 未知, never
// as 0%, on the HTML surface.
func TestUnknownRenderedNotAsZero(t *testing.T) {
	rep := domain.Report{
		GeneratedAt: time.Now(),
		Providers: []domain.ProviderReport{{
			Provider:   domain.ProviderOllama,
			Total:      1,
			WorstState: domain.StateUnknown,
			States:     map[string]domain.CredentialState{"ollama:x": domain.StateUnknown},
			Snapshots: []domain.QuotaSnapshot{{
				Credential: domain.Credential{Key: "ollama:x", Provider: domain.ProviderOllama},
				Windows:    []domain.QuotaWindow{{Name: "5h"}},
				Source:     domain.SourceOllamaWeb,
				FetchedAt:  time.Now(),
				OK:         false,
				Stale:      true,
				Err:        "timeout",
			}},
		}},
	}
	h := NewHandler(&fakeRefresher{report: rep, ok: true}, "test-1")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "未知") {
		t.Error("nil percent must render as 未知")
	}
	if strings.Contains(body, ">0%<") {
		t.Error("nil percent must not render as 0%")
	}
	if !strings.Contains(body, "数据已过期") {
		t.Error("failed snapshot should be marked as stale data")
	}
}

// TestStaleSnapshotShowsLastSuccessTime: the page must not present the failed
// attempt time as the data time.
func TestStaleSnapshotShowsLastSuccessTime(t *testing.T) {
	failedAt := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	lastOK := time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC)
	generated := time.Date(2026, 6, 2, 13, 0, 0, 0, time.UTC)
	used := 96.0
	rep := domain.Report{
		GeneratedAt: generated,
		Degraded:    true,
		Notes:       []string{"额度数据已过期"},
		Providers: []domain.ProviderReport{{
			Provider:   domain.ProviderCodex,
			Total:      1,
			WorstState: domain.StateWarning,
			States:     map[string]domain.CredentialState{"codex:x": domain.StateWarning},
			Snapshots: []domain.QuotaSnapshot{{
				Credential:    domain.Credential{Key: "codex:x", Provider: domain.ProviderCodex, ShortID: "abc123"},
				Windows:       []domain.QuotaWindow{{Name: "7d", UsedPercent: &used}},
				Source:        domain.SourceCPA,
				Confidence:    domain.ConfidenceUnknown,
				FetchedAt:     failedAt,
				LastSuccessAt: lastOK,
				OK:            false,
				Stale:         true,
			}},
		}},
	}
	h := NewHandler(&fakeRefresher{report: rep, ok: true}, "test-1")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// Times are rendered in the configured display zone, so the expectation is
	// the same instant expressed the way the page expresses it.
	if !strings.Contains(body, formatHuman(lastOK)) {
		t.Errorf("page should show the real last-success time (%s)", formatHuman(lastOK))
	}
	if strings.Contains(body, formatHuman(failedAt)) {
		t.Error("page presented the failed attempt time as the data time")
	}
	// The JSON contract keeps RFC3339, now carrying the display offset.
	if !strings.Contains(formatTime(lastOK), "+08:00") && !strings.Contains(formatTime(lastOK), "Z") {
		t.Errorf("JSON timestamp lost its zone: %q", formatTime(lastOK))
	}
	if !strings.Contains(body, "数据已过期") {
		t.Error("stale snapshot not marked on the page")
	}
}

// TestDTOExcludesSensitiveDomainFields proves the whitelist projection, not a
// regex, is what protects the API: a credential with a live AuthIndex and a raw
// error string must not surface either field.
func TestDTOExcludesSensitiveDomainFields(t *testing.T) {
	rep := reportWithSecrets()
	rep.Providers[0].Snapshots[0].Credential.AuthIndex = secretCookie
	rep.Providers[0].Snapshots[0].Err = "upstream: " + secretToken
	h := NewHandler(&fakeRefresher{report: rep, ok: true}, "test-1")

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	assertNoSecrets(t, body)
	if strings.Contains(body, "auth_index") || strings.Contains(body, "authIndex") {
		t.Error("API exposed the auth index field")
	}
	if strings.Contains(body, "\"err\"") {
		t.Error("API serialised the raw snapshot error field")
	}
	// The allow-listed fields must still be present.
	for _, want := range []string{"\"label\"", "\"state\"", "\"windows\"", "\"confidence_text\"", "\"freshness\""} {
		if !strings.Contains(body, want) {
			t.Errorf("API DTO missing %s", want)
		}
	}
}

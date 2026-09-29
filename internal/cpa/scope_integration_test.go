package cpa_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/cpa"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/evaluate"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

// Exercise the actual management decoder and evaluator, rather than constructing
// a scoped snapshot that could hide transport scope loss or inference.
func TestNormalizedMeasurementsThroughEvaluate(t *testing.T) {
	for _, fraction := range []int{0, 1} {
		t.Run(fmt.Sprint(fraction), func(t *testing.T) {
			body := fmt.Sprintf(`{"subscription":{"plan":"fixture-plan"},"summary":[{"key":"credit_balance","value":12.5,"currency":"USD"}],"groups":[{"displayName":"account","buckets":[{"window":"model/global","remainingFraction":%d}]}]}`, fraction)
			snap := fetchNormalized(t, body)
			if !snap.OK || snap.Failure != domain.FailureNone || snap.Confidence != domain.ConfidenceReported || snap.Err != "" || len(snap.Windows) != 1 || snap.Plan != "fixture-plan" || snap.Balance != "12.5 USD" {
				t.Fatalf("successful measurement lost: %+v", snap)
			}
			assertUnknownWindow(t, snap.Windows[0], float64((1-fraction)*100))
			engine := evaluate.New(config.Config{
				Location:          time.UTC,
				DefaultThresholds: config.Thresholds{Notice: 90, Warn: 95, Urgent: 100},
			}, nil)
			previous := state.Empty()
			previous.Bootstrapped = true
			report, alerts, next := engine.Evaluate(previous, []domain.QuotaSnapshot{snap}, snap.FetchedAt)
			if next.Credentials[snap.Credential.Key].State == domain.StateExhausted || len(report.Providers) != 1 || len(report.Providers[0].Snapshots) != 1 {
				t.Fatalf("unknown scope became whole-account exhaustion: %+v", report)
			}
			shown := report.Providers[0].Snapshots[0]
			if !shown.OK || len(shown.Windows) != 1 {
				t.Fatalf("report dropped valid measurement: %+v", shown)
			}
			assertUnknownWindow(t, shown.Windows[0], float64((1-fraction)*100))
			if len(report.Recommendations) != 1 || report.Recommendations[0].Direction != domain.DirectionSteady || !strings.Contains(report.Recommendations[0].Reason, "unknown") {
				t.Fatalf("broad provider advice from unknown scope: %+v", report.Recommendations)
			}
			if fraction == 0 {
				if len(alerts) != 1 || alerts[0].Kind != domain.AlertQuotaThreshold || alerts[0].Scope != domain.ScopeUnknown || alerts[0].ScopeID != "" || alerts[0].Evidence != domain.EvidenceUnknown || !strings.Contains(alerts[0].Advice, "不能确认整凭证耗尽或建议停用") {
					t.Fatalf("unsafe or missing unknown-window alert: %+v", alerts)
				}
			} else if len(alerts) != 0 {
				t.Fatalf("zero usage generated alert: %+v", alerts)
			}
		})
	}
}

func TestNormalizedMixedInvalidMeasurementsFailEntireSnapshot(t *testing.T) {
	for _, invalid := range []string{`{}`, `null`, `{"remainingFraction":null}`, `{"remainingFraction":"0.5"}`, `{"remainingFraction":-0.1}`, `{"remainingFraction":1.1}`} {
		for _, buckets := range []string{invalid + `,{"remainingFraction":0}`, `{"remainingFraction":1},` + invalid} {
			snap := fetchNormalized(t, `{"subscription":{"plan":"fixture-plan"},"groups":[{"buckets":[`+buckets+`]}]}`)
			if snap.OK || snap.Failure != domain.FailureParse || snap.Confidence != domain.ConfidenceUnknown || len(snap.Windows) != 0 || snap.Plan != "" || snap.Balance != "" || snap.FailureScope != domain.ScopeUnknown || snap.FailureScopeID != "" {
				t.Fatalf("partial or inferred result for %s: %+v", buckets, snap)
			}
		}
	}
}

func assertUnknownWindow(t *testing.T, w domain.QuotaWindow, used float64) {
	t.Helper()
	if w.Scope != domain.ScopeUnknown || w.ScopeID != "" || w.UsedPercent == nil || *w.UsedPercent != used {
		t.Fatalf("incorrect numeric or scope semantics: %+v", w)
	}
}

func fetchNormalized(t *testing.T, body string) domain.QuotaSnapshot {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.RequestURI() {
		case "GET /v8/management/credentials?page=1&page_size=100":
			_, _ = w.Write([]byte(`{"files":[{"auth_index":"fixture-handle","provider":"claude","name":"fixture"}]}`))
		case "POST /v8/management/credentials/quota/fetch":
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client := cpa.NewWithOptions(srv.URL, "fixture-key", time.Second, cpa.Options{APIVersion: "v8", QuotaStrategy: "normalized"})
	creds, err := client.ListCredentials(context.Background())
	if err != nil || len(creds) != 1 {
		t.Fatalf("list: count=%d err=%v", len(creds), err)
	}
	snap, err := client.FetchQuota(context.Background(), creds[0])
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/notify/render"
)

func sampleMessage() domain.Message {
	return domain.Message{Title: "额度日报", Body: "一切正常", Kind: "daily"}
}

func TestNotifyPostsJSON(t *testing.T) {
	var gotBody []byte
	var gotMethod, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, srv.Client())
	if n.Name() != "webhook" {
		t.Fatalf("Name() = %q, want webhook", n.Name())
	}
	if err := n.Notify(context.Background(), sampleMessage()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	var decoded payload
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("body is not the whitelist payload: %v\n%s", err, gotBody)
	}
	if decoded.Title != "额度日报" || decoded.Kind != "daily" {
		t.Errorf("projection mismatch: %+v", decoded)
	}
	if !strings.Contains(decoded.Body, "一切正常") {
		t.Errorf("rendered body missing: %q", decoded.Body)
	}
}

func TestNotifyNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	err := New(srv.URL, srv.Client()).Notify(context.Background(), sampleMessage())
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry status and body, got: %v", err)
	}
}

func TestNotifyRespectsCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(srv.URL, srv.Client()).Notify(ctx, sampleMessage()); err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestNotifyNilClientFallsBack(t *testing.T) {
	if New("http://127.0.0.1:1/", nil) == nil {
		t.Fatal("New returned nil")
	}
}

// TestProjectionIsWhitelist guards the external contract: internal handles and
// raw upstream strings must never be serialised.
func TestProjectionIsWhitelist(t *testing.T) {
	const (
		secretCookie = "SUPERSECRET_COOKIE_abc123"
		internalKey  = "codex/hash-deadbeef"
		authIndex    = "auth-index-should-not-leak"
		errText      = "upstream error with token sk-live-xyz789"
	)
	used := 99.0
	msg := domain.Message{
		Title: "额度日报",
		Kind:  "daily",
		Report: &domain.Report{
			Degraded: true,
			Notes:    []string{"控制面不可达"},
			Providers: []domain.ProviderReport{{
				Provider:   domain.ProviderCodex,
				WorstState: domain.StateExhausted,
				Healthy:    0,
				Total:      1,
				Error:      "control plane down",
				BestWindows: []domain.QuotaWindow{
					{Name: "5h", UsedPercent: &used},
				},
				Snapshots: []domain.QuotaSnapshot{{
					Credential: domain.Credential{
						Key:       internalKey,
						Provider:  domain.ProviderCodex,
						Alias:     "主号",
						ShortID:   "a1b2",
						AuthIndex: authIndex,
					},
					Err:        errText,
					OK:         false,
					Confidence: domain.ConfidenceUnknown,
				}},
			}},
		},
		Alerts: []domain.Alert{{
			Kind:     domain.AlertQuotaExhausted,
			Severity: domain.SeverityUrgent,
			Evidence: domain.EvidenceConfirmed,
			Credential: domain.Credential{
				Key:       internalKey,
				Provider:  domain.ProviderCodex,
				Alias:     "主号",
				ShortID:   "a1b2",
				AuthIndex: authIndex,
			},
			Title:  "额度已耗尽",
			Detail: "detail",
			Facts:  []string{"5h 窗口已用 99%"},
			Advice: "暂停使用",
		}},
	}

	raw, err := json.Marshal(project(render.New(""), msg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(raw)

	for _, leak := range []string{internalKey, authIndex, errText, secretCookie, "auth_index", "\"snapshots\"", "management_key"} {
		if strings.Contains(out, leak) {
			t.Errorf("projection leaked internal value %q:\n%s", leak, out)
		}
	}
	// The whitelist fields must be present.
	for _, want := range []string{"额度日报", "codex", "quota_exhausted", "主号 · a1b2", "暂停使用", "degraded", "控制面不可达"} {
		if !strings.Contains(out, want) {
			t.Errorf("projection missing %q:\n%s", want, out)
		}
	}
}

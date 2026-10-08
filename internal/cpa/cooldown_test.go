package cpa

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func listBody(entries ...string) string {
	return `{"files":[` + strings.Join(entries, ",") + `]}`
}

func listFixture(t *testing.T, body string) ([]domain.Credential, error) {
	t.Helper()
	c := newTestClient(t, Options{APIVersion: "v8"}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v8/management/credentials" {
			unexpected(t, w, r)
			return
		}
		writeJSON(w, body)
	})
	return c.ListCredentials(context.Background())
}

func TestCooldownParsing(t *testing.T) {
	creds, err := listFixture(t, listBody(`{
		"auth_index":"a1","provider":"codex","name":"one@example.invalid",
		"status":"error","unavailable":true,
		"status_message":"upstream said: fixture-secret-token",
		"next_retry_after":"2026-10-08T10:15:00Z",
		"cooldowns":[
			{"scope":"model","model_key":"gpt-x","reason":"quota","retry_at":"2026-10-08T10:15:00Z","remaining_seconds":252,"backoff_level":1,"http_status":429},
			{"scope":"credential","reason":"quota","retry_at":"2026-10-08T10:20:00+00:00","http_status":200}
		]}`))
	if err != nil || len(creds) != 1 {
		t.Fatalf("err=%v creds=%d", err, len(creds))
	}
	c := creds[0]
	want := time.Date(2026, 10, 8, 10, 15, 0, 0, time.UTC)
	if c.NextRetryAfter == nil || !c.NextRetryAfter.Equal(want) {
		t.Fatalf("next_retry_after = %v", c.NextRetryAfter)
	}
	if len(c.Cooldowns) != 2 {
		t.Fatalf("cooldowns = %+v", c.Cooldowns)
	}
	m := c.Cooldowns[0]
	if m.Scope != "model" || m.ModelKey != "gpt-x" || m.Reason != "quota" || m.HTTPStatus != 429 || m.RetryAt == nil || !m.RetryAt.Equal(want) {
		t.Fatalf("model cooldown = %+v", m)
	}
	cr := c.Cooldowns[1]
	if cr.Scope != "credential" || cr.ModelKey != "" || cr.HTTPStatus != 0 || cr.RetryAt == nil {
		t.Fatalf("credential cooldown = %+v (http_status outside 400-599 must be dropped)", cr)
	}
	// status_message must never reach any field of the credential.
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "fixture-secret-token") || strings.Contains(fmt.Sprintf("%+v", c), "fixture-secret-token") {
		t.Fatal("status_message leaked into the credential")
	}
}

func TestCooldownNullAndAbsent(t *testing.T) {
	creds, err := listFixture(t, listBody(
		`{"auth_index":"a1","provider":"codex","name":"one","cooldowns":null,"next_retry_after":null}`,
		`{"auth_index":"a2","provider":"codex","name":"two"}`,
		`{"auth_index":"a3","provider":"codex","name":"three","cooldowns":[]}`,
	))
	if err != nil || len(creds) != 3 {
		t.Fatalf("err=%v creds=%d", err, len(creds))
	}
	for _, c := range creds {
		if len(c.Cooldowns) != 0 || c.NextRetryAfter != nil {
			t.Fatalf("%s: unexpected cooldown data %+v", c.Alias, c)
		}
	}
}

func TestMalformedCooldownDataDropsOnlyThatData(t *testing.T) {
	creds, err := listFixture(t, listBody(
		`{"auth_index":"a1","provider":"codex","name":"bad-array","unavailable":true,"cooldowns":"nope","next_retry_after":12345}`,
		`{"auth_index":"a2","provider":"codex","name":"bad-items","next_retry_after":"yesterday","cooldowns":[
			null, 7, {"scope":5}, {"scope":"model","model_key":"m","retry_at":"not-a-time","http_status":"429"},
			{"scope":"model","model_key":"ok","retry_at":"2026-10-08T10:15:00Z","http_status":99999}
		]}`,
		`{"auth_index":"a3","provider":"codex","name":"healthy","next_retry_after":"2026-10-08T10:15:00Z"}`,
	))
	if err != nil || len(creds) != 3 {
		t.Fatalf("a malformed cooldown must not fail the list: err=%v creds=%d", err, len(creds))
	}
	if !creds[0].Unavailable || len(creds[0].Cooldowns) != 0 || creds[0].NextRetryAfter != nil {
		t.Fatalf("bad-array = %+v", creds[0])
	}
	b := creds[1]
	if b.NextRetryAfter != nil {
		t.Fatalf("unparseable next_retry_after kept: %v", b.NextRetryAfter)
	}
	if len(b.Cooldowns) != 2 {
		t.Fatalf("bad-items cooldowns = %+v", b.Cooldowns)
	}
	if b.Cooldowns[0].ModelKey != "m" || b.Cooldowns[0].RetryAt != nil || b.Cooldowns[0].HTTPStatus != 0 {
		t.Fatalf("entry with bad fields = %+v", b.Cooldowns[0])
	}
	if b.Cooldowns[1].ModelKey != "ok" || b.Cooldowns[1].RetryAt == nil || b.Cooldowns[1].HTTPStatus != 0 {
		t.Fatalf("entry with out-of-range status = %+v", b.Cooldowns[1])
	}
	if creds[2].NextRetryAfter == nil {
		t.Fatal("healthy credential lost its next_retry_after")
	}
}

func TestStrictChecksStillApplyWithCooldownFields(t *testing.T) {
	_, err := listFixture(t, listBody(`{"auth_index":"a1","provider":"codex","name":"x","unavailable":null,"cooldowns":[]}`))
	if err == nil {
		t.Fatal("null unavailable must still fail the list")
	}
}

func TestQuotaRateLimitKeepsCode429AndNoVerdict(t *testing.T) {
	c := newTestClient(t, Options{APIVersion: "v8", QuotaStrategy: "proxy"}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v8/management/credentials" && r.Method == "GET":
			writeJSON(w, claudeList)
		case r.URL.Path == "/v8/management/requests/api-call" && r.Method == "POST":
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 429, "header": map[string][]string{}, "body": "fixture-secret rate limited"})
		default:
			unexpected(t, w, r)
		}
	})
	snap, err := c.FetchQuota(context.Background(), credential(t, c))
	if err != nil || snap.OK || snap.Failure != domain.FailureTransport || snap.Code != "429" {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if len(snap.Windows) != 0 || snap.FailureScope != domain.ScopeUnknown || strings.Contains(snap.Err, "fixture-secret") {
		t.Fatalf("429 must stay a non-verdict without payload text: %+v", snap)
	}
}

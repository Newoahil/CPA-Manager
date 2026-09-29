package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func fixtureServer(t *testing.T, status int, body string) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cookie"); got == "" {
			t.Errorf("missing cookie header")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := New(5 * time.Second)
	c.url = srv.URL
	return c, srv
}

func windowByName(t *testing.T, ws []domain.QuotaWindow, name string) domain.QuotaWindow {
	t.Helper()
	for _, w := range ws {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("window %q missing in %+v", name, ws)
	return domain.QuotaWindow{}
}

func TestFetchNormal(t *testing.T) {
	const page = `<!doctype html><html><body>
	<div class="plan-card">Plan: Pro</div>
	<section class="usage">
		<h3>Rolling hourly usage</h3>
		<div><span>42% used</span></div>
		<p>Resets in 3h</p>
	</section>
	<section class="usage">
		<h3>Weekly usage</h3>
		<div><span>88% used</span></div>
		<p>Resets in 6 days</p>
	</section>
	<div>Credit balance: $12.50</div>
	</body></html>`
	c, _ := fixtureServer(t, http.StatusOK, page)
	snap, err := c.Fetch(context.Background(), config.OllamaAccount{Name: "acct", SecureSession: "cookie-value"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !snap.OK || snap.Failure != domain.FailureNone {
		t.Fatalf("not ok: %+v", snap)
	}
	if snap.Source != domain.SourceOllamaWeb || snap.Confidence != domain.ConfidenceEstimated {
		t.Errorf("source/confidence = %q/%q", snap.Source, snap.Confidence)
	}
	if snap.Credential.Key != "ollama/acct" || snap.Credential.Alias != "acct" || snap.Credential.ShortID != "****" {
		t.Errorf("credential = %+v", snap.Credential)
	}
	if snap.Plan == "" {
		t.Errorf("plan not extracted")
	}
	if snap.Balance != "$12.50" {
		t.Errorf("balance = %q", snap.Balance)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	five := windowByName(t, snap.Windows, "5h")
	if five.UsedPercent == nil || *five.UsedPercent != 42 {
		t.Errorf("5h percent = %v", five.UsedPercent)
	}
	if five.ResetAt == nil {
		t.Errorf("5h resetAt nil: %+v", five)
	}
	seven := windowByName(t, snap.Windows, "7d")
	if seven.UsedPercent == nil || *seven.UsedPercent != 88 {
		t.Errorf("7d percent = %v", seven.UsedPercent)
	}
	if seven.ResetAt == nil || seven.ResetAt.Before(snap.FetchedAt) {
		t.Errorf("7d resetAt = %v", seven.ResetAt)
	}
}

func TestFetchPlanOnlyIsParseFailure(t *testing.T) {
	// plan/balance alone must not count as a successful quota read: otherwise a
	// redesigned quota section would look permanently healthy.
	const page = `<html><body><div>Plan: Pro</div><div>Credit balance: $5</div></body></html>`
	c, _ := fixtureServer(t, http.StatusOK, page)
	snap, err := c.Fetch(context.Background(), config.OllamaAccount{Name: "acct", SecureSession: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.OK {
		t.Fatalf("plan-only page must not be OK: %+v", snap)
	}
	if snap.Failure != domain.FailureParse {
		t.Fatalf("failure = %q, want parse", snap.Failure)
	}
	if snap.Plan != "Pro" {
		t.Errorf("auxiliary plan should still be carried: %q", snap.Plan)
	}
}

func TestFetchLoginPage(t *testing.T) {
	const page = `<html><body><form><input type="password" name="password"><title>Sign in</title></form></body></html>`
	c, _ := fixtureServer(t, http.StatusOK, page)
	snap, err := c.Fetch(context.Background(), config.OllamaAccount{Name: "acct", SecureSession: "expired"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.OK || snap.Err != errSessionInvalid || snap.Failure != domain.FailureAuth {
		t.Fatalf("snap = %+v", snap)
	}
}

func TestFetchRedirectNotFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	c := New(5 * time.Second)
	c.url = srv.URL
	snap, err := c.Fetch(context.Background(), config.OllamaAccount{Name: "acct", SecureSession: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.OK || snap.Err != errSessionInvalid || snap.Failure != domain.FailureAuth {
		t.Fatalf("snap = %+v", snap)
	}
}

func TestFetchUnauthorized(t *testing.T) {
	c, _ := fixtureServer(t, http.StatusUnauthorized, "no")
	snap, err := c.Fetch(context.Background(), config.OllamaAccount{Name: "acct", SecureSession: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.OK || snap.Err != errSessionInvalid || snap.Failure != domain.FailureAuth {
		t.Fatalf("snap = %+v", snap)
	}
}

func TestFetchMissingFields(t *testing.T) {
	const page = `<html><body><div>Nothing here at all</div></body></html>`
	c, _ := fixtureServer(t, http.StatusOK, page)
	snap, err := c.Fetch(context.Background(), config.OllamaAccount{Name: "acct", SecureSession: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.OK || snap.Failure != domain.FailureParse {
		t.Fatalf("expected parse failure snapshot, got %+v", snap)
	}
	if snap.Err == "" {
		t.Fatal("expected explanatory Err")
	}
}

func TestFetchResetOnlyIsNotAWindow(t *testing.T) {
	// A reset hint with no number must not fabricate a window.
	const page = `<html><body><section><h3>Weekly usage</h3><p>Resets in 2 days</p></section></body></html>`
	c, _ := fixtureServer(t, http.StatusOK, page)
	snap, err := c.Fetch(context.Background(), config.OllamaAccount{Name: "acct", SecureSession: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.OK || snap.Failure != domain.FailureParse {
		t.Fatalf("snap = %+v", snap)
	}
}

func TestFetchPartialOnlyWeekly(t *testing.T) {
	const page = `<html><body><section><h3>Weekly usage</h3><span>12% used</span></section></body></html>`
	c, _ := fixtureServer(t, http.StatusOK, page)
	snap, err := c.Fetch(context.Background(), config.OllamaAccount{Name: "acct", SecureSession: "x"})
	if err != nil || !snap.OK {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Name != "7d" {
		t.Fatalf("windows = %+v", snap.Windows)
	}
}

func TestCookieNeverLeaks(t *testing.T) {
	const secret = "super-secret-cookie-value"
	const page = `<html><body><h3>Weekly usage</h3><span>50% used</span></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)
	c := New(5 * time.Second)
	c.url = srv.URL
	snap, err := c.Fetch(context.Background(), config.OllamaAccount{Name: "acct", SecureSession: secret})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(snap.Err, secret) {
		t.Fatal("cookie leaked into Err")
	}
	if strings.Contains(snap.Credential.ShortID, secret) {
		t.Fatal("cookie leaked into ShortID")
	}
	if strings.Contains(snap.Credential.Key, secret) {
		t.Fatal("cookie leaked into Key")
	}
}

func TestFetchContextCancelledPreservesDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	c := New(5 * time.Second)
	c.url = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Fetch(ctx, config.OllamaAccount{Name: "acct", SecureSession: "x"})
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestParseResetRelative(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	at, ok := parseReset("6 days", now)
	if !ok || !at.Equal(now.Add(6*24*time.Hour)) {
		t.Fatalf("reset = %v %v", at, ok)
	}
	if _, ok := parseReset("whenever", now); ok {
		t.Fatal("garbage must not parse")
	}
}

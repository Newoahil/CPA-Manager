package collect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/cpa"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/ollama"
)

func TestAllWithoutOllamaAccounts(t *testing.T) {
	cfg := config.Config{CPABaseURL: "http://cpa:8317", CPAManagementKey: "k", CPATimeout: time.Second}
	got := All(cfg)
	if len(got) != 1 || got[0].Name() != "cpa" {
		t.Fatalf("collectors = %+v", got)
	}
}

func TestDisabledSkippedUnavailableStillProbed(t *testing.T) {
	for _, strategy := range []string{"proxy", "normalized"} {
		t.Run(strategy, func(t *testing.T) {
			var fetched atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v8/management/credentials" {
					_, _ = w.Write([]byte(`{"files":[{"auth_index":"disabled","provider":"claude","disabled":true},{"auth_index":"unavailable","provider":"claude","unavailable":true}]}`))
					return
				}
				fetched.Add(1)
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "disabled") {
					t.Error("disabled triggered quota/OAuth request")
				}
				if strategy == "normalized" {
					_, _ = w.Write([]byte(`{"groups":[{"buckets":[{"remainingFraction":0.9}]}]}`))
				} else {
					_, _ = w.Write([]byte(`{"status_code":200,"header":{},"body":"{\"five_hour\":{\"utilization\":10}}"}`))
				}
			}))
			defer srv.Close()
			c := cpa.NewWithOptions(srv.URL, "fixture", time.Second, cpa.Options{APIVersion: "v8", QuotaStrategy: strategy})
			snaps, err := NewCPACollector(c).Collect(context.Background())
			if err != nil || len(snaps) != 1 || !snaps[0].Credential.Unavailable || snaps[0].Credential.Disabled || fetched.Load() != 1 {
				t.Fatalf("snaps=%+v calls=%d err=%v", snaps, fetched.Load(), err)
			}
		})
	}
}

func TestAllWithOllamaAccounts(t *testing.T) {
	cfg := config.Config{
		CPABaseURL:       "http://cpa:8317",
		CPAManagementKey: "k",
		CPATimeout:       time.Second,
		OllamaTimeout:    time.Second,
		OllamaAccounts:   []config.OllamaAccount{{Name: "a", SecureSession: "s"}},
	}
	got := All(cfg)
	if len(got) != 2 || got[0].Name() != "cpa" || got[1].Name() != "ollama" {
		t.Fatalf("collectors = %+v", got)
	}
}

func TestV0ProxyTransportFailureKeepsSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.RequestURI() {
		case "GET /v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[{"auth_index":"fixture-handle","provider":"claude"}]}`))
		case "POST /v0/management/api-call":
			time.Sleep(100 * time.Millisecond)
		default:
			t.Errorf("unexpected/forbidden request %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	client := cpa.NewWithOptions(srv.URL, "fixture-key", 25*time.Millisecond, cpa.Options{APIVersion: "v0"})
	snaps, err := NewCPACollector(client).Collect(context.Background())
	if err != nil || len(snaps) != 1 || snaps[0].Failure != domain.FailureTransport || snaps[0].Source != domain.SourceCPAV0 {
		t.Fatalf("snaps=%+v err=%v", snaps, err)
	}
}

func TestCPACollectorSkipsMissingAuthIndex(t *testing.T) {
	var fetched atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/credentials"):
			_, _ = w.Write([]byte(`{"files":[
				{"auth_index":"a1","name":"one","provider":"codex"},
				{"auth_index":"","name":"skip","provider":"codex"},
				{"auth_index":"a3","name":"three","provider":"claude"}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/credentials/quota/fetch"):
			fetched.Add(1)
			_, _ = w.Write([]byte(`{"groups":[{"buckets":[{"window":"5h","remainingFraction":0.9}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewCPACollector(cpa.NewWithOptions(srv.URL, "key", 5*time.Second, cpa.Options{APIVersion: "v8", QuotaStrategy: "normalized"}))
	snaps, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %+v", snaps)
	}
	if fetched.Load() != 2 {
		t.Fatalf("fetch count = %d, want 2", fetched.Load())
	}
	for _, s := range snaps {
		if !s.OK || s.Failure != domain.FailureNone || s.Source != domain.SourceCPA || s.Err != "" {
			t.Fatalf("snapshot = %+v", s)
		}
		if len(s.Windows) != 1 || s.Windows[0].Scope != domain.ScopeUnknown || s.Windows[0].ScopeID != "" {
			t.Fatalf("normalized windows must retain unknown scope: %+v", s.Windows)
		}
	}
}

func TestCPACollectorSingleFailureDoesNotFailBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/credentials"):
			_, _ = w.Write([]byte(`{"files":[
				{"auth_index":"bad","name":"bad","provider":"claude"},
				{"auth_index":"good","name":"good","provider":"claude"}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/requests/api-call"):
			if strings.Contains(readBody(r), "bad") {
				_, _ = w.Write([]byte(`{"status_code":401,"header":{},"body":""}`))
				return
			}
			_, _ = w.Write([]byte(`{"status_code":200,"header":{},"body":"{\"five_hour\":{\"utilization\":10}}"}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := NewCPACollector(cpa.NewWithOptions(srv.URL, "key", 5*time.Second, cpa.Options{APIVersion: "v8", QuotaStrategy: "proxy"}))
	snaps, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("batch must not fail: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %+v", snaps)
	}
	var okCount, failCount int
	for _, s := range snaps {
		if s.OK {
			okCount++
		} else {
			failCount++
			if s.Err == "" {
				t.Error("failed snapshot needs Err")
			}
		}
	}
	if okCount != 1 || failCount != 1 {
		t.Fatalf("ok=%d fail=%d", okCount, failCount)
	}
}

func TestCPACollectorRespectsConcurrencyLimit(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/credentials") {
			_, _ = w.Write([]byte(`{"files":[
				{"auth_index":"1","provider":"claude"},{"auth_index":"2","provider":"claude"},{"auth_index":"3","provider":"claude"},
				{"auth_index":"4","provider":"claude"},{"auth_index":"5","provider":"claude"},{"auth_index":"6","provider":"claude"},
				{"auth_index":"7","provider":"claude"},{"auth_index":"8","provider":"claude"},{"auth_index":"9","provider":"claude"}
			]}`))
			return
		}
		n := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if n <= old || maxInFlight.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = w.Write([]byte(`{"groups":[{"buckets":[{"window":"5h","remainingFraction":0.9}]}]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewCPACollector(cpa.NewWithOptions(srv.URL, "key", 5*time.Second, cpa.Options{APIVersion: "v8", QuotaStrategy: "normalized"}))
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if maxInFlight.Load() > cpaConcurrency {
		t.Fatalf("max in flight = %d, cap = %d", maxInFlight.Load(), cpaConcurrency)
	}
	if maxInFlight.Load() != 1 {
		t.Fatalf("shared management gate must serialize network attempts, max = %d", maxInFlight.Load())
	}
}

func TestCPACollectorContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/credentials") {
			_, _ = w.Write([]byte(`{"files":[{"auth_index":"1","provider":"claude"}]}`))
			return
		}
		// Outlast the request context so the client sees the deadline while the
		// handler still returns for cleanup.
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	c := NewCPACollector(cpa.NewWithOptions(srv.URL, "key", 5*time.Second, cpa.Options{APIVersion: "v8", QuotaStrategy: "normalized"}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Collect(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestCPACollectorControlPlaneWhenListFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := NewCPACollector(cpa.New(srv.URL, "bad-key", 5*time.Second))
	snaps, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrControlPlane) {
		t.Fatalf("err = %v, want ErrControlPlane", err)
	}
	if !errors.Is(err, cpa.ErrUnauthorized) {
		t.Fatalf("err = %v, want wrapped cpa.ErrUnauthorized", err)
	}
	if snaps == nil {
		t.Fatal("snaps must be non-nil so callers can distinguish empty from absent")
	}
}

func TestCPACollectorControlPlaneWhenListUnreachable(t *testing.T) {
	// A closed server yields a transport error from ListCredentials.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := NewCPACollector(cpa.New(srv.URL, "key", 200*time.Millisecond))
	_, err := c.Collect(context.Background())
	if !errors.Is(err, ErrControlPlane) {
		t.Fatalf("err = %v, want ErrControlPlane", err)
	}
}

func TestCPACollectorCredentialFailureNotControlPlane(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/credentials") {
			_, _ = w.Write([]byte(`{"files":[{"auth_index":"bad","provider":"codex"}]}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"failed to fetch quota: upstream timeout"}`))
	}))
	t.Cleanup(srv.Close)

	c := NewCPACollector(cpa.NewWithOptions(srv.URL, "key", 5*time.Second, cpa.Options{APIVersion: "v8", QuotaStrategy: "normalized"}))
	snaps, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("per-credential failure must not fail the batch: %v", err)
	}
	if len(snaps) != 1 || snaps[0].OK {
		t.Fatalf("snapshots = %+v", snaps)
	}
	// The 502 wrapper contains the word "quota" but is a transport failure, and
	// must never be attributed to the control plane or the credential.
	if snaps[0].Failure != domain.FailureTransport {
		t.Fatalf("failure = %q, want transport", snaps[0].Failure)
	}
}

// fakeOllamaFetcher counts concurrency and returns a fixed snapshot.
type fakeOllamaFetcher struct {
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	delay       time.Duration
	now         func() time.Time
}

func (f *fakeOllamaFetcher) Fetch(ctx context.Context, account config.OllamaAccount) (domain.QuotaSnapshot, error) {
	n := f.inFlight.Add(1)
	for {
		old := f.maxInFlight.Load()
		if n <= old || f.maxInFlight.CompareAndSwap(old, n) {
			break
		}
	}
	defer f.inFlight.Add(-1)
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return domain.QuotaSnapshot{}, ctx.Err()
	}
	return domain.QuotaSnapshot{
		Credential: domain.Credential{Key: "ollama/" + account.Name, Provider: domain.ProviderOllama, Alias: account.Name, ShortID: "****"},
		Windows:    []domain.QuotaWindow{{Name: "7d"}},
		Source:     domain.SourceOllamaWeb,
		Confidence: domain.ConfidenceReported,
		FetchedAt:  time.Now(),
		OK:         true,
	}, nil
}

func TestOllamaCollector(t *testing.T) {
	fake := &fakeOllamaFetcher{delay: 15 * time.Millisecond}
	accounts := []config.OllamaAccount{{Name: "one", SecureSession: "s"}, {Name: "two", SecureSession: "s"}}
	c := NewOllamaCollector(nil, accounts)
	c.client = fake
	snaps, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %+v", snaps)
	}
	for _, s := range snaps {
		if !s.OK || s.Source != domain.SourceOllamaWeb || s.Windows[0].Name != "7d" {
			t.Fatalf("snapshot = %+v", s)
		}
	}
	if fake.maxInFlight.Load() > ollamaConcurrency {
		t.Fatalf("max in flight = %d", fake.maxInFlight.Load())
	}
}

func TestOllamaCollectorName(t *testing.T) {
	if NewOllamaCollector(ollama.New(time.Second), nil).Name() != "ollama" {
		t.Fatal("wrong name")
	}
}

func TestOllamaCollectorContextCancel(t *testing.T) {
	fake := &fakeOllamaFetcher{delay: time.Second}
	c := NewOllamaCollector(nil, []config.OllamaAccount{{Name: "one", SecureSession: "s"}})
	c.client = fake
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Collect(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func readBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	buf := make([]byte, 512)
	n, _ := r.Body.Read(buf)
	return string(buf[:n])
}

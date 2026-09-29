package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestSharedNegativeCacheAllCalls(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(status) })
			start := time.Now()
			if err := c.Ping(context.Background()); err == nil {
				t.Fatal("accepted failure")
			}
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := c.Ping(context.Background()); err == nil {
						t.Error("cache lost error")
					}
					_, got, err := c.do(context.Background(), "v8", "POST", "/credentials/quota/fetch", nil)
					if err != nil || got != status {
						t.Errorf("status=%d err=%v", got, err)
					}
				}()
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("network calls=%d", calls.Load())
			}
			minDelay := 30 * time.Second
			if status == 401 || status == 403 {
				minDelay = 5 * time.Minute
			}
			if c.authRetry.at.Before(start.Add(minDelay)) || c.authRetry.at.After(time.Now().Add(15*time.Minute)) {
				t.Fatal("backoff outside bounds")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNetworkBackoffBoundAndRecovery(t *testing.T) {
	c := NewWithOptions("http://fixture.invalid", "fixture", time.Second, Options{APIVersion: "v8"})
	var calls int
	broken := true
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if broken {
			return nil, errors.New("fixture private transport detail")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"files":[]}`)), Header: http.Header{}}, nil
	})
	for attempt := 0; attempt < 10; attempt++ {
		// No flight is active: move only the retry boundary to simulate time.
		c.listRetry.at = time.Now().Add(-time.Second)
		_, _, err := c.do(context.Background(), "v8", "GET", "/credentials", nil)
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("network failure lost or leaked")
		}
		before := calls
		_, _, _ = c.do(context.Background(), "v8", "GET", "/credentials", nil)
		if calls != before || time.Until(c.listRetry.at) > 15*time.Minute || time.Until(c.listRetry.at) < 29*time.Second {
			t.Fatal("negative cache or bounded backoff failed")
		}
	}
	broken = false
	c.listRetry.at = time.Now().Add(-time.Second)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.listRetry.failures != 0 || !c.listRetry.at.IsZero() {
		t.Fatal("successful recovery did not reset backoff")
	}
}

func TestNormalizedUnknownScopeNeverExhaustsAccount(t *testing.T) {
	c := newTestClient(t, Options{APIVersion: "v8", QuotaStrategy: "normalized"}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, claudeList)
			return
		}
		writeJSON(w, `{"groups":[{"displayName":"some-model","buckets":[{"window":"account","remainingFraction":0}]}]}`)
	})
	s, err := c.FetchQuota(context.Background(), credential(t, c))
	if err != nil || !s.OK || s.Confidence != domain.ConfidenceReported || len(s.Windows) != 1 {
		t.Fatalf("unscoped normalized measurement lost: %+v %v", s, err)
	}
	w := s.Windows[0]
	if w.Scope != domain.ScopeUnknown || w.ScopeID != "" || w.UsedPercent == nil || *w.UsedPercent != 100 {
		t.Fatalf("measurement or scope incorrect: %+v", w)
	}
}

func TestGenericQuotaFailureClearsInferredScope(t *testing.T) {
	s := fail(domain.QuotaSnapshot{FailureScope: domain.ScopeAccount, FailureScopeID: "old-scope"}, domain.FailureQuota, "额度拒绝")
	if s.OK || s.Failure != domain.FailureQuota || s.FailureScope != domain.ScopeUnknown || s.FailureScopeID != "" {
		t.Fatalf("generic rejection acquired scope: %+v", s)
	}
}

func TestOuterQuotaAuthClosesSharedGate(t *testing.T) {
	for _, strategy := range []string{"proxy", "normalized"} {
		t.Run(strategy, func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, Options{APIVersion: "v8", QuotaStrategy: strategy}, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/v8/management/credentials" {
					writeJSON(w, claudeList)
					return
				}
				w.WriteHeader(401)
			})
			cred := credential(t, c)
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s, e := c.FetchQuota(context.Background(), cred)
					if e != nil || s.Failure != domain.FailureControlPlane {
						t.Errorf("failure=%s err=%v", s.Failure, e)
					}
				}()
			}
			wg.Wait()
			if err := c.Ping(context.Background()); !errors.Is(err, ErrUnauthorized) {
				t.Fatal(err)
			}
			if _, err := c.QuotaProviders(context.Background()); !errors.Is(err, ErrUnauthorized) {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatalf("calls=%d", calls.Load())
			}
		})
	}
}

func TestShortLeaderDoesNotCancelLiveWaiter(t *testing.T) {
	for _, capability := range []bool{false, true} {
		t.Run(fmt.Sprint(capability), func(t *testing.T) {
			entered := make(chan struct{})
			var calls atomic.Int32
			c := newTestClient(t, Options{APIVersion: "v8"}, func(w http.ResponseWriter, r *http.Request) {
				if capability && r.URL.Path == "/v8/management/credentials" {
					writeJSON(w, claudeList)
					return
				}
				if calls.Add(1) == 1 {
					close(entered)
				}
				time.Sleep(50 * time.Millisecond)
				if capability {
					writeJSON(w, capabilityFixture)
				} else {
					writeJSON(w, claudeList)
				}
			})
			if capability {
				credential(t, c)
			}
			probe := func(ctx context.Context) error {
				if capability {
					_, e := c.QuotaProviders(ctx)
					return e
				}
				return c.Ping(ctx)
			}
			leader, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- probe(leader) }()
			<-entered
			waiter, cancelWaiter := context.WithTimeout(context.Background(), time.Second)
			defer cancelWaiter()
			if err := probe(waiter); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls=%d", calls.Load())
			}
		})
	}
}

func TestStrictNonemptyListShape(t *testing.T) {
	for _, body := range []string{`{"files":[{}]}`, `{"files":[{"name":42}]}`, `{"files":[{"name":"x","disabled":null}]}`, `{"files":[{"name":"x","provider":false}]}`, `{"files":[{"name":"x","auth_index":null}]}`} {
		c := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, body) })
		if err := c.Ping(context.Background()); err == nil || c.APIVersion() != "" {
			t.Fatalf("accepted %s", body)
		}
	}
	for _, body := range []string{`{"files":[]}`, `{"files":[{"name":"oauth.json"}]}`} {
		if _, err := decodePage([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestActiveFirstPageAndProbeReuse(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.RawQuery == "" {
			writeJSON(w, `{"files":[]}`)
			return
		} // official bare requests do not paginate
		if r.URL.Query().Get("page_size") != "100" {
			t.Error("unbounded page")
		}
		page := r.URL.Query().Get("page")
		switch page {
		case "1":
			writeJSON(w, `{"files":[{"name":"a","auth_index":"a","provider":"claude"}],"page":1,"page_size":100,"total":2}`)
		case "2":
			writeJSON(w, `{"files":[{"name":"b","auth_index":"b","provider":"claude"}],"page":2,"page_size":100,"total":2}`)
		default:
			t.Error("unexpected page")
			w.WriteHeader(500)
		}
	})
	creds, err := c.ListCredentials(context.Background())
	if err != nil || len(creds) != 2 || calls.Load() != 2 {
		t.Fatalf("count=%d calls=%d err=%v", len(creds), calls.Load(), err)
	}
}

func TestEveryDeclaredNormalizedBucketRequired(t *testing.T) {
	for _, bad := range []string{`{}`, `null`, `{"remainingFraction":null}`, `{"remainingFraction":"0.5"}`, `{"remainingFraction":-0.1}`, `{"remainingFraction":1.1}`} {
		for _, buckets := range []string{bad + `,{"remainingFraction":0.5}`, `{"remainingFraction":0.5},` + bad} {
			windows, plan, balance, _, _ := parseContract([]byte(`{"subscription":{"plan":"test"},"groups":[{"buckets":[` + buckets + `]}]}`))
			if len(windows) != 0 || plan != "" || balance != "" {
				t.Fatalf("partial success: %s", buckets)
			}
		}
	}
}

func TestLegacyOAuthAliasAndContextOverride(t *testing.T) {
	for _, provider := range []string{"gemini", "antigravity"} {
		for _, historical := range []bool{false, true} {
			canonical := provider
			if provider == "gemini" {
				canonical = "gemini-cli"
			}
			key := opaqueKey(canonical, "fixture-handle")
			if historical {
				key = opaqueKey(provider, "fixture-handle")
			}
			var proxies atomic.Int32
			c := newTestClient(t, Options{APIVersion: "v0", ContextOverrides: map[string]ContextOverride{key: {ProjectID: "verified-project"}}}, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					writeJSON(w, `{"files":[{"auth_index":"fixture-handle","provider":"`+provider+`","account_type":"oauth","account":"fixture@example.invalid"}]}`)
					return
				}
				proxies.Add(1)
				var payload struct {
					Data string `json:"data"`
				}
				if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Data != `{"project":"verified-project"}` {
					t.Error("override not bound")
				}
				writeJSON(w, `{"status_code":401,"header":{},"body":""}`)
			})
			cred := credential(t, c)
			if cred.Key != opaqueKey(canonical, "fixture-handle") || string(cred.Provider) != canonical {
				t.Fatal("noncanonical identity")
			}
			s, e := c.FetchQuota(context.Background(), cred)
			if e != nil || s.Failure != domain.FailureAuth || proxies.Load() != 1 {
				t.Fatalf("failure=%s err=%v", s.Failure, e)
			}
			raw, _ := json.Marshal(s)
			if strings.Contains(string(raw), "verified-project") || strings.Contains(string(raw), "fixture-handle") {
				t.Fatal("context leaked")
			}
		}
	}
	for _, kind := range []string{"api_key", ""} {
		c := newTestClient(t, Options{APIVersion: "v0"}, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Error("API key misclassified")
			}
			writeJSON(w, `{"files":[{"auth_index":"x","provider":"gemini","account_type":"`+kind+`"}]}`)
		})
		cred := credential(t, c)
		if cred.Provider == domain.ProviderGeminiCLI {
			t.Fatal("API key aliased")
		}
		s, e := c.FetchQuota(context.Background(), cred)
		if e != nil || s.Failure != domain.FailureUnsupported {
			t.Fatal("API key probed")
		}
	}
}

func TestContextOverrideRejectsMissingConflictingAndAmbiguous(t *testing.T) {
	for _, metadata := range []string{`,"project_id":"different"`, `,"project_id":false`} {
		c := newTestClient(t, Options{APIVersion: "v0", ContextOverrides: map[string]ContextOverride{opaqueKey("antigravity", "x"): {ProjectID: "verified"}}}, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, `{"files":[{"auth_index":"x","provider":"antigravity"`+metadata+`}]}`)
		})
		if err := c.Ping(context.Background()); err == nil || strings.Contains(err.Error(), "verified") {
			t.Fatal("conflict accepted or leaked")
		}
	}
	c := NewWithOptions("", "", time.Second, Options{ContextOverrides: map[string]ContextOverride{opaqueKey("antigravity", "x"): {ProjectID: "verified"}}})
	for _, aliases := range []map[string]map[string]bool{{}, {opaqueKey("antigravity", "x"): {"a": true, "b": true}}} {
		if err := c.applyOverrides(nil, aliases); err == nil {
			t.Fatal("missing/ambiguous accepted")
		}
	}
}

func TestContextOverrideJSONStrictAndRedacted(t *testing.T) {
	key := "antigravity/0123456789abcdef"
	for _, raw := range []string{`null`, `[]`, `{"name":{"project_id":"secret"}}`, `{"` + key + `":{"token":"secret"}}`, `{"` + key + `":{"project_id":null}}`, `{"` + key + `":{"project_id":42}}`, `{"` + key + `":{"project_id":"secret","project_id":"other"}}`, `{"` + key + `":{"project_id":"secret"},"` + key + `":{"project_id":"other"}}`, `{"` + key + `":{"project_id":"$TOKEN$"}}`, `{"` + key + `":{"project_id":"line\nbreak"}}`} {
		if _, err := ParseContextOverrides(raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("bad validation: %v", err)
		}
	}
	if _, err := ParseContextOverrides(`{"` + key + `":{"project_id":"verified"}}`); err != nil {
		t.Fatal(err)
	}
}

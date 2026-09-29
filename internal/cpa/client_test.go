package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// All responses in this file are synthetic contract fixtures, never captures.
const normalizedFixture = `{"groups":[{"displayName":"Daily","buckets":[{"window":"daily","remainingFraction":0.25}]}]}`
const claudeList = `{"files":[{"auth_index":"fixture-handle","provider":"claude","name":"fixture@example.invalid"}]}`
const capabilityFixture = `{"providers":[{"plugin_id":"fixture-plugin","provider":"fixture-quota","display_name":"Fixture","supported_providers":["claude"],"supports_reset":false}]}`

func newTestClient(t *testing.T, opts Options, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-management-key" || r.Header.Get("X-Management-Key") != "fixture-management-key" {
			t.Error("management authentication missing or confused with upstream token")
		}
		if r.Header.Get("Chatgpt-Account-Id") != "" || r.Header.Get("anthropic-beta") != "" {
			t.Error("upstream header on outer request")
		}
		if r.Method != "GET" && r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing JSON content type")
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return NewWithOptions(srv.URL+"/", "fixture-management-key", time.Second, opts)
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func unexpected(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	t.Errorf("unexpected/forbidden request %s %s", r.Method, r.URL.RequestURI())
	w.WriteHeader(500)
}

func credential(t *testing.T, c *Client) domain.Credential {
	t.Helper()
	creds, err := c.ListCredentials(context.Background())
	if err != nil || len(creds) != 1 {
		t.Fatalf("list failed: count=%d err=%v", len(creds), err)
	}
	return creds[0]
}

func TestVersionNegotiation(t *testing.T) {
	cases := []struct {
		name, mode, body  string
		status            int
		fallback, success bool
	}{
		{"v8", "auto", `{"files":[]}`, 200, false, true},
		{"only404", "auto", ``, 404, true, true},
		{"unauthorized", "auto", ``, 401, false, false},
		{"forbidden", "auto", ``, 403, false, false},
		{"rateLimited", "auto", ``, 429, false, false},
		{"gateway", "auto", ``, 502, false, false},
		{"server", "auto", ``, 500, false, false},
		{"unavailable", "auto", ``, 503, false, false},
		{"notImplemented", "auto", ``, 501, false, false},
		{"gatewayTimeout", "auto", ``, 504, false, false},
		{"html", "auto", `<html>fixture-secret</html>`, 200, false, false},
		{"missing", "auto", `{}`, 200, false, false},
		{"null", "auto", `{"files":null}`, 200, false, false},
		{"wrongType", "auto", `{"files":{}}`, 200, false, false},
		{"nullEntry", "auto", `{"files":[null]}`, 200, false, false},
		{"explicitV8", "v8", ``, 404, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			c := newTestClient(t, Options{APIVersion: tc.mode}, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.RequestURI())
				if r.Method != "GET" {
					t.Error("not GET")
				}
				switch r.URL.RequestURI() {
				case "/v8/management/credentials?page=1&page_size=100":
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				case "/v0/management/auth-files":
					if !tc.fallback {
						t.Error("unexpected downgrade")
					}
					writeJSON(w, `{"files":[]}`)
				default:
					unexpected(t, w, r)
				}
			})
			err := c.Ping(context.Background())
			if (err == nil) != tc.success {
				t.Fatalf("err=%v", err)
			}
			if err != nil && strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("body leaked")
			}
			want := 1
			if tc.fallback {
				want = 2
			}
			if len(paths) != want {
				t.Fatalf("paths=%v", paths)
			}
			if tc.status == 401 || tc.status == 403 {
				if !errors.Is(err, ErrUnauthorized) {
					t.Fatal("lost unauthorized sentinel")
				}
			}
		})
	}
}

func TestBoth404AndExplicitV0(t *testing.T) {
	for _, mode := range []string{"auto", "v0"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, Options{APIVersion: mode}, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "v0" && r.URL.Path != "/v0/management/auth-files" {
					t.Error("explicit mode crossed versions")
				}
				w.WriteHeader(404)
			})
			err := c.Ping(context.Background())
			if !errors.Is(err, ErrUnsupported) || c.APIVersion() != "" {
				t.Fatalf("err=%v version=%s", err, c.APIVersion())
			}
			want := int32(2)
			if mode == "v0" {
				want = 1
			}
			if calls.Load() != want {
				t.Fatal("wrong probe count")
			}
		})
	}
}

func TestNegotiationTimeoutNoDowngrade(t *testing.T) {
	c := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v8/management/credentials" {
			t.Error("downgrade")
		}
		select {
		case <-r.Context().Done():
		case <-time.After(150 * time.Millisecond):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := c.Ping(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func TestConcurrentNegotiationAndCancelableWaiter(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v8/management/credentials" {
			unexpected(t, w, r)
			return
		}
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		writeJSON(w, claudeList)
	})
	first := make(chan error, 1)
	go func() { first <- c.Ping(context.Background()) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.ListCredentials(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter err=%v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Ping(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	// Keep the server blocked while all workers enter the shared flight.
	time.Sleep(40 * time.Millisecond)
	close(release)
	wg.Wait()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("negotiated %d times", calls.Load())
	}
}

func TestPaginationAndAtomicContextReplacement(t *testing.T) {
	var generation atomic.Int32
	c := newTestClient(t, Options{APIVersion: "v8"}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			unexpected(t, w, r)
			return
		}
		switch r.URL.RequestURI() {
		case "/v8/management/credentials?page=1&page_size=100":
			if generation.Load() == 1 {
				writeJSON(w, `{"files":[]}`)
				return
			}
			writeJSON(w, `{"files":[{"auth_index":"a","provider":"claude"}],"page":1,"page_size":1,"total":2,"has_more":true}`)
		case "/v8/management/credentials?page=2&page_size=1":
			writeJSON(w, `{"files":[{"auth_index":"b","provider":"claude"}],"page":2,"page_size":1,"total":2,"has_more":false}`)
		default:
			unexpected(t, w, r)
		}
	})
	got, err := c.ListCredentials(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("list: %v %v", got, err)
	}
	generation.Store(1)
	if _, err := c.ListCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.contexts) != 0 {
		t.Fatal("removed context retained")
	}
}

func TestPaginationTerminationGuards(t *testing.T) {
	for _, body := range []string{
		`{"files":[],"has_more":true}`,
		`{"files":[{"auth_index":"a"}],"page":2,"has_more":true}`,
		`{"files":[{"auth_index":"a"}],"total":0}`,
		`{"files":[{"auth_index":"a"}],"total":2,"has_more":false}`,
		`{"files":[{"auth_index":"a"}],"has_more":true}`,
	} {
		t.Run(body, func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, Options{APIVersion: "v8"}, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeJSON(w, body) })
			if _, err := c.ListCredentials(context.Background()); err == nil {
				t.Fatal("bad/repeated page accepted")
			}
			if calls.Load() > 2 || len(c.contexts) != 0 {
				t.Fatal("unbounded or partially published list")
			}
		})
	}
}

func TestV0DoesNotPaginate(t *testing.T) {
	var calls int
	c := newTestClient(t, Options{APIVersion: "v0"}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.RequestURI() != "/v0/management/auth-files" {
			unexpected(t, w, r)
		}
		writeJSON(w, `{"files":[],"has_more":true,"total":50}`)
	})
	if err := c.Ping(context.Background()); err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestProxyProviderIntegration(t *testing.T) {
	cases := []struct{ provider, profile, metadata, target, method, body string }{
		{"codex", "current", `,"id_token":{"chatgpt_account_id":"fixture-account"}`, "https://chatgpt.com/backend-api/wham/usage", "GET", `{"rate_limit":{"primary_window":{"used_percent":0.95}}}`},
		{"claude", "current", ``, "https://api.anthropic.com/api/oauth/usage", "GET", `{"five_hour":{"utilization":0.95}}`},
		{"gemini-cli", "current", `,"project_id":"fixture-project"`, "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", "POST", `{"buckets":[{"modelId":"fixture","remainingFraction":0.95}]}`},
		{"antigravity", "current", `,"project_id":"fixture-project"`, "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", "POST", `{"groups":[{"displayName":"fixture","buckets":[{"remainingFraction":0.95}]}]}`},
		{"antigravity", "legacy", `,"project_id":"fixture-project"`, "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels", "POST", `{"models":{"fixture":{"quotaInfo":{"remainingFraction":0.95}}}}`},
	}
	for _, version := range []string{"v0", "v8"} {
		for _, tc := range cases {
			t.Run(version+"/"+tc.provider+"/"+tc.profile, func(t *testing.T) {
				listPath, proxyPath := "/v0/management/auth-files", "/v0/management/api-call"
				if version == "v8" {
					listPath, proxyPath = "/v8/management/credentials?page=1&page_size=100", "/v8/management/requests/api-call"
				}
				var calls int
				c := newTestClient(t, Options{APIVersion: version, QuotaStrategy: "proxy", AntigravityProfile: tc.profile}, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.RequestURI() {
					case listPath:
						if r.Method != "GET" {
							t.Error("list method")
						}
						writeJSON(w, `{"files":[{"auth_index":"fixture-handle","provider":"`+tc.provider+`"`+tc.metadata+`}]}`)
					case proxyPath:
						calls++
						if r.Method != "POST" {
							t.Error("proxy method")
						}
						var payload struct {
							AuthIndex string            `json:"auth_index"`
							Method    string            `json:"method"`
							URL       string            `json:"url"`
							Header    map[string]string `json:"header"`
							Data      string            `json:"data"`
						}
						dec := json.NewDecoder(r.Body)
						dec.DisallowUnknownFields()
						if err := dec.Decode(&payload); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						if payload.AuthIndex != "fixture-handle" || payload.Method != tc.method || payload.URL != tc.target {
							t.Error("wrong proxy request")
						}
						if payload.Header["Authorization"] != "Bearer $TOKEN$" || payload.Header["X-Management-Key"] != "" {
							t.Error("upstream auth contamination")
						}
						if payload.Header["Content-Type"] != "application/json" {
							t.Error("upstream content type")
						}
						for k, v := range payload.Header {
							if strings.Contains(v, "fixture-management-key") || (k != "Authorization" && strings.Contains(v, "$TOKEN$")) {
								t.Error("unsafe header")
							}
						}
						if strings.Contains(payload.Data+payload.URL, "$TOKEN$") {
							t.Error("placeholder outside header")
						}
						if tc.provider == "codex" && (payload.Header["Chatgpt-Account-Id"] != "fixture-account" || payload.Header["User-Agent"] == "") {
							t.Error("missing codex context")
						}
						if tc.provider == "claude" && payload.Header["anthropic-beta"] != "oauth-2025-04-20" {
							t.Error("missing Claude beta")
						}
						if tc.provider == "antigravity" && payload.Header["User-Agent"] == "" {
							t.Error("missing Antigravity UA")
						}
						if tc.method == "POST" {
							if payload.Data != `{"project":"fixture-project"}` {
								t.Errorf("wrong data=%s", payload.Data)
							}
						} else if payload.Data != "" {
							t.Error("GET body")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "header": map[string][]string{}, "body": tc.body})
					default:
						unexpected(t, w, r)
					}
				})
				snap, err := c.FetchQuota(context.Background(), credential(t, c))
				if err != nil || !snap.OK || len(snap.Windows) != 1 || snap.Confidence != domain.ConfidenceReported || calls != 1 {
					t.Fatalf("fetch failed: %+v %v calls=%d", snap, err, calls)
				}
				want := domain.SourceCPA
				if version == "v0" {
					want = domain.SourceCPAV0
				}
				if snap.Source != want {
					t.Fatal("wrong source")
				}
				encoded, _ := json.Marshal(snap)
				for _, secret := range []string{"fixture-handle", "fixture-account", "fixture-project", "fixture-management-key"} {
					if strings.Contains(string(encoded), secret) {
						t.Fatal("internal context leaked")
					}
				}
			})
		}
	}
}

func TestCapabilitySelection(t *testing.T) {
	cases := []struct {
		name, capabilities, metadata, strategy string
		status, fetchStatus                    int
		wantPath                               string
		failure                                domain.FailureKind
	}{
		{"plugin", capabilityFixture, "", "auto", 200, 200, "/credentials/quota/fetch", ""},
		{"supports", `{"providers":[]}`, `,"supports_quota":true`, "auto", 200, 200, "/credentials/quota/fetch", ""},
		{"probe", `{"providers":[]}`, `,"quota_probe":{"url":"https://fixture.invalid"}`, "auto", 200, 200, "/credentials/quota/fetch", ""},
		{"selectedPlugin", capabilityFixture, `,"quota_provider":"fixture-plugin"`, "auto", 200, 200, "/credentials/quota/fetch", ""},
		{"credentialProvider", `{"providers":[]}`, `,"quota_provider":"existing-probe"`, "auto", 200, 200, "/credentials/quota/fetch", ""},
		{"absent", `{"providers":[]}`, `,"supports_quota":false`, "auto", 200, 200, "/requests/api-call", ""},
		{"malformed", `{"providers":["claude"]}`, "", "auto", 200, 200, "", domain.FailureParse},
		{"missing", `{}`, "", "auto", 200, 200, "", domain.FailureParse},
		{"null", `{"providers":null}`, "", "auto", 200, 200, "", domain.FailureParse},
		{"partialObject", `{"providers":[{"provider":"claude"}]}`, "", "auto", 200, 200, "", domain.FailureParse},
		{"invalidMetadata", `{"providers":[]}`, `,"supports_quota":"yes"`, "auto", 200, 200, "", domain.FailureParse},
		{"unavailable", ``, "", "auto", 404, 200, "", domain.FailureUnsupported},
		{"capAuth", ``, "", "auto", 401, 200, "", domain.FailureControlPlane},
		{"capGateway", ``, "", "auto", 502, 200, "", domain.FailureTransport},
		{"capRateLimit", ``, "", "auto", 429, 200, "", domain.FailureTransport},
		{"501NoFallback", capabilityFixture, "", "auto", 200, 501, "/credentials/quota/fetch", domain.FailureUnsupported},
		{"502NoFallback", capabilityFixture, "", "auto", 200, 502, "/credentials/quota/fetch", domain.FailureTransport},
		{"explicitNormalized", ``, "", "normalized", 404, 200, "/credentials/quota/fetch", ""},
		{"explicitProxy", ``, "", "proxy", 404, 200, "/requests/api-call", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var capCalls, fetchCalls int
			c := newTestClient(t, Options{APIVersion: "v8", QuotaStrategy: tc.strategy}, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.RequestURI() {
				case "/v8/management/credentials?page=1&page_size=100":
					writeJSON(w, `{"files":[{"auth_index":"fixture-handle","provider":"claude"`+tc.metadata+`}]}`)
				case "/v8/management/credentials/quota/providers":
					capCalls++
					if r.Method != "GET" || tc.strategy != "auto" {
						t.Error("unexpected capabilities request")
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.capabilities)
				default:
					fetchCalls++
					if tc.wantPath == "" || r.URL.Path != "/v8/management"+tc.wantPath || r.Method != "POST" {
						unexpected(t, w, r)
						return
					}
					if tc.wantPath == "/credentials/quota/fetch" {
						var req map[string]string
						if json.NewDecoder(r.Body).Decode(&req) != nil || !reflect.DeepEqual(req, map[string]string{"auth_index": "fixture-handle"}) {
							t.Error("wrong normalized body")
						}
						w.WriteHeader(tc.fetchStatus)
						_, _ = io.WriteString(w, normalizedFixture)
					} else {
						writeJSON(w, `{"status_code":200,"header":{},"body":"{\"five_hour\":{\"utilization\":5}}"}`)
					}
				}
			})
			snap, err := c.FetchQuota(context.Background(), credential(t, c))
			if err != nil || snap.Failure != tc.failure || snap.OK != (tc.failure == "") {
				t.Fatalf("snap=%+v err=%v", snap, err)
			}
			wantCaps := 0
			if tc.strategy == "auto" {
				wantCaps = 1
			}
			wantFetch := 0
			if tc.wantPath != "" {
				wantFetch = 1
			}
			if capCalls != wantCaps || fetchCalls != wantFetch {
				t.Fatalf("calls cap=%d fetch=%d", capCalls, fetchCalls)
			}
		})
	}
}

func TestProxyErrorLayersAndNoBodyLeaks(t *testing.T) {
	for _, version := range []string{"v0", "v8"} {
		for _, tc := range []struct {
			outer, upstream int
			want            domain.FailureKind
		}{
			{401, 200, domain.FailureControlPlane}, {403, 200, domain.FailureControlPlane}, {502, 200, domain.FailureTransport},
			{200, 401, domain.FailureAuth}, {200, 403, domain.FailureTransport}, {200, 429, domain.FailureTransport}, {200, 500, domain.FailureTransport},
		} {
			t.Run(fmt.Sprintf("%s/%d/%d", version, tc.outer, tc.upstream), func(t *testing.T) {
				list, proxy := "/v0/management/auth-files", "/v0/management/api-call"
				if version == "v8" {
					list, proxy = "/v8/management/credentials", "/v8/management/requests/api-call"
				}
				c := newTestClient(t, Options{APIVersion: version, QuotaStrategy: "proxy"}, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == list && r.Method == "GET" {
						writeJSON(w, claudeList)
						return
					}
					if r.URL.Path != proxy || r.Method != "POST" {
						unexpected(t, w, r)
						return
					}
					w.WriteHeader(tc.outer)
					_ = json.NewEncoder(w).Encode(map[string]any{"status_code": tc.upstream, "header": map[string][]string{}, "body": "fixture-secret token expired quota_exceeded fixture-management-key"})
				})
				snap, err := c.FetchQuota(context.Background(), credential(t, c))
				if err != nil || snap.OK || snap.Failure != tc.want || snap.Confidence != domain.ConfidenceUnknown || len(snap.Windows) != 0 {
					t.Fatalf("snap=%+v err=%v", snap, err)
				}
				if strings.Contains(snap.Err, "fixture-") {
					t.Fatal("raw response leaked")
				}
			})
		}
	}
}

func TestMalformedProxyEnvelope(t *testing.T) {
	for _, body := range []string{`{}`, `{"status_code":200,"header":{},"body":{}}`, `{"status_code":null,"header":{},"body":"{}"}`, `{"status_code":200,"header":{"x":"secret"},"body":"{}"}`, `{"status_code":200,"body":"{}"}`, `{"status_code":0,"header":{},"body":"{}"}`} {
		t.Run(body, func(t *testing.T) {
			c := newTestClient(t, Options{APIVersion: "v0"}, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v0/management/auth-files" {
					writeJSON(w, claudeList)
					return
				}
				if r.URL.Path != "/v0/management/api-call" {
					unexpected(t, w, r)
					return
				}
				writeJSON(w, body)
			})
			snap, err := c.FetchQuota(context.Background(), credential(t, c))
			if err != nil || snap.Failure != domain.FailureParse {
				t.Fatalf("snap=%+v err=%v", snap, err)
			}
		})
	}
}

func TestMissingConflictingAndVerifiedContexts(t *testing.T) {
	cases := []struct {
		provider, metadata string
		valid              bool
	}{
		{"codex", ``, false},
		{"codex", `,"id_token":"encoded-jwt-not-claims"`, false},
		{"codex", `,"id_token":{"chatgpt_account_id":false}`, false},
		{"codex", `,"id_token":{"chatgpt_account_id":"one"},"chatgpt_account_id":"two"`, false},
		{"codex", `,"chatgpt_account_id":"fixture-account"`, true},
		{"codex", `,"id_token":{"chatgpt_account_id":"fixture-account"},"chatgpt_account_id":"fixture-account"`, true},
		{"gemini-cli", ``, false},
		{"gemini-cli", `,"account":"project-from-name"`, false},
		{"gemini-cli", `,"account":"fixture@example.invalid (fixture-project)"`, true},
		{"gemini-cli", `,"project_id":"fixture-project","account":"fixture@example.invalid (different)"`, false},
		{"gemini-cli", `,"project_id":false`, false},
		{"gemini-cli", `,"project_id":"$TOKEN$"`, false},
		{"gemini-cli", `,"project_id":"fixture-project"`, true},
		{"antigravity", `,"account":"fixture@example.invalid (fixture-project)"`, false},
		{"antigravity", `,"project_id":"fixture-project"`, true},
		{"antigravity", `,"project_id":"line\nbreak"`, false},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			var calls int
			c := newTestClient(t, Options{APIVersion: "v0"}, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v0/management/auth-files" && r.Method == "GET" {
					writeJSON(w, `{"files":[{"auth_index":"fixture-handle","name":"not-an-id","provider":"`+tc.provider+`"`+tc.metadata+`}]}`)
					return
				}
				if r.URL.Path != "/v0/management/api-call" || r.Method != "POST" {
					unexpected(t, w, r)
					return
				}
				calls++
				writeJSON(w, `{"status_code":401,"header":{},"body":"fixture-secret"}`)
			})
			snap, err := c.FetchQuota(context.Background(), credential(t, c))
			want := domain.FailureParse
			if tc.valid {
				want = domain.FailureAuth
			}
			if err != nil || snap.Failure != want || (calls == 1) != tc.valid {
				t.Fatalf("snap=%+v err=%v calls=%d", snap, err, calls)
			}
		})
	}
}

func TestNormalizedOnV0AndUnknownAdapter(t *testing.T) {
	for _, strategy := range []string{"normalized", "proxy", "auto"} {
		t.Run(strategy, func(t *testing.T) {
			c := newTestClient(t, Options{APIVersion: "v0", QuotaStrategy: strategy}, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v0/management/auth-files" || r.Method != "GET" {
					unexpected(t, w, r)
					return
				}
				writeJSON(w, `{"files":[{"auth_index":"fixture-handle","provider":"unsupported"}]}`)
			})
			snap, err := c.FetchQuota(context.Background(), credential(t, c))
			if err != nil || snap.Failure != domain.FailureUnsupported {
				t.Fatalf("snap=%+v err=%v", snap, err)
			}
		})
	}
}

func TestFetchDeadlineAndSafeTransportError(t *testing.T) {
	c := newTestClient(t, Options{APIVersion: "v0"}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files" {
			writeJSON(w, claudeList)
			return
		}
		if r.URL.Path != "/v0/management/api-call" {
			unexpected(t, w, r)
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(150 * time.Millisecond):
		}
	})
	cred := credential(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.FetchQuota(ctx, cred)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), c.baseURL) {
		t.Fatalf("err=%v", err)
	}
}

func TestOpaqueIdentityAndNoContextSerialization(t *testing.T) {
	f := credentialFile{AuthIndex: "fixture-live-handle", Provider: "Codex", Name: "fixture@example.invalid"}
	a, b := toCredential(f), toCredential(f)
	if a.Key != b.Key || strings.Contains(a.Key, f.AuthIndex) || a.Provider != domain.ProviderCodex || a.Alias != "fixture" {
		t.Fatalf("bad identity %+v", a)
	}
	f.AuthIndex += "2"
	if a.Key == toCredential(f).Key {
		t.Fatal("key collision")
	}
	raw, _ := json.Marshal(a)
	if strings.Contains(string(raw), "fixture-live-handle") || strings.Contains(string(raw), "example.invalid") {
		t.Fatal("credential serialization leaked")
	}
	if len([]rune(safeAlias(strings.Repeat("界", 40)))) != 32 {
		t.Fatal("alias truncation")
	}
}

func TestCapabilitiesSingleFlight(t *testing.T) {
	var capCalls atomic.Int32
	c := newTestClient(t, Options{APIVersion: "v8"}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			writeJSON(w, claudeList)
		case "/v8/management/credentials/quota/providers":
			capCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			writeJSON(w, capabilityFixture)
		case "/v8/management/credentials/quota/fetch":
			writeJSON(w, normalizedFixture)
		default:
			unexpected(t, w, r)
		}
	})
	cred := credential(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, e := c.FetchQuota(context.Background(), cred)
			if e != nil || !s.OK || s.Failure != domain.FailureNone || len(s.Windows) != 1 || s.Windows[0].Scope != domain.ScopeUnknown || s.Windows[0].ScopeID != "" {
				t.Errorf("fetch failure %v %v", s.Failure, e)
			}
		}()
	}
	wg.Wait()
	if capCalls.Load() != 1 {
		t.Fatalf("capabilities calls=%d", capCalls.Load())
	}
	providers, err := c.QuotaProviders(context.Background())
	if err != nil || !reflect.DeepEqual(providers, []string{"fixture-quota"}) {
		t.Fatalf("providers=%v err=%v", providers, err)
	}
}

func TestNormalizedStrictProductionPath(t *testing.T) {
	for _, body := range []string{
		`{"five_hour":{"used_percent":42}}`,
		`{"groups":[{"buckets":[{"remainingFraction":null}]}]}`,
		`{"groups":[{"buckets":[{"remainingFraction":true}]}]}`,
		`{"groups":[{"buckets":[{"remainingFraction":1.1}]}]}`,
		`{"groups":[{"buckets":[{"remainingFraction":-0.1}]}]}`,
		`{"groups":[{"buckets":[{"remainingFraction":0.5}]}],"error":"fixture-secret"}`,
	} {
		t.Run(body, func(t *testing.T) {
			c := newTestClient(t, Options{APIVersion: "v8", QuotaStrategy: "normalized"}, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v8/management/credentials" {
					writeJSON(w, claudeList)
					return
				}
				if r.URL.Path != "/v8/management/credentials/quota/fetch" {
					unexpected(t, w, r)
					return
				}
				writeJSON(w, body)
			})
			s, e := c.FetchQuota(context.Background(), credential(t, c))
			if e != nil || s.Failure != domain.FailureParse || s.OK || len(s.Windows) != 0 || s.Confidence != domain.ConfidenceUnknown || strings.Contains(s.Err, "fixture-secret") {
				t.Fatalf("snap=%+v err=%v", s, e)
			}
		})
	}
}

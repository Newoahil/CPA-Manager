package cpa

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestCredentialFailureDoesNotPoisonOtherCalls(t *testing.T) {
	for _, strategy := range []string{"normalized", "proxy"} {
		for _, cancelRequest := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%t", strategy, cancelRequest), func(t *testing.T) {
				var lists, quotas atomic.Int32
				c := newTestClient(t, Options{APIVersion: "v8", QuotaStrategy: strategy}, func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "GET" {
						lists.Add(1)
						writeJSON(w, `{"files":[{"auth_index":"first","provider":"claude"},{"auth_index":"second","provider":"claude"}]}`)
						return
					}
					if quotas.Add(1) == 1 {
						if cancelRequest {
							select {
							case <-r.Context().Done():
							case <-time.After(time.Second):
							}
						}
						w.WriteHeader(502)
						return
					}
					if strategy == "normalized" {
						writeJSON(w, normalizedFixture)
					} else {
						writeJSON(w, `{"status_code":200,"header":{},"body":"{\"five_hour\":{\"utilization\":10}}"}`)
					}
				})
				creds, err := c.ListCredentials(context.Background())
				if err != nil || len(creds) != 2 {
					t.Fatalf("list count=%d err=%v", len(creds), err)
				}
				ctx := context.Background()
				if cancelRequest {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
					defer cancel()
				}
				first, err := c.FetchQuota(ctx, creds[0])
				if cancelRequest {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("canceled request: %v", err)
					}
				} else if err != nil || first.Failure != domain.FailureTransport {
					t.Fatalf("first failure=%s err=%v", first.Failure, err)
				}
				if err := c.Ping(context.Background()); err != nil || lists.Load() != 2 {
					t.Fatalf("Ping was poisoned: calls=%d err=%v", lists.Load(), err)
				}
				second, err := c.FetchQuota(context.Background(), creds[1])
				if err != nil || !second.OK || quotas.Load() != 2 {
					t.Fatalf("second credential was poisoned: calls=%d failure=%s err=%v", quotas.Load(), second.Failure, err)
				}
			})
		}
	}
}

func TestControlBackoffIsRouteScoped(t *testing.T) {
	for _, status := range []int{429, 502} {
		for _, path := range []string{"/credentials?page=1&page_size=100", "/credentials/quota/providers"} {
			t.Run(fmt.Sprintf("%d/%s", status, path), func(t *testing.T) {
				var calls atomic.Int32
				c := newTestClient(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.RequestURI() == "/v8/management"+path {
						w.WriteHeader(status)
						return
					}
					writeJSON(w, `{}`)
				})
				for i := 0; i < 3; i++ {
					_, got, err := c.do(context.Background(), "v8", "GET", path, nil)
					if err != nil || got != status {
						t.Fatalf("status=%d err=%v", got, err)
					}
				}
				if calls.Load() != 1 {
					t.Fatal("control failure was not cached")
				}
				other := "/credentials/quota/providers"
				if path == other {
					other = "/credentials?page=1&page_size=100"
				}
				_, _, _ = c.do(context.Background(), "v8", "GET", other, nil)
				_, _, _ = c.do(context.Background(), "v8", "POST", "/requests/api-call", []byte(`{}`))
				if calls.Load() != 3 {
					t.Fatal("control backoff contaminated unrelated routes")
				}
			})
		}
	}
}

func TestHTTPTimeoutIsPerPageAndExcludesGateWait(t *testing.T) {
	var pages atomic.Int32
	c := newTestClient(t, Options{APIVersion: "v8"}, func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		time.Sleep(35 * time.Millisecond)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		writeJSON(w, fmt.Sprintf(`{"files":[{"auth_index":"%d","provider":"claude"}],"page":%d,"page_size":1,"total":2}`, page, page))
	})
	c.http.Timeout = 50 * time.Millisecond
	c.requestGate <- struct{}{}
	released := make(chan struct{})
	go func() {
		time.Sleep(70 * time.Millisecond)
		<-c.requestGate
		close(released)
	}()
	creds, err := c.ListCredentials(context.Background())
	<-released
	if err != nil || len(creds) != 2 || pages.Load() != 2 {
		t.Fatalf("per-request timeout consumed by flight/gate: pages=%d count=%d err=%v", pages.Load(), len(creds), err)
	}
	if c.FlightTimeout() != 5*time.Minute || gateWaitTimeout != 30*time.Second {
		t.Fatal("unexpected aggregate/gate bounds")
	}
}

func TestCapabilityFailureCachedForListGeneration(t *testing.T) {
	for _, status := range []int{404, 200} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			var failList atomic.Bool
			c := newTestClient(t, Options{APIVersion: "v8"}, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v8/management/credentials" {
					if failList.Load() {
						w.WriteHeader(404)
						return
					}
					body := `{"files":[`
					for i := 0; i < 20; i++ {
						if i > 0 {
							body += ","
						}
						body += fmt.Sprintf(`{"auth_index":"%d","provider":"claude"}`, i)
					}
					writeJSON(w, body+`]}`)
					return
				}
				if r.URL.Path != "/v8/management/credentials/quota/providers" {
					unexpected(t, w, r)
					return
				}
				calls.Add(1)
				w.WriteHeader(status)
				writeJSON(w, `{"providers":[{}]}`)
			})
			for round := int32(1); round <= 2; round++ {
				creds, err := c.ListCredentials(context.Background())
				if err != nil || len(creds) != 20 {
					t.Fatalf("list count=%d err=%v", len(creds), err)
				}
				// Two batches expose both concurrent-flight and completed-cache use.
				for batch := 0; batch < 2; batch++ {
					var wg sync.WaitGroup
					for _, cred := range creds[batch*10 : (batch+1)*10] {
						wg.Add(1)
						go func(cred domain.Credential) {
							defer wg.Done()
							s, err := c.FetchQuota(context.Background(), cred)
							want := domain.FailureParse
							if status == 404 {
								want = domain.FailureUnsupported
							}
							if err != nil || s.Failure != want {
								t.Errorf("capability failure=%s err=%v", s.Failure, err)
							}
						}(cred)
					}
					wg.Wait()
				}
				if calls.Load() != round {
					t.Fatalf("round=%d capability requests=%d", round, calls.Load())
				}
				failList.Store(true)
				if err := c.Ping(context.Background()); err == nil {
					t.Fatal("failed list accepted")
				}
				_, _ = c.QuotaProviders(context.Background())
				if calls.Load() != round {
					t.Fatal("failed list invalidated capability generation")
				}
				failList.Store(false)
			}
		})
	}
}

func TestCapabilityFlightExcludesGateFromHTTPBudget(t *testing.T) {
	c := newTestClient(t, Options{APIVersion: "v8"}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v8/management/credentials" {
			writeJSON(w, claudeList)
			return
		}
		time.Sleep(35 * time.Millisecond)
		writeJSON(w, capabilityFixture)
	})
	credential(t, c)
	c.http.Timeout = 50 * time.Millisecond
	c.requestGate <- struct{}{}
	released := make(chan struct{})
	go func() {
		time.Sleep(70 * time.Millisecond)
		<-c.requestGate
		close(released)
	}()
	providers, err := c.QuotaProviders(context.Background())
	<-released
	if err != nil || len(providers) != 1 {
		t.Fatalf("capability gate consumed HTTP timeout: %v", err)
	}
}

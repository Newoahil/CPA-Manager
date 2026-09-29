package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/collect"
	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestWatcherAndCollectorShareNegotiation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var v8Calls, v0Calls, proxyCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("missing management auth")
		}
		switch r.Method + " " + r.URL.RequestURI() {
		case "GET /v8/management/credentials?page=1&page_size=100":
			if v8Calls.Add(1) == 1 {
				close(entered)
			}
			<-release
			w.WriteHeader(404)
		case "GET /v0/management/auth-files":
			v0Calls.Add(1)
			_, _ = io.WriteString(w, `{"files":[{"auth_index":"fixture-handle","provider":"claude"}]}`)
		case "POST /v0/management/api-call":
			proxyCalls.Add(1)
			_, _ = io.WriteString(w, `{"status_code":200,"header":{},"body":"{\"five_hour\":{\"utilization\":5}}"}`)
		default:
			t.Errorf("unexpected/forbidden request %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	cfg := config.Config{CPABaseURL: srv.URL, CPAManagementKey: "fixture-key", CPATimeout: time.Second, CPAAPIVersion: "auto", CPAQuotaStrategy: "auto", AntigravityQuotaProfile: "current"}
	client := collect.NewCPAClient(cfg)
	collectors := collect.AllWithCPA(cfg, client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		watchCPA(ctx, client, srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	<-entered
	result := make(chan []domain.QuotaSnapshot, 1)
	go func() {
		snaps, err := collectors[0].Collect(ctx)
		if err != nil {
			t.Error(err)
		}
		result <- snaps
	}()
	time.Sleep(30 * time.Millisecond)
	close(release)
	select {
	case snaps := <-result:
		if len(snaps) != 1 || !snaps[0].OK || snaps[0].Source != domain.SourceCPAV0 {
			t.Fatalf("snapshots=%+v", snaps)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("collection stalled")
	}
	cancel()
	select {
	case <-watchDone:
	case <-time.After(time.Second):
		t.Fatal("watcher ignored cancellation")
	}
	if v8Calls.Load() != 1 || v0Calls.Load() != 1 || proxyCalls.Load() != 1 {
		t.Fatalf("calls v8=%d v0=%d proxy=%d", v8Calls.Load(), v0Calls.Load(), proxyCalls.Load())
	}
}

type watchLog chan string

func (w watchLog) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

func TestWatcherUsesWholeListBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(35 * time.Millisecond)
		page := r.URL.Query().Get("page")
		_, _ = fmt.Fprintf(w, `{"files":[{"auth_index":"%s","provider":"claude"}],"page":%s,"page_size":1,"total":2}`, page, page)
	}))
	defer srv.Close()
	client := collect.NewCPAClient(config.Config{CPABaseURL: srv.URL, CPAManagementKey: "fixture", CPATimeout: 50 * time.Millisecond, CPAAPIVersion: "v8"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	logs := make(watchLog, 2)
	go func() {
		defer close(done)
		watchCPA(ctx, client, srv.URL, slog.New(slog.NewTextHandler(logs, nil)))
	}()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case line := <-logs:
		if !strings.Contains(line, "management api reachable") {
			t.Fatalf("watcher used single HTTP budget for complete list: %s", line)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not complete paginated probe")
	}
}

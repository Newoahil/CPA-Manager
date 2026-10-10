package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

type cooldownEnv struct {
	mu    sync.Mutex
	now   time.Time
	creds []domain.Credential
	err   error
}

func (e *cooldownEnv) list(context.Context) ([]domain.Credential, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]domain.Credential(nil), e.creds...), e.err
}

func (e *cooldownEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *cooldownEnv) set(creds ...domain.Credential) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.creds = creds
}

func (e *cooldownEnv) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

var cdT0 = time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)

func cdCred(key string, cooldowns ...domain.Cooldown) domain.Credential {
	return domain.Credential{Key: key, Provider: domain.ProviderCodex, Alias: key, ShortID: "****1", Cooldowns: cooldowns}
}

func cdLong(model string) domain.Cooldown {
	at := cdT0.Add(time.Hour)
	return domain.Cooldown{Scope: "credential", ModelKey: model, RetryAt: &at, HTTPStatus: 429}
}

func cdShort(model string) domain.Cooldown {
	at := cdT0.Add(time.Minute)
	return domain.Cooldown{Scope: "credential", ModelKey: model, RetryAt: &at, HTTPStatus: 429}
}

func newCooldownApp(t *testing.T, collectors []domain.Collector, notifiers ...domain.Notifier) (*App, *cooldownEnv, *memStore) {
	t.Helper()
	store := newMemStore()
	a := newTestApp(store, collectors)
	env := &cooldownEnv{now: cdT0}
	a.now = env.clock
	a.SetNotifiers(notifiers)
	a.SetCooldownLister(env.list)
	return a, env, store
}

func TestCooldownAlertsAreBatchedIntoOneMessage(t *testing.T) {
	n := &fakeNotifier{name: "feishu"}
	a, env, store := newCooldownApp(t, nil, n)
	env.set(cdCred("a", cdLong("m1"), cdLong("m2")), cdCred("b", cdLong("m1")))

	a.cooldownTick(context.Background(), a.cooldownWatcher())

	if n.calls.Load() != 1 {
		t.Fatalf("notify calls = %d, want exactly 1 batched message", n.calls.Load())
	}
	msg := n.lastMessage()
	if len(msg.Alerts) != 3 || msg.Kind != string(domain.AlertRateLimited) {
		t.Fatalf("message = kind %q, %d alerts", msg.Kind, len(msg.Alerts))
	}
	if msg.Report != nil {
		t.Fatal("no report exists yet, so Report must be nil")
	}
	if got := store.st.PendingFor("feishu"); len(got) != 0 {
		t.Fatalf("outbox not cleared after delivery: %+v", got)
	}

	// Same episodes again: no new message.
	a.cooldownTick(context.Background(), a.cooldownWatcher())
	if n.calls.Load() != 1 {
		t.Fatalf("episodes re-alerted: calls = %d", n.calls.Load())
	}
}

func TestCooldownAlertAttachesLastReport(t *testing.T) {
	n := &fakeNotifier{name: "feishu"}
	a, env, _ := newCooldownApp(t, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{freshSnap("codex-1")}},
	}, n)
	if _, ok := a.runCycle(context.Background()); !ok {
		t.Fatal("runCycle failed")
	}
	env.set(cdCred("a", cdLong("m")))
	a.cooldownTick(context.Background(), a.cooldownWatcher())
	if n.lastMessage().Report == nil {
		t.Fatal("expected the last report to be attached")
	}
}

func TestCooldownAlertSurvivesChannelOutage(t *testing.T) {
	down := &fakeNotifier{name: "feishu", fail: true}
	a, env, store := newCooldownApp(t, nil, down)
	env.set(cdCred("a", cdLong("m")))

	a.cooldownTick(context.Background(), a.cooldownWatcher())
	queued := store.st.PendingFor("feishu")
	if len(queued) != 1 || queued[0].Kind != domain.AlertRateLimited {
		t.Fatalf("alert not queued after failed send: %+v", queued)
	}

	up := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{up})
	a.FlushPending(context.Background())
	if up.calls.Load() != 1 || len(up.lastMessage().Alerts) != 1 {
		t.Fatalf("retry did not deliver the queued alert: calls=%d", up.calls.Load())
	}
	if got := store.st.PendingFor("feishu"); len(got) != 0 {
		t.Fatalf("outbox not cleared: %+v", got)
	}
}

func TestCooldownClearedAlertAndListErrorHandling(t *testing.T) {
	n := &fakeNotifier{name: "feishu"}
	a, env, _ := newCooldownApp(t, nil, n)
	env.set(cdCred("a", cdLong("m")))
	a.cooldownTick(context.Background(), a.cooldownWatcher())
	if n.calls.Load() != 1 {
		t.Fatalf("setup calls = %d", n.calls.Load())
	}

	// A failed list is logged and ignored: no message, episode kept.
	env.mu.Lock()
	env.err = errors.New("cpa unreachable")
	env.mu.Unlock()
	env.advance(time.Minute)
	a.cooldownTick(context.Background(), a.cooldownWatcher())
	if n.calls.Load() != 1 {
		t.Fatalf("list failure produced a message: calls=%d", n.calls.Load())
	}

	env.mu.Lock()
	env.err = nil
	env.mu.Unlock()
	env.set(cdCred("a"))
	env.advance(time.Minute)
	a.cooldownTick(context.Background(), a.cooldownWatcher())
	if n.calls.Load() != 2 {
		t.Fatalf("cleared alert not sent: calls=%d", n.calls.Load())
	}
	msg := n.lastMessage()
	if len(msg.Alerts) != 1 || msg.Alerts[0].Kind != domain.AlertRateLimitCleared || msg.Kind != string(domain.AlertRateLimitCleared) {
		t.Fatalf("message = %q %+v", msg.Kind, msg.Alerts)
	}
}

func TestSendSummaryAttachesAndResetsRateLimitTallies(t *testing.T) {
	n := &fakeNotifier{name: "feishu"}
	a, env, _ := newCooldownApp(t, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{freshSnap("codex-1")}},
	}, n)

	// A short episode: seen, then gone before the alert threshold.
	env.set(cdCred("a", cdShort("m")))
	a.cooldownTick(context.Background(), a.cooldownWatcher())
	env.advance(2 * time.Minute)
	env.set(cdCred("a"))
	a.cooldownTick(context.Background(), a.cooldownWatcher())
	if n.calls.Load() != 0 {
		t.Fatalf("short episode alerted: calls=%d", n.calls.Load())
	}

	a.SendSummary(context.Background())
	var digest domain.Message
	n.mu.Lock()
	digest = n.last
	n.mu.Unlock()
	if digest.Kind != string(domain.AlertBootstrap) || digest.Report == nil {
		t.Fatalf("digest = %+v", digest)
	}
	rl := digest.Report.RateLimits
	if len(rl) != 1 || rl[0].Credential.Key != "a" || rl[0].Count != 1 || rl[0].Longest != 2*time.Minute {
		t.Fatalf("rate limits = %+v", rl)
	}
	if last, ok := a.LastReport(); !ok || len(last.RateLimits) != 0 {
		t.Fatal("tallies must not leak into the cached report")
	}

	a.SendSummary(context.Background())
	n.mu.Lock()
	second := n.last
	n.mu.Unlock()
	if second.Report == nil || len(second.Report.RateLimits) != 0 {
		t.Fatalf("tallies not reset after the digest: %+v", second.Report)
	}
}

// recNotifier remembers every alert kind it was sent.
type recNotifier struct {
	mu    sync.Mutex
	kinds []domain.AlertKind
}

func (r *recNotifier) Name() string { return "feishu" }

func (r *recNotifier) Notify(_ context.Context, msg domain.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range msg.Alerts {
		r.kinds = append(r.kinds, a.Kind)
	}
	return nil
}

func (r *recNotifier) saw(k domain.AlertKind) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.kinds {
		if got == k {
			return true
		}
	}
	return false
}

func TestRunPollRunsCooldownLoop(t *testing.T) {
	n := &recNotifier{}
	a, env, _ := newCooldownApp(t, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{freshSnap("codex-1")}},
	}, n)
	a.cfg.CooldownPollInterval = 15 * time.Millisecond
	env.set(cdCred("a", cdLong("m")))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.RunPoll(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		if n.saw(domain.AlertRateLimited) {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("cooldown loop never delivered its alert")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunPoll did not return after cancel")
	}
}

func TestCooldownDisabledWithoutIntervalOrLister(t *testing.T) {
	n := &fakeNotifier{name: "feishu"}
	a, env, _ := newCooldownApp(t, nil, n)
	env.set(cdCred("a", cdLong("m")))
	// Interval 0 (the test default): runCooldown must return immediately.
	done := make(chan struct{})
	go func() { a.runCooldown(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runCooldown should return at once when disabled")
	}
	if n.calls.Load() != 0 {
		t.Fatal("disabled watcher polled")
	}

	b := newTestApp(newMemStore(), nil)
	b.cfg.CooldownPollInterval = time.Second
	done = make(chan struct{})
	go func() { b.runCooldown(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runCooldown should return at once without a lister")
	}
}

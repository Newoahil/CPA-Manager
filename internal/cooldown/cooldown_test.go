package cooldown

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

var t0 = time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)

type fixture struct {
	mu    sync.Mutex
	now   time.Time
	creds []domain.Credential
	err   error
	calls int
	w     *Watcher
}

func newFixture(alertAfter time.Duration) *fixture {
	f := &fixture{now: t0}
	f.w = New(func(context.Context) ([]domain.Credential, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		return append([]domain.Credential(nil), f.creds...), f.err
	}, Options{AlertAfter: alertAfter, Now: func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}})
	return f
}

func (f *fixture) set(creds ...domain.Credential) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creds = creds
}

func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fixture) poll(t *testing.T) []domain.Alert {
	t.Helper()
	alerts, err := f.w.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return alerts
}

func at(d time.Duration) *time.Time {
	v := t0.Add(d)
	return &v
}

func cred(key string, cooldowns ...domain.Cooldown) domain.Credential {
	return domain.Credential{Key: key, Provider: domain.ProviderCodex, Alias: key, ShortID: "****1234", Cooldowns: cooldowns}
}

func modelCooldown(model string, retry *time.Time) domain.Cooldown {
	return domain.Cooldown{Scope: "model", ModelKey: model, Reason: "quota", RetryAt: retry, HTTPStatus: 429}
}

func hasFact(a domain.Alert, prefix string) (string, bool) {
	for _, f := range a.Facts {
		if strings.HasPrefix(f, prefix) {
			return f, true
		}
	}
	return "", false
}

func TestRoutineCooldownNeverAlertsHoweverLong(t *testing.T) {
	// CPA keeps re-cooling within its own backoff: the retry time stays near,
	// so however long the episode lasts it is routine and never pushed.
	f := newFixture(30 * time.Minute)
	for i := 0; i < 12; i++ {
		f.set(cred("a", modelCooldown("gpt-x", at(time.Duration(i)*10*time.Minute+5*time.Minute))))
		if got := f.poll(t); len(got) != 0 {
			t.Fatalf("poll %d: routine cooldown alerted: %+v", i, got)
		}
		f.advance(10 * time.Minute)
	}
	f.set(cred("a"))
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("routine recovery announced: %+v", got)
	}
	if tallies := f.w.TakeTallies(); len(tallies) != 1 || tallies[0].Count != 1 {
		t.Fatalf("routine episode must be tallied for the digest: %+v", tallies)
	}
}

func TestCooldownWithoutRetryTimeNeverAlerts(t *testing.T) {
	f := newFixture(time.Minute)
	f.set(cred("a", domain.Cooldown{Scope: "model", ModelKey: "m"}))
	f.poll(t)
	f.advance(2 * time.Hour)
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("cooldown without retry time alerted: %+v", got)
	}
}

func TestAlertsWhenRetryBeyondBackoff(t *testing.T) {
	f := newFixture(30 * time.Minute)
	f.set(cred("a", modelCooldown("gpt-x", at(30*time.Minute))))
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("retry exactly at the backoff cap alerted: %+v", got)
	}
	f.advance(5 * time.Minute)
	f.set(cred("a", modelCooldown("gpt-x", at(5*time.Minute+31*time.Minute))))
	got := f.poll(t)
	if len(got) != 1 {
		t.Fatalf("want 1 alert once the retry lies beyond the cap, got %+v", got)
	}
	a := got[0]
	if strings.Contains(a.Title, "****") || !strings.HasPrefix(a.Title, "a ") {
		t.Fatalf("title must name the alias without the masked id: %q", a.Title)
	}
	if a.Kind != domain.AlertRateLimited || a.Severity != domain.SeverityWarn || a.Evidence != domain.EvidenceConfirmed {
		t.Fatalf("alert = %+v", a)
	}
	if a.Scope != domain.ScopeModel || a.ScopeID != "gpt-x" || a.Credential.Key != "a" || a.Title == "" {
		t.Fatalf("scope/credential/title wrong: %+v", a)
	}
	if v, ok := hasFact(a, "状态码:"); !ok || v != "状态码: 429" {
		t.Fatalf("status fact = %q", v)
	}
	if v, ok := hasFact(a, "预计恢复:"); !ok || v != "预计恢复: "+t0.Add(36*time.Minute).Format(time.RFC3339) {
		t.Fatalf("recovery fact = %q", v)
	}
	if v, ok := hasFact(a, "持续:"); !ok || v != "持续: 5m" {
		t.Fatalf("duration fact = %q", v)
	}
	if len(a.Credential.Cooldowns) != 0 || a.Credential.NextRetryAfter != nil {
		t.Fatal("alert credential must not carry the cooldown payload")
	}

	// One alert per episode.
	f.advance(10 * time.Minute)
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("episode re-alerted: %+v", got)
	}
}

func TestImmediateAlertWhenRetryIsFarAway(t *testing.T) {
	f := newFixture(5 * time.Minute)
	f.set(cred("a", modelCooldown("gpt-x", at(30*time.Minute))))
	got := f.poll(t)
	if len(got) != 1 || got[0].Kind != domain.AlertRateLimited {
		t.Fatalf("want immediate alert, got %+v", got)
	}
	if v, _ := hasFact(got[0], "持续:"); v != "持续: 0m" {
		t.Fatalf("duration fact = %q", v)
	}
}

func TestRetryTimeBecomingKnownLaterTriggersAlert(t *testing.T) {
	f := newFixture(5 * time.Minute)
	f.set(cred("a", modelCooldown("m", nil)))
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("unexpected %+v", got)
	}
	f.advance(time.Minute)
	f.set(cred("a", modelCooldown("m", at(20*time.Minute))))
	if got := f.poll(t); len(got) != 1 {
		t.Fatalf("a retry time >= threshold after firstSeen must alert, got %+v", got)
	}
}

func TestOmittedFactsWhenUnknown(t *testing.T) {
	f := newFixture(time.Minute)
	f.set(cred("a", domain.Cooldown{Scope: "model", ModelKey: "m", RetryAt: at(time.Hour)}))
	got := f.poll(t)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if _, ok := hasFact(got[0], "状态码:"); ok {
		t.Fatal("status fact emitted without a known status")
	}
	if _, ok := hasFact(got[0], "持续:"); !ok {
		t.Fatal("duration fact missing")
	}
}

func TestClearedAlertAfterAlertedEpisode(t *testing.T) {
	f := newFixture(5 * time.Minute)
	f.set(cred("a", modelCooldown("m", at(time.Hour))))
	if got := f.poll(t); len(got) != 1 {
		t.Fatalf("setup: %+v", got)
	}
	f.advance(8 * time.Minute)
	f.set(cred("a"))
	got := f.poll(t)
	if len(got) != 1 {
		t.Fatalf("want 1 cleared alert, got %+v", got)
	}
	a := got[0]
	if a.Kind != domain.AlertRateLimitCleared || a.Severity != domain.SeverityInfo {
		t.Fatalf("alert = %+v", a)
	}
	if a.Scope != domain.ScopeModel || a.ScopeID != "m" || a.Credential.Key != "a" {
		t.Fatalf("scope wrong: %+v", a)
	}
	if v, _ := hasFact(a, "持续:"); v != "持续: 8m" {
		t.Fatalf("duration fact = %q", v)
	}
	if tallies := f.w.TakeTallies(); len(tallies) != 0 {
		t.Fatalf("alerted episode must not be tallied: %+v", tallies)
	}
	// Nothing more afterwards.
	f.advance(time.Minute)
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("unexpected %+v", got)
	}
}

func TestShortEpisodeTalliedAndResetAfterDigest(t *testing.T) {
	f := newFixture(5 * time.Minute)
	for i, d := range []time.Duration{time.Minute, 3 * time.Minute} {
		f.set(cred("a", modelCooldown("m", at(0))))
		f.poll(t)
		f.advance(d)
		f.set(cred("a"))
		if got := f.poll(t); len(got) != 0 {
			t.Fatalf("episode %d: short episode alerted: %+v", i, got)
		}
		f.advance(time.Minute)
	}
	f.set(cred("b", modelCooldown("m", at(0))))
	f.poll(t)
	f.advance(time.Minute)
	f.set(cred("b"))
	f.poll(t)

	tallies := f.w.TakeTallies()
	if len(tallies) != 2 {
		t.Fatalf("tallies = %+v", tallies)
	}
	if tallies[0].Credential.Key != "a" || tallies[0].Count != 2 || tallies[0].Longest != 3*time.Minute {
		t.Fatalf("a tally = %+v", tallies[0])
	}
	if tallies[1].Credential.Key != "b" || tallies[1].Count != 1 || tallies[1].Longest != time.Minute {
		t.Fatalf("b tally = %+v", tallies[1])
	}
	if again := f.w.TakeTallies(); len(again) != 0 {
		t.Fatalf("tallies not reset: %+v", again)
	}
}

func TestDisabledCredentialsAreSkipped(t *testing.T) {
	f := newFixture(time.Minute)
	c := cred("a", modelCooldown("m", at(time.Hour)))
	c.Disabled = true
	f.set(c)
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("disabled credential alerted: %+v", got)
	}
	f.advance(time.Hour)
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("disabled credential alerted later: %+v", got)
	}
}

func TestDisablingAnAlertedCredentialIsNotARecovery(t *testing.T) {
	f := newFixture(time.Minute)
	f.set(cred("a", modelCooldown("m", at(time.Hour))))
	if got := f.poll(t); len(got) != 1 {
		t.Fatalf("setup: %+v", got)
	}
	c := cred("a", modelCooldown("m", at(time.Hour)))
	c.Disabled = true
	f.set(c)
	f.advance(time.Minute)
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("disable announced as recovery: %+v", got)
	}
	if got := f.w.TakeTallies(); len(got) != 0 {
		t.Fatalf("disable counted as short episode: %+v", got)
	}
}

func TestUnavailableWithoutCooldownsIsOneAccountEpisode(t *testing.T) {
	f := newFixture(5 * time.Minute)
	c := cred("a")
	c.Unavailable = true
	c.NextRetryAfter = at(20 * time.Minute)
	f.set(c)
	got := f.poll(t)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Scope != domain.ScopeAccount || got[0].ScopeID != "" {
		t.Fatalf("scope = %v/%q", got[0].Scope, got[0].ScopeID)
	}
	if _, ok := hasFact(got[0], "状态码:"); ok {
		t.Fatal("no status is known for a synthetic episode")
	}
	if _, ok := hasFact(got[0], "预计恢复:"); !ok {
		t.Fatal("recovery fact missing")
	}
	// Unavailable without a retry time is not an episode.
	g := newFixture(time.Minute)
	bare := cred("b")
	bare.Unavailable = true
	g.set(bare)
	g.poll(t)
	g.advance(time.Hour)
	if got := g.poll(t); len(got) != 0 {
		t.Fatalf("unavailable without next_retry_after alerted: %+v", got)
	}
}

func TestCooldownsTakePrecedenceOverNextRetryAfter(t *testing.T) {
	f := newFixture(5 * time.Minute)
	c := cred("a", modelCooldown("m", at(time.Minute)))
	c.Unavailable = true
	c.NextRetryAfter = at(time.Hour)
	f.set(c)
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("synthetic episode double counted: %+v", got)
	}
}

func TestEpisodesAreIndependentPerScopeAndModel(t *testing.T) {
	f := newFixture(5 * time.Minute)
	f.set(cred("a",
		modelCooldown("m1", at(time.Hour)),
		modelCooldown("m2", at(time.Hour)),
		domain.Cooldown{Scope: "credential", RetryAt: at(time.Hour), HTTPStatus: 429},
	))
	got := f.poll(t)
	if len(got) != 3 {
		t.Fatalf("want 3 independent alerts, got %d: %+v", len(got), got)
	}
	var model, account int
	for _, a := range got {
		switch a.Scope {
		case domain.ScopeModel:
			model++
		case domain.ScopeAccount:
			account++
		}
	}
	if model != 2 || account != 1 {
		t.Fatalf("model=%d account=%d", model, account)
	}
	// One model recovers: only its cleared alert.
	f.advance(time.Minute)
	f.set(cred("a",
		modelCooldown("m2", at(time.Hour)),
		domain.Cooldown{Scope: "credential", RetryAt: at(time.Hour), HTTPStatus: 429},
	))
	got = f.poll(t)
	if len(got) != 1 || got[0].Kind != domain.AlertRateLimitCleared || got[0].ScopeID != "m1" {
		t.Fatalf("got %+v", got)
	}
}

func TestRemovedCredentialIsNotARecovery(t *testing.T) {
	f := newFixture(5 * time.Minute)
	f.set(cred("a", modelCooldown("m", at(time.Hour))), cred("b", modelCooldown("m", at(time.Minute))))
	if got := f.poll(t); len(got) != 1 {
		t.Fatalf("setup: %+v", got)
	}
	f.advance(time.Minute)
	f.set() // both credentials deleted from CPA
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("removal announced as recovery: %+v", got)
	}
	if got := f.w.TakeTallies(); len(got) != 0 {
		t.Fatalf("removal counted as short episode: %+v", got)
	}
}

func TestListErrorNeverClearsEpisodes(t *testing.T) {
	f := newFixture(time.Minute)
	f.set(cred("a", modelCooldown("m", at(time.Hour))))
	if got := f.poll(t); len(got) != 1 {
		t.Fatalf("setup: %+v", got)
	}
	f.mu.Lock()
	f.err = errors.New("cpa down")
	f.mu.Unlock()
	f.advance(time.Minute)
	if _, err := f.w.Poll(context.Background()); err == nil {
		t.Fatal("list error must be returned")
	}
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("outage produced alerts: %+v", got)
	}
}

func TestStatusMessageNeverSurfaces(t *testing.T) {
	const secret = "upstream-body-SECRET-do-not-leak"
	// The cpa package never reads status_message into a Credential; the only
	// free text that could reach the watcher is Status. Make sure nothing but
	// whitelisted fields is rendered into the alert.
	f := newFixture(time.Minute)
	c := cred("a", modelCooldown("m", at(time.Hour)))
	c.Unavailable = true
	f.set(c)
	got := f.poll(t)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), secret) || strings.Contains(fmt.Sprintf("%+v", got), secret) {
		t.Fatal("secret surfaced")
	}
	for _, field := range []string{"status_message", "StatusMessage"} {
		if strings.Contains(string(raw), field) {
			t.Fatalf("alert exposes %s", field)
		}
	}
}

func TestConcurrentPollAndTally(t *testing.T) {
	f := newFixture(time.Minute)
	f.set(cred("a", modelCooldown("m", at(time.Hour))))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := f.w.Poll(context.Background()); err != nil {
					t.Error(err)
					return
				}
				f.w.TakeTallies()
				if i == 0 && j%10 == 0 {
					f.advance(time.Second)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestDefaultAlertAfter(t *testing.T) {
	w := New(func(context.Context) ([]domain.Credential, error) { return nil, nil }, Options{})
	if w.alertAfter != DefaultAlertAfter {
		t.Fatalf("alertAfter = %v", w.alertAfter)
	}
}

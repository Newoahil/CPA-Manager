// Package cooldown watches CPA's own rate-limit cooldown data.
//
// CPA cools a credential down after an upstream 429 and recovers it by itself,
// so a short cooldown is routine and not worth an alert. The watcher polls only
// the credential list (never a quota or upstream endpoint), tracks each cooldown
// "episode" in memory and raises:
//
//   - one domain.AlertRateLimited when an episode has lasted COOLDOWN_ALERT_AFTER,
//     or when CPA's own retry time already guarantees it will;
//   - one domain.AlertRateLimitCleared when an alerted episode ends;
//   - a per-credential tally (count + longest) for episodes that ended quietly,
//     which the daily digest reports and then resets.
//
// State is in memory only: a restart forgets every episode, so a cooldown that
// spans a restart is timed from the first poll after it (and an alerted episode
// that ends during downtime never produces a cleared alert). That is deliberate:
// persisting it would need a state schema change for a minutes-long signal.
//
// Nothing here ever reads CPA's status_message: it can carry verbatim upstream
// response text. Only the cpa package's whitelisted cooldown fields are used.
package cooldown

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// Lister returns the current CPA credential list. It is the only upstream call
// the watcher makes.
type Lister func(ctx context.Context) ([]domain.Credential, error)

// Options configures a Watcher.
type Options struct {
	// AlertAfter is how long an episode must last before it is alerted. A
	// non-positive value means DefaultAlertAfter.
	AlertAfter time.Duration
	// Now is the clock; nil means time.Now. Injected by tests.
	Now func() time.Time
}

// DefaultAlertAfter is used when Options.AlertAfter is not positive.
const DefaultAlertAfter = 5 * time.Minute

// accountScopeName is the cooldown scope used for the synthetic episode of a
// credential that is unavailable with a next_retry_after but lists no cooldown
// entries. It matches CPA's own "credential" scope so the two never split one
// outage into two episodes.
const accountScopeName = "credential"

type episode struct {
	cred       domain.Credential
	scope      string
	modelKey   string
	firstSeen  time.Time
	retryAt    *time.Time
	httpStatus int
	alerted    bool
}

type tally struct {
	cred    domain.Credential
	count   int
	longest time.Duration
}

// Watcher tracks cooldown episodes. It is safe for concurrent use.
type Watcher struct {
	list       Lister
	now        func() time.Time
	alertAfter time.Duration

	mu       sync.Mutex
	episodes map[string]*episode
	tallies  map[string]*tally
}

// New builds a Watcher over list.
func New(list Lister, opts Options) *Watcher {
	w := &Watcher{
		list:       list,
		now:        opts.Now,
		alertAfter: opts.AlertAfter,
		episodes:   map[string]*episode{},
		tallies:    map[string]*tally{},
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.alertAfter <= 0 {
		w.alertAfter = DefaultAlertAfter
	}
	return w
}

// observation is one cooldown seen in the current list.
type observation struct {
	cred       domain.Credential
	scope      string
	modelKey   string
	retryAt    *time.Time
	httpStatus int
}

func episodeKey(credKey, scope, modelKey string) string {
	return credKey + "|" + scope + "|" + modelKey
}

// slim drops the cooldown payload from a credential so alerts and tallies carry
// identity only.
func slim(c domain.Credential) domain.Credential {
	c.Cooldowns = nil
	c.NextRetryAfter = nil
	return c
}

// observe derives the episodes a credential is currently in.
func observe(c domain.Credential, into map[string]*observation) {
	base := slim(c)
	add := func(scope, modelKey string, retryAt *time.Time, status int) {
		key := episodeKey(c.Key, scope, modelKey)
		if o, ok := into[key]; ok {
			// Duplicate entries for one episode: keep the latest known retry
			// time and the first known status.
			if retryAt != nil && (o.retryAt == nil || retryAt.After(*o.retryAt)) {
				o.retryAt = retryAt
			}
			if o.httpStatus == 0 {
				o.httpStatus = status
			}
			return
		}
		into[key] = &observation{cred: base, scope: scope, modelKey: modelKey, retryAt: retryAt, httpStatus: status}
	}
	if len(c.Cooldowns) > 0 {
		for _, cd := range c.Cooldowns {
			scope := cd.Scope
			if scope == "" {
				scope = accountScopeName
			}
			add(scope, cd.ModelKey, cd.RetryAt, cd.HTTPStatus)
		}
		return
	}
	if c.Unavailable && c.NextRetryAfter != nil {
		add(accountScopeName, "", c.NextRetryAfter, 0)
	}
}

// Poll lists credentials once and returns the alerts that became due.
//
// A list failure returns the error and leaves every episode untouched: not
// being able to read CPA is not evidence that a cooldown ended.
func (w *Watcher) Poll(ctx context.Context) ([]domain.Alert, error) {
	creds, err := w.list(ctx)
	if err != nil {
		return nil, err
	}
	now := w.now()

	observed := map[string]*observation{}
	active := map[string]bool{} // enabled credentials present in this list
	for _, c := range creds {
		if c.Disabled {
			continue
		}
		active[c.Key] = true
		observe(c, observed)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	var alerts []domain.Alert

	for _, key := range sortedKeys(observed) {
		o := observed[key]
		ep := w.episodes[key]
		if ep == nil {
			ep = &episode{scope: o.scope, modelKey: o.modelKey, firstSeen: now}
			w.episodes[key] = ep
		}
		ep.cred, ep.retryAt, ep.httpStatus = o.cred, o.retryAt, o.httpStatus
		if !ep.alerted && w.due(ep, now) {
			ep.alerted = true
			alerts = append(alerts, w.limitedAlert(ep, now))
		}
	}

	for _, key := range sortedEpisodeKeys(w.episodes) {
		if _, still := observed[key]; still {
			continue
		}
		ep := w.episodes[key]
		delete(w.episodes, key)
		if !active[ep.cred.Key] {
			// The credential was disabled or removed: that is not a recovery,
			// so it is neither announced nor counted.
			continue
		}
		if ep.alerted {
			alerts = append(alerts, w.clearedAlert(ep, now))
			continue
		}
		t := w.tallies[ep.cred.Key]
		if t == nil {
			t = &tally{cred: ep.cred}
			w.tallies[ep.cred.Key] = t
		}
		t.count++
		if d := now.Sub(ep.firstSeen); d > t.longest {
			t.longest = d
		}
	}
	return alerts, nil
}

// due reports whether the episode deserves its alert now: it has lasted long
// enough, or CPA's own retry time says it certainly will.
func (w *Watcher) due(ep *episode, now time.Time) bool {
	if now.Sub(ep.firstSeen) >= w.alertAfter {
		return true
	}
	return ep.retryAt != nil && ep.retryAt.Sub(ep.firstSeen) >= w.alertAfter
}

// TakeTallies returns the short-episode tallies recorded since the previous
// call and resets them. The result is ordered by count, then longest duration.
func (w *Watcher) TakeTallies() []domain.RateLimitTally {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.tallies) == 0 {
		return nil
	}
	out := make([]domain.RateLimitTally, 0, len(w.tallies))
	for _, t := range w.tallies {
		out = append(out, domain.RateLimitTally{Credential: t.cred, Count: t.count, Longest: t.longest})
	}
	w.tallies = map[string]*tally{}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Longest != out[j].Longest {
			return out[i].Longest > out[j].Longest
		}
		return out[i].Credential.Key < out[j].Credential.Key
	})
	return out
}

func alertScope(ep *episode) (domain.QuotaScope, string) {
	if ep.scope == "model" {
		return domain.ScopeModel, ep.modelKey
	}
	return domain.ScopeAccount, ""
}

func minutes(d time.Duration) int {
	if d < 0 {
		return 0
	}
	return int(d / time.Minute)
}

func (w *Watcher) limitedAlert(ep *episode, now time.Time) domain.Alert {
	scope, scopeID := alertScope(ep)
	elapsed := now.Sub(ep.firstSeen)
	var facts []string
	if ep.httpStatus != 0 {
		facts = append(facts, fmt.Sprintf("%s %d", domain.FactPrefixStatus, ep.httpStatus))
	}
	if ep.retryAt != nil {
		facts = append(facts, fmt.Sprintf("%s %s", domain.FactPrefixRecovery, ep.retryAt.UTC().Format(time.RFC3339)))
	}
	facts = append(facts, fmt.Sprintf("%s %dm", domain.FactPrefixDuration, minutes(elapsed)))
	return domain.Alert{
		Scope:      scope,
		ScopeID:    scopeID,
		Kind:       domain.AlertRateLimited,
		Severity:   domain.SeverityWarn,
		Evidence:   domain.EvidenceConfirmed,
		Credential: ep.cred,
		Title:      ep.cred.Label() + " 被限流，冷却中",
		Detail:     fmt.Sprintf("CPA 将该凭证（%s）置于冷却，已持续约 %d 分钟，冷却结束后会自动恢复。", domain.ScopeText(scope, scopeID), minutes(elapsed)),
		Facts:      facts,
		OccurredAt: now,
	}
}

func (w *Watcher) clearedAlert(ep *episode, now time.Time) domain.Alert {
	scope, scopeID := alertScope(ep)
	elapsed := now.Sub(ep.firstSeen)
	var facts []string
	if ep.httpStatus != 0 {
		facts = append(facts, fmt.Sprintf("%s %d", domain.FactPrefixStatus, ep.httpStatus))
	}
	facts = append(facts, fmt.Sprintf("%s %dm", domain.FactPrefixDuration, minutes(elapsed)))
	return domain.Alert{
		Scope:      scope,
		ScopeID:    scopeID,
		Kind:       domain.AlertRateLimitCleared,
		Severity:   domain.SeverityInfo,
		Evidence:   domain.EvidenceConfirmed,
		Credential: ep.cred,
		Title:      ep.cred.Label() + " 限流已解除",
		Detail:     fmt.Sprintf("CPA 已结束该凭证（%s）的冷却，共持续约 %d 分钟。", domain.ScopeText(scope, scopeID), minutes(elapsed)),
		Facts:      facts,
		OccurredAt: now,
	}
}

func sortedKeys(m map[string]*observation) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedEpisodeKeys(m map[string]*episode) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

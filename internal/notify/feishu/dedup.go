package feishu

import (
	"sync"
	"time"
)

// dedup defaults. The set is a soft guard against Feishu re-delivering the same
// event (long-connection reconnects and at-least-once delivery), not a durable
// record: it lives only in memory and is empty again after a restart, which is
// fine because a redelivery matters only within a short window.
const (
	dedupCapacity = 512
	dedupTTL      = 10 * time.Minute
)

// dedup is a bounded, concurrency-safe set of recently handled event keys.
//
// Eviction is whichever comes first: a key older than the TTL, or the oldest
// key once the set is full. Both bounds are enforced so a burst cannot grow it
// without limit and a quiet period cannot keep a key forever.
type dedup struct {
	mu    sync.Mutex
	cap   int
	ttl   time.Duration
	now   func() time.Time
	seen  map[string]time.Time
	order []string // insertion order of the keys currently in seen
}

func newDedup(capacity int, ttl time.Duration) *dedup {
	if capacity <= 0 {
		capacity = dedupCapacity
	}
	if ttl <= 0 {
		ttl = dedupTTL
	}
	return &dedup{
		cap:  capacity,
		ttl:  ttl,
		now:  time.Now,
		seen: make(map[string]time.Time, capacity),
	}
}

// mark attempts to record key. It returns true when the caller owns the key and
// should proceed, false when the key was already recorded (or reserved by a
// concurrent caller) and the event must be ignored.
func (d *dedup) mark(key string) bool {
	if key == "" {
		// Without a stable key we cannot dedupe; process rather than risk
		// silently dropping a real event.
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.evict(now)
	if _, ok := d.seen[key]; ok {
		return false
	}
	d.seen[key] = now
	d.order = append(d.order, key)
	d.trimLocked()
	return true
}

// forget removes key so it can be marked again. It is used when a reservation
// made by mark cannot be honoured (e.g. the bot is shutting down and the task
// cannot start): the event was not actually handled, so replaying it must be
// allowed.
func (d *dedup) forget(key string) {
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[key]; !ok {
		return
	}
	delete(d.seen, key)
	for i, k := range d.order {
		if k == key {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
}

// evict drops keys past their TTL. The insertion order lets it stop at the
// first live key, because times are non-decreasing in that order.
func (d *dedup) evict(now time.Time) {
	kept := d.order[:0]
	for _, k := range d.order {
		if at, ok := d.seen[k]; ok && now.Sub(at) >= d.ttl {
			delete(d.seen, k)
			continue
		}
		kept = append(kept, k)
	}
	d.order = kept
}

// trimLocked enforces the capacity bound by dropping the oldest keys.
func (d *dedup) trimLocked() {
	for len(d.order) > d.cap {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, oldest)
	}
}

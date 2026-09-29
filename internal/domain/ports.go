package domain

import "context"

// Collector gathers quota snapshots for one upstream family.
//
// A Collector must never return a partial error for the whole batch when only
// one credential failed: failures belong in the individual snapshot (OK=false,
// Err set). A returned error means the collector itself could not run at all.
type Collector interface {
	Name() string
	Collect(ctx context.Context) ([]QuotaSnapshot, error)
}

// Notifier delivers one outbound message to a channel.
//
// Implementations must be safe for concurrent use and must never log or embed
// secrets (cookies, tokens, management keys).
type Notifier interface {
	Name() string
	Notify(ctx context.Context, msg Message) error
}

// QuotaRefresher performs an on-demand, read-only re-collection.
//
// It is what the Feishu "refresh quota" button and the @Bot query call. It must
// not mutate any CPA credential state.
type QuotaRefresher interface {
	RefreshNow(ctx context.Context) (Report, error)
	// LastReport returns the most recent evaluated report without hitting
	// upstreams. ok is false before the first successful cycle.
	LastReport() (Report, bool)
}

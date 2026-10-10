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

// ReportSource exposes the most recent evaluated report without touching any
// upstream. The status page reads it, and the Feishu bot serves @Bot queries
// from it: collection is entirely scheduler-driven, so a query never triggers a
// scrape.
type ReportSource interface {
	// LastReport returns the most recent evaluated report. ok is false before
	// the first successful cycle.
	LastReport() (Report, bool)
}

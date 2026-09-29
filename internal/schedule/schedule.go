// Package schedule implements the daily summary trigger.
//
// It fires at fixed local wall-clock times every day, weekends and public
// holidays included (explicitly confirmed by the user). It carries no calendar
// logic of its own; the holiday context belongs to the report, not the trigger.
package schedule

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"
)

var defaultTimes = []string{"10:00", "17:00"}

// Scheduler fires fn at the configured local times each day.
type Scheduler struct {
	loc   *time.Location
	times []dailyTime
	fn    func(context.Context)

	// now and after are injectable so tests drive time without sleeping.
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
}

type dailyTime struct {
	hour, min int
}

// New builds a scheduler.
//
// times are "HH:MM" strings in loc. Invalid entries are dropped and the default
// 10:00,17:00 is used when none remain, so a typo in configuration degrades to a
// working schedule instead of a dead cron. A nil location falls back to
// time.Local.
func New(loc *time.Location, times []string, fn func(context.Context)) *Scheduler {
	if loc == nil {
		loc = time.Local
	}
	parsed := parseTimes(times)
	if len(parsed) == 0 {
		parsed = parseTimes(defaultTimes)
	}
	return &Scheduler{
		loc:   loc,
		times: parsed,
		fn:    fn,
		now:   time.Now,
		after: time.After,
	}
}

func parseTimes(raw []string) []dailyTime {
	seen := map[dailyTime]bool{}
	out := []dailyTime{}
	for _, spec := range raw {
		t, err := time.Parse("15:04", spec)
		if err != nil {
			continue
		}
		d := dailyTime{hour: t.Hour(), min: t.Minute()}
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].hour != out[j].hour {
			return out[i].hour < out[j].hour
		}
		return out[i].min < out[j].min
	})
	return out
}

// Run blocks until ctx is cancelled, firing fn once per due time.
//
// Semantics:
//   - A time that already passed when the process started is NOT back-filled;
//     only today's still-future times (and every subsequent day) fire.
//   - If fn is still running when the next time arrives, that tick is skipped
//     (no re-entry).
//   - Times are resolved in loc on each day, so DST shifts are handled by the
//     wall clock: a 10:00 trigger stays at 10:00 local even across a change.
//   - The timer is re-armed after every wake-up, which also absorbs system
//     clock changes.
//   - It returns ctx.Err() when the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.fn == nil {
		return errors.New("schedule: nil function")
	}
	log := slog.Default().With("component", "schedule")

	// idle is nil while no invocation is running; otherwise it is the channel
	// closed when the in-flight fn returns. Only this goroutine touches it, so
	// there is no race.
	var idle <-chan struct{}

	for {
		next := s.nextAfter(s.now())
		timer := s.after(time.Until(next))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer:
			if idle != nil {
				log.Warn("scheduled task still running; skipping tick", "at", next.Format(time.RFC3339))
				continue
			}
			log.Info("scheduled task triggered", "at", next.Format(time.RFC3339))
			done := make(chan struct{})
			idle = done
			go func() {
				defer close(done)
				s.fn(ctx)
			}()
		case <-idle:
			idle = nil
		}
	}
}

// nextAfter returns the earliest configured time strictly after t.
//
// Candidates are built with time.Date in loc rather than by adding a duration
// to midnight: time.Date interprets the fields as local wall-clock time and
// normalises DST gaps, so a 10:00 trigger remains 10:00 local across a
// spring-forward or fall-back transition.
func (s *Scheduler) nextAfter(t time.Time) time.Time {
	local := t.In(s.loc)
	for _, d := range s.times {
		cand := time.Date(local.Year(), local.Month(), local.Day(), d.hour, d.min, 0, 0, s.loc)
		if cand.After(t) {
			return cand
		}
	}
	// All of today's times have passed: first configured time tomorrow.
	d := s.times[0]
	return time.Date(local.Year(), local.Month(), local.Day()+1, d.hour, d.min, 0, 0, s.loc)
}

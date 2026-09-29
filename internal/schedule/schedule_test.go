package schedule

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeTimer hands the test control of every timer Run arms.
type fakeTimer struct {
	chans chan chan time.Time
}

func newFakeTimer() *fakeTimer { return &fakeTimer{chans: make(chan chan time.Time, 16)} }

func (f *fakeTimer) after(time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	f.chans <- c
	return c
}

// nextTimer returns the next armed timer channel, failing if Run never armed one.
func (f *fakeTimer) nextTimer(t *testing.T) chan time.Time {
	t.Helper()
	select {
	case c := <-f.chans:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("Run never armed a timer")
		return nil
	}
}

func TestParseTimesDefaultsAndOrdering(t *testing.T) {
	got := parseTimes([]string{"17:00", "10:00", "10:00", "bad", "25:99"})
	if len(got) != 2 {
		t.Fatalf("expected 2 valid times, got %d: %+v", len(got), got)
	}
	if got[0].hour != 10 || got[1].hour != 17 {
		t.Errorf("times not sorted/deduped: %+v", got)
	}
}

func TestNewFallsBackToDefaults(t *testing.T) {
	s := New(nil, []string{"nope"}, func(context.Context) {})
	if len(s.times) != 2 || s.times[0].hour != 10 || s.times[1].hour != 17 {
		t.Fatalf("default times not applied: %+v", s.times)
	}
}

func TestNextAfterNoBackfill(t *testing.T) {
	loc := time.UTC
	s := New(loc, []string{"10:00", "17:00"}, func(context.Context) {})

	// Process starts at noon: today's 10:00 must NOT be back-filled.
	next := s.nextAfter(time.Date(2026, 3, 13, 12, 0, 0, 0, loc))
	if want := time.Date(2026, 3, 13, 17, 0, 0, 0, loc); !next.Equal(want) {
		t.Errorf("noon -> %v, want %v", next, want)
	}

	// Starts before the first time.
	next = s.nextAfter(time.Date(2026, 3, 13, 9, 0, 0, 0, loc))
	if want := time.Date(2026, 3, 13, 10, 0, 0, 0, loc); !next.Equal(want) {
		t.Errorf("09:00 -> %v, want %v", next, want)
	}

	// Starts after the last time: first time tomorrow.
	next = s.nextAfter(time.Date(2026, 3, 13, 18, 30, 0, 0, loc))
	if want := time.Date(2026, 3, 14, 10, 0, 0, 0, loc); !next.Equal(want) {
		t.Errorf("18:30 -> %v, want %v", next, want)
	}

	// Exactly on a boundary advances to the next slot, never repeats.
	single := New(loc, []string{"10:00"}, func(context.Context) {})
	next = single.nextAfter(time.Date(2026, 3, 13, 10, 0, 0, 0, loc))
	if want := time.Date(2026, 3, 14, 10, 0, 0, 0, loc); !next.Equal(want) {
		t.Errorf("on-boundary -> %v, want %v", next, want)
	}
}

func TestNextAfterEveryDayIncludingWeekend(t *testing.T) {
	loc := time.UTC
	s := New(loc, []string{"10:00"}, func(context.Context) {})
	// 2026-03-14 is a Saturday; the scheduler must still fire.
	saturday := time.Date(2026, 3, 14, 8, 0, 0, 0, loc)
	if saturday.Weekday() != time.Saturday {
		t.Fatalf("test date is %v, expected Saturday", saturday.Weekday())
	}
	next := s.nextAfter(saturday)
	if want := time.Date(2026, 3, 14, 10, 0, 0, 0, loc); !next.Equal(want) {
		t.Errorf("weekend -> %v, want %v", next, want)
	}
}

func TestDSTKeepsWallClock(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}
	s := New(loc, []string{"10:00"}, func(context.Context) {})

	// Spring forward 2026-03-08 02:00 -> 03:00. Trigger must stay 10:00 local.
	next := s.nextAfter(time.Date(2026, 3, 8, 1, 0, 0, 0, loc))
	if next.Hour() != 10 || next.Day() != 8 {
		t.Errorf("spring-forward day -> %v (local %v), want 10:00 local same day", next, next.In(loc))
	}
	// Fall back 2026-11-01. Trigger must still be 10:00 local.
	next = s.nextAfter(time.Date(2026, 11, 1, 1, 30, 0, 0, loc))
	if next.Hour() != 10 || next.Day() != 1 {
		t.Errorf("fall-back day -> %v (local %v), want 10:00 local same day", next, next.In(loc))
	}
}

func TestRunFiresOnce(t *testing.T) {
	ft := newFakeTimer()
	s := New(time.UTC, []string{"10:00"}, func(context.Context) {})
	s.after = ft.after

	calls := make(chan struct{}, 4)
	s.fn = func(context.Context) { calls <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	timer := ft.nextTimer(t)
	timer <- time.Now()

	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("fn was not called after the timer fired")
	}
}

func TestRunSkipsWhenStillRunning(t *testing.T) {
	ft := newFakeTimer()
	s := New(time.UTC, []string{"10:00"}, func(context.Context) {})
	s.after = ft.after

	var mu sync.Mutex
	count := 0
	release := make(chan struct{})
	s.fn = func(context.Context) {
		mu.Lock()
		count++
		mu.Unlock()
		<-release
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	first := ft.nextTimer(t)
	first <- time.Now()
	// Let the first invocation actually start.
	time.Sleep(50 * time.Millisecond)

	// Second tick arrives while the first fn is still blocked: must be skipped.
	second := ft.nextTimer(t)
	second <- time.Now()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := count
	mu.Unlock()
	close(release)
	if got != 1 {
		t.Fatalf("fn ran %d times, want 1 (second tick should be skipped)", got)
	}
}

func TestRunReturnsOnCancel(t *testing.T) {
	ft := newFakeTimer()
	s := New(time.UTC, []string{"10:00"}, func(context.Context) {})
	s.after = ft.after

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	_ = ft.nextTimer(t) // ensure Run is blocked in select
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunNilFunc(t *testing.T) {
	s := New(time.UTC, []string{"10:00"}, nil)
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("expected error for nil function")
	}
}

package feishu

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestTaskGroupRunsAndWaits(t *testing.T) {
	g := newTaskGroup()
	defer g.Close()

	var ran int64
	if !g.Go(func(context.Context) { atomic.AddInt64(&ran, 1) }) {
		t.Fatal("Go refused on an open group")
	}
	g.wait()
	if got := atomic.LoadInt64(&ran); got != 1 {
		t.Fatalf("task ran %d times, want 1", got)
	}
}

// TestTaskGroupCloseCancelsAndDrains: Close cancels the context the tasks see
// and waits for them to observe it.
func TestTaskGroupCloseCancelsAndDrains(t *testing.T) {
	g := newTaskGroup()
	cancelled := make(chan struct{})
	g.Go(func(ctx context.Context) {
		<-ctx.Done()
		close(cancelled)
	})

	done := make(chan struct{})
	go func() { g.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return; a task was not drained")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("Close returned without cancelling the task context")
	}
}

// TestTaskGroupRefusesAfterClose: a task scheduled during shutdown is refused
// rather than added to a WaitGroup that Wait may already be observing.
func TestTaskGroupRefusesAfterClose(t *testing.T) {
	g := newTaskGroup()
	g.Close()
	if g.Go(func(context.Context) { t.Error("task ran after Close") }) {
		t.Fatal("Go accepted a task after Close")
	}
}

// TestTaskGroupNoGoroutineLeak: every goroutine Go started has exited by the
// time Close returns.
func TestTaskGroupNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	g := newTaskGroup()
	for i := 0; i < 32; i++ {
		g.Go(func(ctx context.Context) { <-ctx.Done() })
	}
	g.Close()

	// Close waits, so the count should return to the pre-test baseline. Allow a
	// little slack for unrelated runtime goroutines.
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Fatalf("goroutines before=%d after=%d, suspected leak", before, after)
	}
}

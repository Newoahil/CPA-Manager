package feishu

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/notify/render"
)

// --- unit tests for the dedup set -----------------------------------------

func TestDedupMarksFirstOnly(t *testing.T) {
	d := newDedup(8, time.Minute)
	if !d.mark("a") {
		t.Fatal("first mark should win")
	}
	if d.mark("a") {
		t.Fatal("second mark of the same key should lose")
	}
	if !d.mark("b") {
		t.Fatal("a different key should win")
	}
}

func TestDedupEmptyKeyAlwaysProcesses(t *testing.T) {
	d := newDedup(8, time.Minute)
	for i := 0; i < 3; i++ {
		if !d.mark("") {
			t.Fatal("an empty key must not be treated as a duplicate")
		}
	}
}

func TestDedupCapacityEvictsOldest(t *testing.T) {
	d := newDedup(2, time.Hour)
	d.mark("a")
	d.mark("b")
	d.mark("c") // evicts "a"
	if !d.mark("a") {
		t.Fatal("the oldest key should have been evicted and be markable again")
	}
	if d.mark("c") {
		t.Fatal("the newest key should still be remembered")
	}
}

func TestDedupTTLEviction(t *testing.T) {
	now := time.Unix(0, 0)
	d := newDedup(8, time.Minute)
	d.now = func() time.Time { return now }
	if !d.mark("a") {
		t.Fatal("first mark should win")
	}
	now = now.Add(30 * time.Second)
	if d.mark("a") {
		t.Fatal("within the TTL the key must still be a duplicate")
	}
	now = now.Add(31 * time.Second) // total 61s > 60s TTL
	if !d.mark("a") {
		t.Fatal("after the TTL the key must be markable again")
	}
}

// TestDedupConcurrentSingleWinner: 10 goroutines racing on one key, exactly one
// wins.
func TestDedupConcurrentSingleWinner(t *testing.T) {
	d := newDedup(64, time.Minute)
	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if d.mark("same") {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
}

// --- handler-level dedup ---------------------------------------------------

// messageEvent builds an inbound event with a header event id.
func messageEvent(chatID, messageID, eventID, content string) *larkim.P2MessageReceiveV1 {
	ev := msgEvent(chatID, messageID, content, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	ev.EventV2Base = &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: eventID}}
	return ev
}

func TestHandleMessageDeduplicatesByEventID(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	ev := messageEvent(targetChat, "om_dup", "ev-1", `{"text":"@_user_1 额度"}`)
	for i := 0; i < 3; i++ {
		if err := b.HandleMessageV1(context.Background(), ev); err != nil {
			t.Fatalf("handler error: %v", err)
		}
	}
	waitAsync(b)
	if r.callCount() != 1 {
		t.Errorf("refresh calls = %d, want 1 (redelivered events ignored)", r.callCount())
	}
	if _, _, replyCards := s.counts(); replyCards != 1 {
		t.Errorf("card replies = %d, want 1", replyCards)
	}
}

// TestHandleMessageConcurrentDedup: the same event delivered 10 times at once
// must be processed once.
func TestHandleMessageConcurrentDedup(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)
	ev := messageEvent(targetChat, "om_race", "ev-race", `{"text":"@_user_1 额度"}`)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = b.HandleMessageV1(context.Background(), ev)
		}()
	}
	close(start)
	wg.Wait()
	waitAsync(b)

	if r.callCount() != 1 {
		t.Errorf("refresh calls = %d, want exactly 1", r.callCount())
	}
	if len(s.replyCards) != 1 {
		t.Errorf("card replies = %d, want exactly 1", len(s.replyCards))
	}
}

// TestHandleMessageFallsBackToMessageID: without a header event id the message
// id still prevents a second reply.
func TestHandleMessageFallsBackToMessageID(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)
	for i := 0; i < 2; i++ {
		ev := msgEvent(targetChat, "om_nhdr", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
		if err := b.HandleMessageV1(context.Background(), ev); err != nil {
			t.Fatalf("handler error: %v", err)
		}
	}
	waitAsync(b)
	_, _, replyCards := s.counts()
	if r.callCount() != 1 || replyCards != 1 {
		t.Fatalf("refresh=%d cards=%d, want 1/1", r.callCount(), replyCards)
	}
}

// TestHandleMessageReturnsImmediately is the fix for the duplicate-reply root
// cause: a slow collection must not block the event handler, or Feishu
// redelivers the event and the user sees two replies.
func TestHandleMessageReturnsImmediately(t *testing.T) {
	const collection = 300 * time.Millisecond
	r := &fakeRefresher{report: testReport(), takes: collection, block: false}
	b, s := newTestBot(t, r)
	b.messageBudget = 5 * time.Second

	ev := messageEvent(targetChat, "om_fast", "ev-fast", `{"text":"@_user_1 额度"}`)
	start := time.Now()
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	elapsed := time.Since(start)
	// The handler must return well before the collection could finish.
	if elapsed > collection/2 {
		t.Fatalf("handler blocked for %v, must return immediately", elapsed)
	}
	// And the reply is still delivered, eventually.
	waitAsync(b)
	if _, _, replyCards := s.counts(); replyCards != 1 {
		t.Fatalf("async reply not sent: cards=%d", replyCards)
	}
}

// trackingRefresher records the peak number of concurrent collections, which is
// what the cap actually bounds. Asserting on peak rather than on how many cards
// were produced avoids a race where a late goroutine acquires a slot a fast one
// already released.
type trackingRefresher struct {
	fakeRefresher
	mu      sync.Mutex
	current int
	peak    int
}

func (t *trackingRefresher) RefreshNow(ctx context.Context) (domain.Report, error) {
	t.mu.Lock()
	t.current++
	if t.current > t.peak {
		t.peak = t.current
	}
	t.mu.Unlock()

	select {
	case <-time.After(150 * time.Millisecond):
	case <-ctx.Done():
	}
	t.mu.Lock()
	t.current--
	t.mu.Unlock()
	return t.report, nil
}

// TestQueryConcurrencyCap: no more than maxInflightQueries collections run at
// once, and the refused queries are told to retry rather than dropped.
func TestQueryConcurrencyCap(t *testing.T) {
	r := &trackingRefresher{fakeRefresher: fakeRefresher{report: testReport()}}
	b, s := newTestBot(t, r)
	b.messageBudget = 5 * time.Second

	total := maxInflightQueries + 3
	for i := 0; i < total; i++ {
		ev := messageEvent(targetChat, fmt.Sprintf("om_cap_%d", i), fmt.Sprintf("ev-cap-%d", i), `{"text":"@_user_1 额度"}`)
		if err := b.HandleMessageV1(context.Background(), ev); err != nil {
			t.Fatalf("handler error: %v", err)
		}
	}
	waitAsync(b)

	r.mu.Lock()
	peak := r.peak
	r.mu.Unlock()
	if peak > maxInflightQueries {
		t.Errorf("peak concurrent collections = %d, exceeds the cap %d", peak, maxInflightQueries)
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d; the test did not exercise parallelism", peak)
	}

	// Every query is answered, either with a card or with the busy note.
	_, replies, replyCards := s.counts()
	if got := replyCards + replies; got != total {
		t.Fatalf("answers = %d, want %d (no query may be silently dropped)", got, total)
	}
	busy := 0
	for _, re := range s.snapshotReplies() {
		if strings.Contains(re, "查询较多") {
			busy++
		}
	}
	if busy == 0 {
		t.Error("over-cap queries were not told to retry")
	}
}

// TestQueryConcurrencyCapDoesNotDropTheUser: a refused query still gets a reply
// (no silent drop).
func TestQueryConcurrencyCapDoesNotDropTheUser(t *testing.T) {
	r := &trackingRefresher{fakeRefresher: fakeRefresher{report: testReport()}}
	b, s := newTestBot(t, r)
	b.messageBudget = 5 * time.Second
	total := maxInflightQueries + 1
	for i := 0; i < total; i++ {
		ev := messageEvent(targetChat, fmt.Sprintf("om_nd_%d", i), fmt.Sprintf("ev-nd-%d", i), `{"text":"@_user_1 额度"}`)
		_ = b.HandleMessageV1(context.Background(), ev)
	}
	waitAsync(b)
	_, replies, replyCards := s.counts()
	if got := replyCards + replies; got != total {
		t.Fatalf("every query must be answered: got %d answers for %d queries", got, total)
	}
}

func TestHandleCardActionDeduplicates(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)
	ev := cardEvent(targetChat, "ou_user1", render.RefreshAction)
	ev.EventV2Base = &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: "cev-1"}}

	first, err := b.HandleCardActionTrigger(context.Background(), ev)
	if err != nil {
		t.Fatalf("first callback error: %v", err)
	}
	if first.Toast == nil || !strings.Contains(first.Toast.Content, "正在刷新") {
		t.Fatalf("first callback should return refreshing toast: %+v", first.Toast)
	}
	second, err := b.HandleCardActionTrigger(context.Background(), ev)
	if err != nil {
		t.Fatalf("second callback error: %v", err)
	}
	if second.Toast == nil || !strings.Contains(second.Toast.Content, "已处理") {
		t.Fatalf("duplicate callback should return already handled toast: %+v", second.Toast)
	}
	waitAsync(b)
	if r.callCount() != 1 {
		t.Errorf("refresh calls = %d, want 1", r.callCount())
	}
	if len(s.patchCards) != 1 {
		t.Errorf("patchCards = %d, want 1", len(s.patchCards))
	}
}

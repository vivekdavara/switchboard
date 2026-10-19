package budget

import (
	"sync"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
)

// clock is a time the test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)} }

// retries counts how many of n retries the budget allows now.
func retries(b *Budget, n int) int {
	allowed := 0
	for range n {
		if b.Retry() {
			allowed++
		}
	}
	return allowed
}

func TestRetriesAreAShareOfTheRequests(t *testing.T) {
	c := newClock()
	b := New(config.RetryBudget{Ratio: 0.2}, c.now)
	if b.Retry() {
		t.Fatal("no requests, no floor: no retry")
	}
	for range 100 {
		b.Request()
	}
	if got := retries(b, 30); got != 20 {
		t.Errorf("100 requests at a ratio of 0.2 allowed %d retries, want 20", got)
	}
	// Five more requests buy one more retry.
	for range 5 {
		b.Request()
	}
	if got := retries(b, 3); got != 1 {
		t.Errorf("5 more requests allowed %d more retries, want 1", got)
	}
}

func TestTheFloorAllowsRetriesWithoutTraffic(t *testing.T) {
	c := newClock()
	b := New(config.RetryBudget{MinPerSecond: 1.5}, c.now)
	if got := retries(b, 20); got != 15 {
		t.Errorf("a floor of 1.5 a second allowed %d retries in a window, want 15", got)
	}
	c.add(Window)
	if got := retries(b, 20); got != 15 {
		t.Errorf("a window later: %d, want 15 again", got)
	}
}

func TestRequestsAndRetriesExpireAfterTheWindow(t *testing.T) {
	c := newClock()
	b := New(config.RetryBudget{Ratio: 0.5}, c.now)
	for range 10 {
		b.Request()
	}
	if got := retries(b, 5); got != 5 {
		t.Fatalf("%d retries, want 5", got)
	}
	// 9 seconds on, the requests still count, and so do the retries made: none is left.
	c.add(9 * time.Second)
	if b.Retry() {
		t.Error("a retry allowed 9 s later, when the window still holds 10 requests and 5 retries")
	}
	// At 10 s the first second leaves the window, with its requests and retries.
	c.add(time.Second)
	if b.Retry() {
		t.Error("a retry allowed with no requests left in the window")
	}
	for range 2 {
		b.Request()
	}
	if got := retries(b, 3); got != 1 {
		t.Errorf("after the window: %d retries for 2 requests, want 1", got)
	}
}

func TestAnOldSlotStopsCountingEvenIfNoSecondReusesIt(t *testing.T) {
	c := newClock()
	b := New(config.RetryBudget{Ratio: 1}, c.now)
	b.Request()
	// 15 s later this second's slot is a different one from the request's, which nothing has cleared.
	c.add(15 * time.Second)
	if b.Retry() {
		t.Error("a request from 15 s ago paid for a retry")
	}
}

func TestASlotIsReusedOnlyForItsOwnSecond(t *testing.T) {
	c := newClock()
	b := New(config.RetryBudget{Ratio: 1}, c.now)
	b.Request()
	// Exactly one window later the same slot comes round again; what it held must not count twice.
	c.add(Window)
	if b.Retry() {
		t.Error("a request from 10 s ago paid for a retry")
	}
	// A clock that steps back (an NTP correction) must not count the future either.
	b.Request()
	c.add(-3 * time.Second)
	if b.Retry() {
		t.Error("a request from 3 s in the future paid for a retry")
	}
}

func TestZeroTurnsFallbackOff(t *testing.T) {
	b := New(config.RetryBudget{}, nil)
	for range 1000 {
		b.Request()
	}
	if b.Retry() {
		t.Error("a budget of 0 and 0 allowed a retry")
	}
}

func TestConcurrentUseCountsEveryCall(t *testing.T) {
	c := newClock()
	b := New(config.RetryBudget{Ratio: 0.1}, c.now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				b.Request()
				if b.Retry() {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if allowed != 800 {
		t.Errorf("8,000 requests at 0.1 allowed %d retries, want 800", allowed)
	}
}

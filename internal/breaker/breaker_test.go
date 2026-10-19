package breaker

import (
	"sync"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
)

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

func setup() (*Breaker, *clock) {
	c := &clock{t: time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)}
	return New(config.Breaker{Failures: 3, Cooldown: 10 * time.Second}, c.now), c
}

func allow(t *testing.T, b *Breaker) *Ticket {
	t.Helper()
	tk, ok, _ := b.Allow()
	if !ok {
		t.Fatalf("refused while %s", b.State())
	}
	return tk
}

func refused(t *testing.T, b *Breaker, wantRetryAt time.Time) {
	t.Helper()
	if _, ok, at := b.Allow(); ok || !at.Equal(wantRetryAt) {
		t.Fatalf("allowed %v, retry at %v; want refused until %v (state %s)", ok, at, wantRetryAt, b.State())
	}
}

func TestFailuresInARowOpenIt(t *testing.T) {
	b, c := setup()
	for range 2 {
		allow(t, b).Failure()
	}
	allow(t, b).Success() // the count is of failures in a row
	for range 2 {
		allow(t, b).Failure()
	}
	if b.State() != Closed {
		t.Fatalf("%s after 2 + 2 failures with a success between", b.State())
	}
	allow(t, b).Failure()
	if b.State() != Open {
		t.Fatalf("%s after 3 in a row", b.State())
	}
	refused(t, b, c.now().Add(10*time.Second))
	c.add(9 * time.Second)
	refused(t, b, c.now().Add(time.Second))
}

func TestAfterTheCooldownOneProbeDecides(t *testing.T) {
	b, c := setup()
	for range 3 {
		allow(t, b).Failure()
	}
	c.add(10 * time.Second)
	if b.State() != HalfOpen {
		t.Fatalf("%s at the end of the cooldown", b.State())
	}
	probe := allow(t, b)
	refused(t, b, c.now()) // one probe at a time; the next may come as soon as it is back
	probe.Failure()
	if b.State() != Open {
		t.Fatalf("a failed probe left it %s", b.State())
	}
	refused(t, b, c.now().Add(10*time.Second)) // a whole new cooldown
	c.add(10 * time.Second)
	allow(t, b).Success()
	if b.State() != Closed {
		t.Fatalf("a probe's success left it %s", b.State())
	}
	// Closed again, with the count of failures started over.
	for range 2 {
		allow(t, b).Failure()
	}
	if b.State() != Closed {
		t.Errorf("%s after 2 failures", b.State())
	}
}

func TestAReleasedProbeLetsTheNextRequestProbe(t *testing.T) {
	b, c := setup()
	for range 3 {
		allow(t, b).Failure()
	}
	c.add(10 * time.Second)
	allow(t, b).Release() // say the client left: nothing learnt
	if b.State() != HalfOpen {
		t.Fatalf("%s after a released probe", b.State())
	}
	allow(t, b).Success()
	if b.State() != Closed {
		t.Errorf("%s", b.State())
	}
}

func TestAResultCountsOnlyInTheStateItsAttemptWasAllowedIn(t *testing.T) {
	b, c := setup()
	early := []*Ticket{allow(t, b), allow(t, b), allow(t, b), allow(t, b)}
	for range 3 {
		allow(t, b).Failure()
	}
	// Allowed while closed, ending after it opened: a late success doesn't close it, a late failure doesn't
	// restart the cooldown.
	early[0].Success()
	c.add(5 * time.Second)
	early[1].Failure()
	if b.State() != Open {
		t.Fatalf("%s", b.State())
	}
	refused(t, b, c.now().Add(5*time.Second))
	// Half-open: a late success from the closed state doesn't close it, and doesn't free the probe's place.
	c.add(5 * time.Second)
	probe := allow(t, b)
	early[2].Success()
	if b.State() != HalfOpen {
		t.Fatalf("a closed-state success during the probe: %s", b.State())
	}
	refused(t, b, c.now())
	probe.Success()
	// Closed again: a result from before both changes counts for nothing either.
	early[3].Failure()
	for range 2 {
		allow(t, b).Failure()
	}
	if b.State() != Closed {
		t.Errorf("an old failure was counted: %s", b.State())
	}
}

func TestATicketReportsOnce(t *testing.T) {
	b, c := setup()
	for range 3 {
		allow(t, b).Failure()
	}
	c.add(10 * time.Second)
	probe := allow(t, b)
	probe.Success()
	probe.Failure() // ignored: a deferred report after the real one
	probe.Release()
	if b.State() != Closed {
		t.Errorf("%s", b.State())
	}
	tk := allow(t, b)
	tk.Failure()
	tk.Failure()
	tk.Failure()
	if b.State() != Closed {
		t.Errorf("one ticket counted three failures: %s", b.State())
	}
}

func TestZeroFailuresMeansNoBreaker(t *testing.T) {
	b := New(config.Breaker{Failures: 0, Cooldown: time.Second}, nil)
	for range 100 {
		allow(t, b).Failure()
	}
	if b.State() != Closed {
		t.Errorf("%s", b.State())
	}
}

func TestConcurrentFailuresOpenItOnce(t *testing.T) {
	b, c := setup()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if tk, ok, _ := b.Allow(); ok {
					tk.Failure()
				}
			}
		}()
	}
	wg.Wait()
	if b.State() != Open {
		t.Fatalf("%s", b.State())
	}
	// It opened once, at the third failure; the refusals since didn't move the end of its cooldown.
	refused(t, b, c.now().Add(10*time.Second))
}

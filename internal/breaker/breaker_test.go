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

func TestAnAnsweredProbeClosesAtOnceAndALaterFailureCountsThen(t *testing.T) {
	b, c := setup()
	for range 3 {
		allow(t, b).Failure()
	}
	c.add(10 * time.Second)
	probe := allow(t, b)
	probe.Answered() // a stream's first event: the breaker closes now, not when the stream ends
	if b.State() != Closed {
		t.Fatalf("%s after the probe answered", b.State())
	}
	probe.Failure() // the stream broke: one failure in a row, seen now
	allow(t, b).Failure()
	if b.State() != Closed {
		t.Fatalf("%s after 2 failures", b.State())
	}
	allow(t, b).Failure()
	if b.State() != Open {
		t.Fatalf("%s after 3: the late failure wasn't counted", b.State())
	}

	// A late failure while open doesn't restart the cooldown, and while half-open the new probe decides.
	c.add(10 * time.Second)
	p2 := allow(t, b)
	p2.Answered()
	for range 3 {
		allow(t, b).Failure() // open again
	}
	c.add(5 * time.Second)
	p2.Failure()
	refused(t, b, c.now().Add(5*time.Second))
	c.add(5 * time.Second)
	p3 := allow(t, b)
	p3.Answered()
	p3.Success() // the stream ended well: nothing more
	if b.State() != Closed {
		t.Errorf("%s", b.State())
	}
}

func TestAnsweredWaitsForTheEndUnlessItIsAProbe(t *testing.T) {
	// A closed breaker's ticket reports when its attempt ends: a stream that started and then broke is a failure in
	// a row like any other, so three of them open the breaker (reporting each start as a success reset the count).
	b, _ := setup()
	for range 3 {
		tk := allow(t, b)
		tk.Answered()
		if b.State() != Closed {
			t.Fatalf("%s", b.State())
		}
		tk.Failure()
	}
	if b.State() != Open {
		t.Errorf("%s after three streams that broke", b.State())
	}
	// Breakers off: Answered and Failure do nothing.
	off := New(config.Breaker{}, nil)
	for range 10 {
		tk, _, _ := off.Allow()
		tk.Answered()
		tk.Failure()
	}
	if off.State() != Closed {
		t.Errorf("%s", off.State())
	}
}

func TestAProbesLateFailureWhileHalfOpenIsIgnored(t *testing.T) {
	// failures 1. Probe a answers (closed), the breaker opens again, its cooldown passes, and probe b is out when
	// a's stream breaks: a's failure must not open the breaker under b (b decides), so b's success closes it.
	c := &clock{t: time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC)}
	b := New(config.Breaker{Failures: 1, Cooldown: 10 * time.Second}, c.now)
	allow(t, b).Failure()
	c.add(10 * time.Second)
	a := allow(t, b)
	a.Answered()
	allow(t, b).Failure() // open again
	c.add(10 * time.Second)
	pb := allow(t, b)
	a.Failure()
	if b.State() != HalfOpen {
		t.Fatalf("%s: a late failure acted while half-open", b.State())
	}
	pb.Success()
	if b.State() != Closed {
		t.Errorf("%s", b.State())
	}
}

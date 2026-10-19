// Package breaker is a circuit breaker for one provider. While a provider keeps failing, sending it more requests
// only makes each of them wait for its failure (often a timeout) before falling back; the breaker notices the
// failures and skips the provider for a while instead, then lets one request through to see whether it is back.
//
// Closed: every attempt is allowed; Failures failed attempts in a row open the breaker. Open: no attempt is allowed
// until Cooldown has passed; then the breaker is half-open. Half-open: one attempt at a time is let through as a
// probe; its success closes the breaker, its failure opens it for another Cooldown. A result counts only in the
// state its attempt was allowed in: an attempt allowed while closed that fails after the breaker opened can't open it
// again, and one that succeeds can't close it.
package breaker

import (
	"sync"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
)

// State is a breaker's state.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	return [...]string{"closed", "open", "half-open"}[s]
}

// Breaker is one provider's breaker. It is safe for concurrent use.
type Breaker struct {
	failures int // in a row, to open it; 0 means the breaker never opens
	cooldown time.Duration
	now      func() time.Time

	mu         sync.Mutex
	state      State
	inARow     int       // failures in a row while closed
	until      time.Time // while open: when it becomes half-open
	probing    bool      // while half-open: a probe is out
	generation uint64    // changes with every change of state
}

// New makes a closed breaker with c's settings; now is its clock (time.Now if nil).
func New(c config.Breaker, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{failures: c.Failures, cooldown: c.Cooldown, now: now}
}

// Ticket is an allowed attempt. Report its result once, with Success, Failure or Release; later reports are ignored,
// so a deferred Release after one of the others is harmless.
type Ticket struct {
	b          *Breaker
	generation uint64
	probe      bool
	done       bool
	late       bool // a probe that reported its success early (Answered): a later Failure is a failure seen then
}

// Allow asks whether an attempt may go to the provider now. If not, it returns when one may (the end of the
// cooldown, or now if a probe is out and its result is awaited).
func (b *Breaker) Allow() (*Ticket, bool, time.Time) {
	if b.failures == 0 {
		return &Ticket{}, true, time.Time{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	switch b.state {
	case Open:
		if now.Before(b.until) {
			return nil, false, b.until
		}
		b.set(HalfOpen)
		fallthrough
	case HalfOpen:
		if b.probing {
			return nil, false, now
		}
		b.probing = true
		return &Ticket{b: b, generation: b.generation, probe: true}, true, time.Time{}
	default:
		return &Ticket{b: b, generation: b.generation}, true, time.Time{}
	}
}

// State returns the breaker's state now (an open breaker whose cooldown is over reads as half-open).
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && !b.now().Before(b.until) {
		return HalfOpen
	}
	return b.state
}

// Success reports that the provider answered.
func (t *Ticket) Success() {
	if t != nil {
		t.late = false
	}
	t.report(true, false)
}

// Failure reports that the provider failed: it answered with an error about itself, or didn't answer. After
// Answered, it is a failure seen now (lateFailure).
func (t *Ticket) Failure() {
	if t != nil && t.late {
		t.late = false
		t.b.lateFailure()
		return
	}
	t.report(false, false)
}

// Release reports an attempt that says nothing about the provider's health (the client left, the gateway is
// stopping, the provider doesn't know the upstream model). A probe released lets the next request probe.
func (t *Ticket) Release() {
	if t != nil {
		t.late = false
	}
	t.report(false, true)
}

// Answered reports that the provider has begun to answer: a stream's first event. A probe reports its success at
// once, so a half-open breaker doesn't wait for the whole stream to learn that its provider is back (a half-open
// breaker held by a streamed probe skipped a healthy provider for as long as one client read slowly; found in the
// day-2 review). Any other attempt reports when it ends, as a plain one does, so a stream that breaks later still
// counts as a failure in a row. (Reporting every stream's start as a success reset the count each time, and a
// provider that broke every stream after its first chunk never tripped its breaker; found in the second review.)
func (t *Ticket) Answered() {
	if t == nil || t.b == nil || t.done || !t.probe {
		return
	}
	t.report(true, false)
	t.late = true
}

// lateFailure counts the failure of a probe that had reported its success early: its stream broke after the
// breaker had closed on it. It counts in the breaker's state now: one more failure in a row while closed, nothing
// while open (the provider is skipped already) or half-open (a new probe decides that state).
func (b *Breaker) lateFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Closed {
		if b.inARow++; b.inARow >= b.failures {
			b.trip()
		}
	}
}

func (t *Ticket) report(ok, neutral bool) {
	if t == nil || t.b == nil || t.done {
		return
	}
	t.done = true
	b := t.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.generation != b.generation {
		return // allowed in an earlier state
	}
	if t.probe {
		b.probing = false
		switch {
		case neutral:
		case ok:
			b.set(Closed)
		default:
			b.trip()
		}
		return
	}
	switch {
	case neutral:
	case ok:
		b.inARow = 0
	default:
		if b.inARow++; b.inARow >= b.failures {
			b.trip()
		}
	}
}

func (b *Breaker) trip() {
	b.set(Open)
	b.until = b.now().Add(b.cooldown)
}

func (b *Breaker) set(s State) {
	b.state, b.inARow, b.probing = s, 0, false
	b.generation++
}

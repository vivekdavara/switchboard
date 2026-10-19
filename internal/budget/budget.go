// Package budget is the gateway's retry budget: a cap on the attempts it makes after a request's first, as a share
// of the requests it receives. When a provider fails, every request falls back to the others, just when they are
// busiest; a fixed number of retries per request would let one outage multiply the load on the rest (a retry storm).
// The budget lets fallback absorb a provider's occasional failures, and refuses retries once they are a larger share
// of the traffic than the ratio allows.
package budget

import (
	"sync"
	"time"

	"github.com/vivekdavara/switchboard/internal/config"
)

// Window is how far back the budget counts. It is divided into one-second slots.
const Window = 10 * time.Second

const slots = int(Window / time.Second)

// Budget counts requests and retries over the last Window. It is safe for concurrent use.
type Budget struct {
	ratio float64 // retries allowed per request
	floor float64 // retries allowed per Window whatever the traffic: MinPerSecond × 10
	now   func() time.Time

	mu    sync.Mutex
	slots [slots]slot
}

// slot counts one second.
type slot struct {
	second            int64 // the Unix second it counts; a slot holding an older second is stale
	requests, retries int
}

// New makes a budget with c's settings; now is its clock (time.Now if nil).
func New(c config.RetryBudget, now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	return &Budget{ratio: c.Ratio, floor: c.MinPerSecond * Window.Seconds(), now: now}
}

// Request records a request's first attempt, which is never refused.
func (b *Budget) Request() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.current().requests++
}

// Retry reports whether one more attempt may be made now and, if it may, counts it. In any Window the gateway makes
// at most ratio × requests + floor retries.
func (b *Budget) Retry() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.current()
	requests, retries := b.totals()
	if float64(retries+1) > b.ratio*float64(requests)+b.floor {
		return false
	}
	s.retries++
	return true
}

// current returns the slot of this second, cleared if it last counted an older one.
func (b *Budget) current() *slot {
	sec := b.now().Unix()
	s := &b.slots[int(((sec%int64(slots))+int64(slots))%int64(slots))]
	if s.second != sec {
		*s = slot{second: sec}
	}
	return s
}

// totals adds up the slots of the last Window, this second included.
func (b *Budget) totals() (requests, retries int) {
	now := b.now().Unix()
	for _, s := range b.slots {
		if now-s.second < int64(slots) && s.second <= now {
			requests += s.requests
			retries += s.retries
		}
	}
	return requests, retries
}

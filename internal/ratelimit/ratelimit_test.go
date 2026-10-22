package ratelimit

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vivekdavara/switchboard/internal/config"
	"github.com/vivekdavara/switchboard/internal/redistest"
)

// clock is a hand-moved clock in microseconds.
type clock struct{ us atomic.Int64 }

func (c *clock) now() int64               { return c.us.Load() }
func (c *clock) advance(d time.Duration)  { c.us.Add(d.Microseconds()) }
func (c *clock) back(d time.Duration)     { c.us.Add(-d.Microseconds()) }
func newClock(start time.Duration) *clock { c := &clock{}; c.us.Store(start.Microseconds()); return c }

func limiter(t *testing.T) (*Limiter, *clock, *redis.Client) {
	rdb, prefix := redistest.Client(t)
	l := New(rdb, prefix)
	c := newClock(500_000 * time.Hour) // 57 years: about where Redis's own clock is
	l.Now = c.now
	return l, c, rdb
}

func take(t *testing.T, l *Limiter, client string, limit config.RateLimit) Decision {
	t.Helper()
	d, err := l.Take(context.Background(), client, limit)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAFreshBucketAllowsItsBurstThenSaysWhenToRetry(t *testing.T) {
	l, _, _ := limiter(t)
	limit := config.RateLimit{PerMinute: 60, Burst: 3} // one a second
	for i := 2; i >= 0; i-- {
		d := take(t, l, "c", limit)
		if !d.Allowed || d.Remaining != i || d.Limit != 3 {
			t.Fatalf("request %d: %+v", 3-i, d)
		}
	}
	d := take(t, l, "c", limit)
	if d.Allowed || d.Remaining != 0 || d.RetryAfter != time.Second || d.Reset != 3*time.Second {
		t.Errorf("the fourth: %+v", d)
	}
}

func TestTheBucketRefillsAtItsRateAndNoFurther(t *testing.T) {
	l, c, _ := limiter(t)
	limit := config.RateLimit{PerMinute: 120, Burst: 2} // one per 500 ms
	take(t, l, "c", limit)
	take(t, l, "c", limit)
	c.advance(250 * time.Millisecond)
	if d := take(t, l, "c", limit); d.Allowed || d.RetryAfter != 250*time.Millisecond {
		t.Errorf("half a request back: %+v", d)
	}
	c.advance(250 * time.Millisecond) // a refusal takes nothing, so the half kept counting
	if d := take(t, l, "c", limit); !d.Allowed || d.Remaining != 0 {
		t.Errorf("one request back: %+v", d)
	}
	c.advance(time.Hour)
	for i, want := range []bool{true, true, false} {
		if d := take(t, l, "c", limit); d.Allowed != want {
			t.Errorf("after an hour, request %d: %+v; the bucket holds at most its burst", i+1, d)
		}
	}
}

func TestAFractionalRateAccumulates(t *testing.T) {
	l, c, _ := limiter(t)
	limit := config.RateLimit{PerMinute: 0.5, Burst: 1} // one every two minutes
	take(t, l, "c", limit)
	for i := 0; i < 119; i++ {
		c.advance(time.Second)
		if d := take(t, l, "c", limit); d.Allowed {
			t.Fatalf("allowed after %d s", i+1)
		}
	}
	c.advance(time.Second)
	if d := take(t, l, "c", limit); !d.Allowed {
		t.Errorf("after 120 s: %+v", d)
	}
}

func TestAClockGoingBackRefillsNothingAndCountsNothingTwice(t *testing.T) {
	l, c, _ := limiter(t)
	limit := config.RateLimit{PerMinute: 60, Burst: 1}
	take(t, l, "c", limit)
	c.back(10 * time.Second)
	if d := take(t, l, "c", limit); d.Allowed || d.RetryAfter != 11*time.Second {
		t.Errorf("a clock 10 s behind waits 10 s longer: %+v", d)
	}
	// Back where it was, then half a second on: the 10 s it spent behind refilled nothing, and only the half second
	// since the request counts.
	c.advance(10*time.Second + 500*time.Millisecond)
	if d := take(t, l, "c", limit); d.Allowed || d.RetryAfter != 500*time.Millisecond {
		t.Errorf("half a second after the bucket's time: %+v", d)
	}
}

func TestClientsHaveTheirOwnBuckets(t *testing.T) {
	l, _, _ := limiter(t)
	limit := config.RateLimit{PerMinute: 1, Burst: 1}
	if !take(t, l, "a", limit).Allowed || take(t, l, "a", limit).Allowed {
		t.Fatal("a's bucket")
	}
	if !take(t, l, "b", limit).Allowed {
		t.Error("b's bucket is b's own")
	}
}

func TestABucketExpiresWhenItWouldBeFull(t *testing.T) {
	l, _, rdb := limiter(t)
	limit := config.RateLimit{PerMinute: 60, Burst: 10}
	take(t, l, "c", limit) // one second from full
	ttl, err := rdb.PTTL(context.Background(), l.Key("c")).Result()
	if err != nil || ttl <= 900*time.Millisecond || ttl > time.Second {
		t.Errorf("PTTL %s %v: the time to full, 1 s", ttl, err)
	}
	for range 9 {
		take(t, l, "c", limit)
	}
	if ttl, _ = rdb.PTTL(context.Background(), l.Key("c")).Result(); ttl <= 9900*time.Millisecond || ttl > 10*time.Second {
		t.Errorf("empty: PTTL %s, want 10 s", ttl)
	}
	if d := take(t, l, "c", limit); d.Allowed || d.Reset != 10*time.Second {
		t.Errorf("refused: %+v", d)
	}
	if ttl, _ = rdb.PTTL(context.Background(), l.Key("c")).Result(); ttl > 10*time.Second || ttl < 9900*time.Millisecond {
		t.Errorf("a refusal changes nothing: PTTL %s", ttl)
	}
}

func TestNoTwoRequestsTakeTheSameToken(t *testing.T) {
	l, _, _ := limiter(t)
	limit := config.RateLimit{PerMinute: 0.001, Burst: 50} // no refill to speak of
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 10 {
				d, err := l.Take(context.Background(), "c", limit)
				if err != nil {
					t.Error(err)
					return
				}
				if d.Allowed {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if allowed.Load() != 50 {
		t.Errorf("200 requests at once against a burst of 50: %d allowed", allowed.Load())
	}
}

func TestRedisOwnClockIsTheDefault(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	l := New(rdb, prefix)                             // no Now: Redis's TIME
	limit := config.RateLimit{PerMinute: 6, Burst: 2} // one per 10 s
	for i, want := range []bool{true, true, false} {
		d, err := l.Take(context.Background(), "c", limit)
		if err != nil || d.Allowed != want {
			t.Fatalf("request %d: %+v %v", i+1, d, err)
		}
		if !want && (d.RetryAfter < 9*time.Second || d.RetryAfter > 10*time.Second) {
			t.Errorf("retry after %s, want just under 10 s", d.RetryAfter)
		}
	}
}

func TestARedisThatIsDownOrSlowIsAnError(t *testing.T) {
	refused := redistest.New(closedPort(t))
	defer refused.Close()
	if _, err := New(refused, "x:").Take(context.Background(), "c", config.RateLimit{PerMinute: 1, Burst: 1}); err == nil {
		t.Error("a refused connection")
	}

	silent, err := net.Listen("tcp", "127.0.0.1:0") // accepts, never answers
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	slow := redis.NewClient(&redis.Options{Addr: silent.Addr().String(), MaxRetries: -1, ReadTimeout: 50 * time.Millisecond,
		DisableIdentity: true})
	defer slow.Close()
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := New(slow, "x:").Take(ctx, "c", config.RateLimit{PerMinute: 1, Burst: 1}); err == nil {
		t.Error("a Redis that never answers")
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("took %s: the read timeout is 50 ms", took)
	}
}

// closedPort returns an address nothing listens on.
func closedPort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// A bucket that has been full for a while holds its burst, no more: the time it sat full is no credit.
func TestAFullBucketGivesNoCreditForTheTimeItSatFull(t *testing.T) {
	l, c, _ := limiter(t)
	limit := config.RateLimit{PerMinute: 60, Burst: 1} // one a second
	take(t, l, "c", limit)
	c.advance(1500 * time.Millisecond) // full again for the last half second
	if d := take(t, l, "c", limit); !d.Allowed {
		t.Fatalf("a full bucket: %+v", d)
	}
	c.advance(500 * time.Millisecond)
	if d := take(t, l, "c", limit); d.Allowed || d.RetryAfter != 500*time.Millisecond {
		t.Errorf("half a second after the last request: %+v; the half second it sat full before counts for nothing", d)
	}
}

// The fastest rate the configuration allows, 1,000,000 a minute, is a token every 60 µs: a full bucket's expiry is
// then under a millisecond, which the script rounds up (PX 0 is an error to Redis, so rounding down made every check
// of such a client fail as "unavailable"; a surviving mutant of the day-5 review).
func TestTheFastestRateAllowedWorks(t *testing.T) {
	l, c, rdb := limiter(t)
	limit := config.RateLimit{PerMinute: config.MaxRequestsPerMinute, Burst: 1}
	if d := take(t, l, "c", limit); !d.Allowed || d.Reset != 60*time.Microsecond {
		t.Fatalf("the first: %+v", d)
	}
	if ttl, err := rdb.PTTL(context.Background(), l.Key("c")).Result(); err != nil || ttl <= 0 || ttl > time.Millisecond {
		t.Errorf("the bucket's expiry: %v %v", ttl, err)
	}
	if d := take(t, l, "c", limit); d.Allowed || d.RetryAfter != 60*time.Microsecond {
		t.Errorf("the second at once: %+v", d)
	}
	c.advance(60 * time.Microsecond)
	if d := take(t, l, "c", limit); !d.Allowed {
		t.Errorf("60 µs later: %+v", d)
	}
}

// Package ratelimit keeps a token bucket per client in Redis (DESIGN.md, "Rate limits"). A bucket holds up to Burst
// requests and refills at PerMinute a minute; a request takes one, and a request that finds less than one is refused,
// told how long until one is back. The bucket lives in Redis, so every gateway that names the same Redis shares it,
// and one Lua script reads and updates it in one step, so two gateways can't both take the last request.
package ratelimit

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vivekdavara/switchboard/internal/config"
)

// takeScript is the bucket, in one atomic step, kept as GCRA keeps it (the generic cell rate algorithm, the token
// bucket's twin): instead of the tokens left and when they were last counted, one number, the time at which the bucket
// will be full again (the "theoretical arrival time", tat). Each request allowed pushes tat one interval (60 s / the
// rate) later; a request is allowed if, pushed, tat is at most burst intervals from now. A bucket that holds tokens as
// a fraction drifts: refilled a second at a time, 120 refills of 1/120 of a token added up to less than one (a test
// caught it). Here every value is a whole number of microseconds, which a Lua number holds exactly for 285 years.
//
// KEYS[1] is the bucket. ARGV: the interval and the burst's span (interval × burst), in microseconds, and now in
// microseconds, or "" for Redis's own clock (TIME), which every gateway shares whatever its own clock says. A clock
// behind tat just sees a longer wait, and time is never counted twice: the state is a time, not a count of refills.
// The key expires when the bucket would be full, which is what a missing key means. It returns whether the request
// may go, whole requests left, microseconds until one is back (when refused), and microseconds until the bucket is
// full.
var takeScript = redis.NewScript(`
local interval = tonumber(ARGV[1])
local span = tonumber(ARGV[2])
local now
if ARGV[3] == '' then
	local t = redis.call('TIME')
	now = tonumber(t[1]) * 1000000 + tonumber(t[2])
else
	now = tonumber(ARGV[3])
end
local tat = tonumber(redis.call('GET', KEYS[1]) or now)
if tat < now then
	tat = now
end
local pushed = tat + interval
if pushed - now > span then
	return {0, 0, pushed - now - span, tat - now}
end
redis.call('SET', KEYS[1], pushed, 'PX', math.ceil((pushed - now) / 1000))
return {1, math.floor((span - (pushed - now)) / interval), 0, pushed - now}
`)

// Limiter takes requests from clients' buckets.
type Limiter struct {
	rdb    redis.Scripter
	prefix string
	// Now, if set, is the clock in microseconds, for tests; nil means Redis's own clock.
	Now func() int64
}

// New makes a limiter keeping its buckets in rdb, under prefix.
func New(rdb redis.Scripter, prefix string) *Limiter {
	return &Limiter{rdb: rdb, prefix: prefix}
}

// Decision is what a bucket said about one request.
type Decision struct {
	Allowed bool
	// Limit is the bucket's capacity, Remaining the whole requests left in it after this one.
	Limit, Remaining int
	// RetryAfter, for a refused request, is how long until the bucket holds one request again.
	RetryAfter time.Duration
	// Reset is how long until the bucket is full again.
	Reset time.Duration
}

// Key is the Redis key of a client's bucket.
func (l *Limiter) Key(client string) string { return l.prefix + "ratelimit:" + client }

// Interval is the time one request's token takes to come back: 60 s divided by the rate, to the microsecond (at most
// a microsecond off per request: 7 a minute is 8,571,429 µs, 3 µs a minute slow).
func Interval(limit config.RateLimit) time.Duration {
	return time.Duration(math.Round(60e6/limit.PerMinute)) * time.Microsecond
}

// Take takes one request from the client's bucket. An error means Redis couldn't say (down, slow, or refusing the
// script); what then happens is the caller's choice (config.Redis.RefuseWhenDown).
func (l *Limiter) Take(ctx context.Context, client string, limit config.RateLimit) (Decision, error) {
	now := ""
	if l.Now != nil {
		now = strconv.FormatInt(l.Now(), 10)
	}
	interval := Interval(limit).Microseconds()
	res, err := takeScript.Run(ctx, l.rdb, []string{l.Key(client)}, interval, interval*int64(limit.Burst),
		now).Int64Slice()
	if err != nil {
		return Decision{}, err
	}
	if len(res) != 4 {
		return Decision{}, fmt.Errorf("the rate limit script returned %d values, not 4", len(res))
	}
	return Decision{
		Allowed:    res[0] == 1,
		Limit:      limit.Burst,
		Remaining:  int(min(max(res[1], 0), math.MaxInt32)),
		RetryAfter: time.Duration(res[2]) * time.Microsecond,
		Reset:      time.Duration(res[3]) * time.Microsecond,
	}, nil
}

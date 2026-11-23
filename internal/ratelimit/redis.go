// Package ratelimit implements a distributed rate limiter on Redis.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// slidingWindow is a sliding-window log: a sorted set holds one entry per
// send from the last window, scored by time. A new send is allowed only if
// fewer than limit entries remain after dropping old ones. This guarantees
// at most `limit` sends in ANY window-long span. A fixed per-second counter
// would not: it allows 100 at 0.99s and another 100 at 1.01s.
//
// It runs as a Lua script, so the check-and-add is atomic even when many
// worker processes call it at once. It uses Redis's own clock, so workers
// with slightly different clocks still agree.
//
// Time is kept in microseconds. An earlier version rounded to milliseconds,
// so a slot granted at 12.9ms was stored as 12ms and freed up to 1ms early.
//
// The guarantee holds where the decision is made, in Redis (verified with
// redis-cli MONITOR: never more than 100 grants in any 1s span). The moment a
// request actually leaves the worker is a few ms later, and that delay
// varies, so a receiver can see a few more than `limit` in some 1s spans
// during a burst. If a provider enforces its limit strictly, configure a
// little headroom (e.g. 90/s for a 100/s provider limit).
//
// Returns 0 if the send is allowed, otherwise how many microseconds to wait.
var slidingWindow = redis.NewScript(`
local key    = KEYS[1]
local limit  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local member = ARGV[3]

-- TIME returns {seconds, microseconds}. Microseconds since the epoch are
-- about 1.8e15, well inside the 2^53 range a Lua number holds exactly.
local t   = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])

redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)
if redis.call('ZCARD', key) < limit then
  redis.call('ZADD', key, now, member)
  redis.call('PEXPIRE', key, math.ceil(window / 1000))
  return 0
end

local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
local wait = tonumber(oldest[2]) + window - now
if wait < 1 then wait = 1 end
return wait
`)

type Limiter struct {
	rdb    *redis.Client
	limit  int
	window time.Duration
}

// New allows `limit` events per `window` for each key.
func New(rdb *redis.Client, limit int, window time.Duration) *Limiter {
	return &Limiter{rdb: rdb, limit: limit, window: window}
}

// Wait blocks until an event for key is allowed, or ctx is cancelled.
func (l *Limiter) Wait(ctx context.Context, key string) error {
	redisKey := "ratelimit:" + key
	for {
		waitUs, err := slidingWindow.Run(ctx, l.rdb, []string{redisKey},
			l.limit, l.window.Microseconds(), uuid.NewString()).Int64()
		if err != nil {
			return fmt.Errorf("rate limiter: %w", err)
		}
		if waitUs == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(waitUs) * time.Microsecond):
		}
	}
}

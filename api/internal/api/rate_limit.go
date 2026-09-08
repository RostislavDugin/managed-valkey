package api

import (
	"math"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/auth"
)

const (
	AuthRateLimit  = 10
	AuthRateWindow = 60 * time.Second
)

type RateLimiter struct {
	mu      sync.Mutex
	clock   auth.Clock
	limit   int
	window  time.Duration
	entries map[string][]time.Time
}

func NewRateLimiter(clock auth.Clock, limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{clock: clock, limit: limit, window: window, entries: make(map[string][]time.Time)}
}

func (l *RateLimiter) Allow(address string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock.Now()
	cutoff := now.Add(-l.window)
	for key, values := range l.entries {
		kept := discardExpired(values, cutoff)
		if len(kept) == 0 {
			delete(l.entries, key)
		} else {
			l.entries[key] = kept
		}
	}

	values := l.entries[address]
	if len(values) >= l.limit {
		seconds := max(int(math.Ceil(values[0].Add(l.window).Sub(now).Seconds())), 1)

		return false, seconds
	}

	l.entries[address] = append(values, now)

	return true, 0
}

func discardExpired(values []time.Time, cutoff time.Time) []time.Time {
	first := 0
	for first < len(values) && !values[first].After(cutoff) {
		first++
	}

	return values[first:]
}

func RateLimit(limiter *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		address := c.Request.RemoteAddr
		host, _, err := net.SplitHostPort(address)
		if err == nil {
			address = host
		}

		allowed, retryAfter := limiter.Allow(address)
		if !allowed {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			apierr.Write(
				c,
				apierr.New(apierr.CodeRateLimited, "Слишком много запросов", map[string]any{"retry_after": retryAfter}),
			)

			return
		}

		c.Next()
	}
}

package httpapi

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const rateLimitSweepInterval = time.Minute

// RateLimit configures a token bucket per authenticated client. Requests
// that fail authentication are limited per remote address instead, using the
// same rate. A zero RequestsPerSecond disables rate limiting.
type RateLimit struct {
	RequestsPerSecond float64
	Burst             int
}

// rateLimiter keeps one token bucket per key. Each bucket holds up to burst
// tokens and refills at rate tokens per second; a request takes one token.
type rateLimiter struct {
	rate, burst float64
	now         func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(cfg RateLimit, now func() time.Time) *rateLimiter {
	if cfg.RequestsPerSecond <= 0 {
		return nil
	}
	return &rateLimiter{
		rate:    cfg.RequestsPerSecond,
		burst:   math.Max(1, float64(cfg.Burst)),
		now:     now,
		buckets: map[string]*bucket{},
	}
}

// allow takes a token from key's bucket. If the bucket is empty it returns
// false and how long the caller should wait before retrying. A nil limiter
// allows everything.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// sweep forgets buckets that have refilled completely. A full bucket behaves
// exactly like a new one, so this bounds memory without changing any limit.
func (l *rateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < rateLimitSweepInterval {
		return
	}
	l.lastSweep = now
	refill := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) >= refill {
			delete(l.buckets, k)
		}
	}
}

// limit applies the rate limit for key and writes a 429 response if it is
// exceeded.
func (l *rateLimiter) limit(w http.ResponseWriter, key string) bool {
	ok, wait := l.allow(key)
	if ok {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
	writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
	return false
}

// remoteHost is the address of the TCP peer. X-Forwarded-For is not trusted:
// behind a proxy this is the proxy's address.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

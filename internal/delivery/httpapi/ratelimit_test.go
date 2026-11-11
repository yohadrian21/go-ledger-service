package httpapi

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestRateLimiterTokenBucket(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newRateLimiter(RateLimit{RequestsPerSecond: 2, Burst: 3}, clock.now)

	for i := range 3 {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("request %d within the burst was rejected", i+1)
		}
	}
	ok, wait := l.allow("a")
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("allow after the burst = %v, %v; want false, 500ms", ok, wait)
	}
	if ok, _ := l.allow("b"); !ok {
		t.Fatal("buckets must be independent per key")
	}

	clock.advance(500 * time.Millisecond)
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("a token should have been refilled")
	}
	if ok, _ := l.allow("a"); ok {
		t.Fatal("only one token should have been refilled")
	}

	clock.advance(time.Hour)
	for i := range 3 {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("request %d after a long pause was rejected: the bucket should be full", i+1)
		}
	}
	if ok, _ := l.allow("a"); ok {
		t.Fatal("refill must be capped at the burst size")
	}
}

func TestRateLimiterForgetsIdleBuckets(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newRateLimiter(RateLimit{RequestsPerSecond: 1, Burst: 1}, clock.now)
	l.allow("idle")
	clock.advance(2 * rateLimitSweepInterval)
	l.allow("active")
	if _, ok := l.buckets["idle"]; ok || len(l.buckets) != 1 {
		t.Fatalf("idle bucket was not dropped: %d buckets", len(l.buckets))
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	l := newRateLimiter(RateLimit{}, time.Now)
	for range 100 {
		if ok, _ := l.allow("a"); !ok {
			t.Fatal("a zero rate must disable limiting")
		}
	}
}

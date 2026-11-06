// Package ratelimit provides an in-memory, per-key token-bucket rate limiter.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter allows each key `rate` events per second with bursts of up to `burst`.
// It is safe for concurrent use. Idle keys are swept periodically so memory stays bounded.
type Limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	now     func() time.Time
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter. now is injectable for tests; pass nil to use time.Now.
func New(rate float64, burst int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: rate, burst: float64(burst), now: now, buckets: map[string]*bucket{}, swept: now()}
}

// Allow consumes one token for key and reports whether the event is permitted.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Blocked reports whether key currently has no tokens left, without consuming one.
func (l *Limiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refill(key).tokens < 1
}

// refill must be called with l.mu held.
func (l *Limiter) refill(key string) *bucket {
	now := l.now()
	l.sweep(now)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
		return b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	return b
}

// sweep drops keys whose bucket would be full again, at most once a minute.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < time.Minute {
		return
	}
	l.swept = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// Len returns the number of tracked keys (for tests and metrics).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

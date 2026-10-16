// Package ratelimit implements a token bucket: a steady rate with a burst.
// It serves both the per-instance send pacing (Wait) and the per-tenant API
// limit (Allow).
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Bucket holds up to burst tokens, refilled at rate tokens per second. It
// is safe for concurrent use.
type Bucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

// New returns a full bucket. rate must be positive; burst is at least 1.
func New(rate float64, burst int) *Bucket {
	b := float64(max(burst, 1))
	return &Bucket{rate: rate, burst: b, tokens: b, last: time.Now()}
}

// Set changes the rate and burst, keeping the current tokens (capped).
func (b *Bucket) Set(rate float64, burst int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(time.Now())
	b.rate, b.burst = rate, float64(max(burst, 1))
	b.tokens = min(b.tokens, b.burst)
}

// refill adds the tokens earned since the last call; b.mu must be held.
func (b *Bucket) refill(now time.Time) {
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
}

// Allow takes a token if one is available. Otherwise it reports how long
// until one will be.
func (b *Bucket) Allow() (ok bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(time.Now())
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}

// Wait blocks until a token is available or ctx ends.
func (b *Bucket) Wait(ctx context.Context) error {
	for {
		ok, wait := b.Allow()
		if ok {
			return nil
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

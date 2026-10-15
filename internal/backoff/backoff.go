// Package backoff computes retry delays: exponential growth capped at a
// maximum, with jitter. It is shared by reconnects, the outbox and webhook
// delivery, so all retry timing follows one policy.
package backoff

import (
	"math/rand/v2"
	"time"
)

// Policy is an exponential backoff from Min doubling up to Max.
type Policy struct {
	Min time.Duration
	Max time.Duration
}

// Ceiling returns the un-jittered delay for attempt n (n >= 1): Min·2^(n-1)
// capped at Max. It is O(1) and safe against overflow for any n.
func (p Policy) Ceiling(n int) time.Duration {
	if n <= 0 || p.Min <= 0 {
		return 0
	}
	shift := min(n-1, 62)
	if p.Min > p.Max>>shift {
		return p.Max
	}
	return p.Min << shift
}

// Delay returns a jittered delay for attempt n, uniformly distributed in
// [Min/2, Ceiling(n)]. Jitter spreads out many instances that fail at the
// same moment (e.g. a network blip on the host); the Min/2 floor keeps a
// tight retry loop impossible.
func (p Policy) Delay(n int) time.Duration {
	ceiling := p.Ceiling(n)
	if ceiling == 0 {
		return 0
	}
	floor := min(p.Min/2, ceiling)
	return floor + rand.N(ceiling-floor+1)
}

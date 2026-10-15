package client

import (
	"math/rand/v2"
	"time"
)

// Backoff computes reconnect delays: exponential growth capped at Max, with
// "full jitter" (a uniformly random delay in [0, cap]). Full jitter spreads
// out many instances that lost their connection at the same moment (e.g. a
// network blip on the host), so they do not all hit the server at once.
type Backoff struct {
	Min time.Duration
	Max time.Duration
}

// DefaultBackoff is used when Config.Backoff is zero.
var DefaultBackoff = Backoff{Min: time.Second, Max: 5 * time.Minute}

// Delay returns the delay before reconnect attempt n (n >= 1).
func (b Backoff) Delay(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	ceiling := b.Min
	for i := 1; i < n && ceiling < b.Max; i++ {
		ceiling *= 2
	}
	ceiling = min(ceiling, b.Max)
	// Never return less than Min/2, so a tight loop stays impossible.
	floor := b.Min / 2
	return floor + rand.N(ceiling-floor+1)
}

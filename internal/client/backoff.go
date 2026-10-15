package client

import (
	"time"

	"github.com/mdenesfe/wax/internal/backoff"
)

// Backoff computes reconnect delays (exponential, capped, jittered).
type Backoff = backoff.Policy

// DefaultBackoff is used when Config.Backoff is zero.
var DefaultBackoff = Backoff{Min: time.Second, Max: 5 * time.Minute}

package api

import (
	"context"
	"crypto/hmac"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mdenesfe/wax/internal/ratelimit"
)

// DefaultRequestsPerSecond is the API rate of a tenant without its own.
const DefaultRequestsPerSecond = 20

// tenantLimiter holds one token bucket per tenant (bounded by the number
// of tenants). Limits apply per node: with N nodes behind a load balancer a
// tenant may get up to N times its rate.
type tenantLimiter struct {
	defaultRate int
	mu          sync.Mutex
	buckets     map[string]*ratelimit.Bucket
	rates       map[string]int
}

func newTenantLimiter(defaultRate int) *tenantLimiter {
	if defaultRate <= 0 {
		defaultRate = DefaultRequestsPerSecond
	}
	return &tenantLimiter{defaultRate: defaultRate, buckets: map[string]*ratelimit.Bucket{}, rates: map[string]int{}}
}

// allow takes a token for tenant at its current rate (rate <= 0: default);
// the burst is two seconds' worth.
func (l *tenantLimiter) allow(tenant string, rate int) (bool, time.Duration) {
	if rate <= 0 {
		rate = l.defaultRate
	}
	l.mu.Lock()
	b, ok := l.buckets[tenant]
	switch {
	case !ok:
		b = ratelimit.New(float64(rate), 2*rate)
		l.buckets[tenant] = b
	case l.rates[tenant] != rate: // changed through the admin API
		b.Set(float64(rate), 2*rate)
	}
	l.rates[tenant] = rate
	l.mu.Unlock()
	return b.Allow()
}

// tooManyRequests answers 429 with the seconds to wait.
func tooManyRequests(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int((wait+time.Second-1)/time.Second))))
	writeJSON(w, http.StatusTooManyRequests, errorJSON{Error: "rate limit exceeded"})
}

// --- signed forwarding ----------------------------------------------------------

// forwardMaxAge bounds how old a forwarding signature may be.
const forwardMaxAge = 30 * time.Second

type forwardedKey struct{}

// signForward returns the forwarded-header value: node;unix;mac, where the
// MAC (keyed from the master key every node shares) covers node, time,
// method and request URI. Clients cannot forge it, so it can exempt
// forwarded requests from forwarding again and from the rate limit (which
// the receiving node already applied).
func (s *Server) signForward(r *http.Request) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	node := s.m.NodeID()
	return node + ";" + ts + ";" + hex.EncodeToString(s.store.NodeMAC([]byte(node), []byte(ts), []byte(r.Method), []byte(r.URL.RequestURI())))
}

// verifyForward reports whether r carries a valid, fresh forwarding
// signature from another node.
func (s *Server) verifyForward(r *http.Request) bool {
	parts := strings.Split(r.Header.Get(forwardedHeader), ";")
	if len(parts) != 3 {
		return false
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	if age := time.Since(time.Unix(ts, 0)); age > forwardMaxAge || age < -forwardMaxAge {
		return false
	}
	mac, err := hex.DecodeString(parts[2])
	return err == nil && hmac.Equal(mac, s.store.NodeMAC([]byte(parts[0]), []byte(parts[1]), []byte(r.Method), []byte(r.URL.RequestURI())))
}

func isForwarded(r *http.Request) bool {
	v, _ := r.Context().Value(forwardedKey{}).(bool)
	return v
}

func withForwarded(r *http.Request, v bool) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), forwardedKey{}, v))
}

// --- large request fairness ---------------------------------------------------------

// largeSlots limits concurrent large requests per node, and per tenant to
// half of them, so one tenant's uploads cannot starve the others.
type largeSlots struct {
	total, perTenant int
	mu               sync.Mutex
	inUse            int
	byTenant         map[string]int
	free             chan struct{} // signalled when a slot is released
}

func newLargeSlots(total int) *largeSlots {
	return &largeSlots{total: total, perTenant: max(1, total/2), byTenant: map[string]int{}, free: make(chan struct{}, 1)}
}

func (l *largeSlots) tryAcquire(tenant string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inUse >= l.total || l.byTenant[tenant] >= l.perTenant {
		return false
	}
	l.inUse++
	l.byTenant[tenant]++
	return true
}

func (l *largeSlots) release(tenant string) {
	l.mu.Lock()
	l.inUse--
	if l.byTenant[tenant]--; l.byTenant[tenant] == 0 {
		delete(l.byTenant, tenant)
	}
	l.mu.Unlock()
	select {
	case l.free <- struct{}{}:
	default:
	}
}

// acquire waits for a slot for tenant until ctx ends.
func (l *largeSlots) acquire(ctx context.Context, tenant string) error {
	for !l.tryAcquire(tenant) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for an upload slot: %w", ctx.Err())
		case <-l.free:
		case <-time.After(100 * time.Millisecond): // a release may have woken another waiter
		}
	}
	return nil
}

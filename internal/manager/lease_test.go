package manager

import (
	"testing"
	"time"

	"github.com/mdenesfe/wax/internal/store"
)

func TestFairShare(t *testing.T) {
	for _, c := range []struct{ instances, nodes, max, want int }{
		{0, 1, 0, 0}, {4, 1, 0, 4}, {4, 2, 0, 2}, {5, 2, 0, 3}, {1, 3, 0, 1},
		{10, 0, 0, 10}, // no live node recorded yet: this node takes all
		{10, 1, 3, 3},  // capped by -max-instances
	} {
		if got := fairShare(c.instances, c.nodes, c.max); got != c.want {
			t.Errorf("fairShare(%d, %d, %d) = %d, want %d", c.instances, c.nodes, c.max, got, c.want)
		}
	}
}

// TestFenceIfStale: instances keep running while the last renewal is
// recent, and are all stopped once a lease could expire.
func TestFenceIfStale(t *testing.T) {
	m := New(Config{Timing: Timing{LeaseTTL: 900 * time.Millisecond}})
	for _, id := range []string{"t:a", "t:b"} {
		in := newInstance(m, store.Instance{ID: id})
		in.running = true
		m.instances[id] = in
	}
	l := newLeaser(m)
	l.fenceIfStale() // never renewed: nothing to fence yet
	if len(m.RunningIDs()) != 2 {
		t.Fatal("fenced without any renewal")
	}
	l.lastRenew = time.Now().Add(-l.interval())
	l.fenceIfStale()
	if len(m.RunningIDs()) != 2 {
		t.Fatal("fenced although the lease is far from expiring")
	}
	l.lastRenew = time.Now().Add(-(l.ttl - l.interval()))
	l.fenceIfStale()
	if n := len(m.RunningIDs()); n != 0 {
		t.Fatalf("%d instances still running after the fence", n)
	}
}

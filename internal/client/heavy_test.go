package client

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestHeavySlots: no more than the configured number of heavy jobs run at
// once, and waiting ends with the session.
func TestHeavySlots(t *testing.T) {
	slots := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{c: &Client{cfg: Config{HeavySlots: slots}}, ctx: ctx}
	var running, peak atomic.Int32
	done := make(chan struct{})
	for range 6 {
		go func() {
			_, _ = s.withHeavySlot(func() (bool, error) {
				n := running.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				time.Sleep(20 * time.Millisecond)
				running.Add(-1)
				return true, nil
			})
			done <- struct{}{}
		}()
	}
	for range 6 {
		<-done
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency %d, want 2", peak.Load())
	}
	slots <- struct{}{}
	slots <- struct{}{} // all slots taken
	cancel()
	if _, err := s.withHeavySlot(func() (bool, error) { return true, nil }); err == nil {
		t.Fatal("ran without a slot after the session ended")
	}
}

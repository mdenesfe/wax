package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAllowBurstThenRate(t *testing.T) {
	b := New(100, 3) // one token per 10ms
	for i := range 3 {
		if ok, _ := b.Allow(); !ok {
			t.Fatalf("burst token %d refused", i)
		}
	}
	ok, wait := b.Allow()
	if ok || wait <= 0 || wait > 10*time.Millisecond {
		t.Fatalf("after the burst: ok=%v wait=%v", ok, wait)
	}
	time.Sleep(wait + time.Millisecond)
	if ok, _ := b.Allow(); !ok {
		t.Fatal("token not refilled after retryAfter")
	}
}

func TestWait(t *testing.T) {
	b := New(1000, 1)
	ctx := context.Background()
	start := time.Now()
	for range 5 {
		if err := b.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 3*time.Millisecond {
		t.Fatalf("5 tokens at 1000/s took only %v", d)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	b.Set(0.001, 1)
	b.Allow()
	if err := b.Wait(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait on canceled context: %v", err)
	}
}

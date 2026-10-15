package backoff

import (
	"testing"
	"time"
)

func TestCeiling(t *testing.T) {
	p := Policy{Min: time.Second, Max: 8 * time.Second}
	want := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for n, w := range want {
		if got := p.Ceiling(n); got != w {
			t.Errorf("Ceiling(%d) = %v, want %v", n, got, w)
		}
	}
	if got := p.Ceiling(1 << 30); got != p.Max {
		t.Errorf("huge attempt: %v", got)
	}
}

func TestDelayBounds(t *testing.T) {
	p := Policy{Min: time.Second, Max: 8 * time.Second}
	for n := 1; n < 10; n++ {
		for range 100 {
			d := p.Delay(n)
			if d < p.Min/2 || d > p.Ceiling(n) {
				t.Fatalf("Delay(%d) = %v out of bounds", n, d)
			}
		}
	}
	if p.Delay(0) != 0 {
		t.Fatal("Delay(0) must be 0")
	}
}

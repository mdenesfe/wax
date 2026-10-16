package client

import (
	"context"
	"testing"
	"time"

	"github.com/mdenesfe/wax/internal/wabin"
)

func TestNodeQueue(t *testing.T) {
	q := newNodeQueue()
	ctx := context.Background()
	// Push never blocks, even far beyond any channel buffer.
	const n = 5000
	for i := range n {
		q.push(&wabin.Node{Tag: "n", Attrs: wabin.Attrs{"i": i}})
	}
	for i := range n {
		got, ok := q.pop(ctx)
		if !ok || got.Attrs["i"] != i {
			t.Fatalf("pop %d: got %v", i, got)
		}
		if i == n/2 { // interleave pushes with pops (compaction path)
			q.push(&wabin.Node{Tag: "late"})
		}
	}
	if got, ok := q.pop(ctx); !ok || got.Tag != "late" {
		t.Fatalf("late node: %v", got)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, ok := q.pop(cctx); ok {
		t.Fatal("pop on empty queue should end with the context")
	}
}

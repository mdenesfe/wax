package client

import (
	"context"
	"sync"

	"github.com/mdenesfe/wax/internal/wabin"
)

// nodeQueue hands stanzas from the reader to the handler goroutine. Push
// never blocks: if the reader waited for the handler, a handler blocked on
// an IQ would never see the response (only the reader delivers it), and
// the session would stall until the IQ timed out. Growth is bounded in
// practice by the server, which delivers its offline queue in windows and
// waits for acknowledgements, and acks are sent by the handler.
type nodeQueue struct {
	mu    sync.Mutex
	items []*wabin.Node
	head  int
	ready chan struct{} // capacity 1: "items may be non-empty"
}

func newNodeQueue() *nodeQueue { return &nodeQueue{ready: make(chan struct{}, 1)} }

// push appends n in O(1) amortized time.
func (q *nodeQueue) push(n *wabin.Node) {
	q.mu.Lock()
	q.items = append(q.items, n)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// pop returns the oldest node, waiting until one arrives or ctx ends.
func (q *nodeQueue) pop(ctx context.Context) (*wabin.Node, bool) {
	for {
		q.mu.Lock()
		if q.head < len(q.items) {
			n := q.items[q.head]
			q.items[q.head] = nil // release for GC
			q.head++
			switch {
			case q.head == len(q.items): // drained: reuse the backing array
				q.items, q.head = q.items[:0], 0
			case q.head >= 1024 && q.head*2 >= len(q.items):
				// Mostly consumed under sustained load: compact so the
				// consumed prefix does not keep growing the array.
				q.items = q.items[:copy(q.items, q.items[q.head:])]
				q.head = 0
			}
			q.mu.Unlock()
			return n, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-q.ready:
		}
	}
}

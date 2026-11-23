package client

import (
	"fmt"
	"testing"

	"github.com/mdenesfe/wax/internal/wabin"
)

// TestOfflineBatches: a preview of the stanzas queued while we were away is
// answered with a batch request, the next batch is asked for once the
// previous one arrived, and nothing more once the server reports the queue
// empty.
func TestOfflineBatches(t *testing.T) {
	old := offlineBatchSize
	offlineBatchSize = 2
	t.Cleanup(func() { offlineBatchSize = old })
	h, c, _ := loggedIn(t, nil)

	expectBatch := func() {
		t.Helper()
		ib := readNodeWith(t, h, c, "ib", "")
		b, ok := ib.Child("offline_batch")
		if n, _ := b.AttrInt("count"); !ok || n != 2 {
			t.Fatalf("batch request %s", ib.String())
		}
	}
	queued := func(id string) {
		t.Helper()
		must(t, c.Send(h.ctx, wabin.Node{Tag: "notification", Attrs: wabin.Attrs{
			"from": bobPN, "id": id, "type": "picture", "offline": "1", "t": "1700000000"}}))
		readNodeWith(t, h, c, "ack", id)
	}

	must(t, c.Send(h.ctx, wabin.Node{Tag: "ib", Content: []wabin.Node{{Tag: "offline_preview",
		Attrs: wabin.Attrs{"count": 3, "message": 0, "notification": 3, "receipt": 0}}}}))
	expectBatch()
	for i := range 2 {
		queued(fmt.Sprintf("N-%d", i))
	}
	expectBatch()
	queued("N-2")
	must(t, c.Send(h.ctx, wabin.Node{Tag: "ib", Content: []wabin.Node{{Tag: "offline", Attrs: wabin.Attrs{"count": 3}}}}))

	// The queue is empty: a live stanza marked offline asks for nothing.
	queued("N-3")
	queued("N-4")
	must(t, c.Send(h.ctx, wabin.Node{Tag: "notification", Attrs: wabin.Attrs{
		"from": bobPN, "id": "N-5", "type": "picture", "t": "1700000000"}}))
	next, err := c.ReadUntil(h.ctx, func(n *wabin.Node) bool {
		id, _ := n.AttrString("id")
		return n.Tag == "ib" || (n.Tag == "ack" && id == "N-5")
	})
	must(t, err)
	if next.Tag == "ib" {
		t.Fatalf("batch requested after the queue was empty: %s", next.String())
	}
}

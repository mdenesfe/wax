package client

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/wabin"
	"github.com/mdenesfe/wax/proto/wae2e"
)

// BenchmarkReceiveMessage measures the full path of one incoming 1:1
// message over the fake server: decode, decrypt with the session in a
// database transaction, store, emit, and the delivery receipt. It is the
// per-message cost of draining an offline queue.
func BenchmarkReceiveMessage(b *testing.B) {
	h := newHarness(b, nil)
	_, c := h.pair()
	upload := h.serveLoginUpload(c, 0)
	bob := newPeer(b, wabin.JID{User: bobLID.User, Server: wabin.ServerLID}, 4321)
	toUs := &signal.Record{}
	must(b, toUs.ProcessBundle(bob.local(), ourBundle(b, upload)))
	send := func(i int) {
		id := fmt.Sprintf("BENCH-%d", i)
		must(b, c.Send(h.ctx, wabin.Node{Tag: "message", Attrs: wabin.Attrs{
			"from": bob.jid, "id": id, "t": fmt.Sprint(time.Now().Unix()), "type": "text", "notify": "Bob", "sender_pn": bobPN,
		}, Content: []wabin.Node{encNode(b, toUs, &wae2e.Message{Conversation: proto.String("merhaba")})}}))
		waitEvent[Message](h)
		readNodeWith(b, h, c, "receipt", id)
	}
	send(-1) // establish the session (pkmsg) outside the timed loop
	i := 0
	b.ResetTimer()
	for b.Loop() {
		send(i)
		i++
	}
}

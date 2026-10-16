package client

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/wabin"
)

// TestLiveEditAndRevoke: an incoming edit and revoke change the stored
// message they refer to and are not stored as messages of their own.
func TestLiveEditAndRevoke(t *testing.T) {
	h, _, _ := loggedIn(t, nil)
	chat := wabin.JID{User: bobLID.User, Server: wabin.ServerLID}
	device := wabin.JID{User: bobLID.User, Device: 3, Server: wabin.ServerLID}
	at := time.Now().Truncate(time.Second)
	cl := h.client
	cl.storeLiveMessage(h.ctx, chat, device, "E1", false, at, "Bob", &wae2e.Message{Conversation: proto.String("hi")})

	change := func(id string, pm *wae2e.ProtocolMessage) {
		pm.Key = &wae2e.MessageKey{ID: proto.String("E1")}
		cl.storeLiveMessage(h.ctx, chat, device, id, false, at.Add(time.Minute), "Bob", &wae2e.Message{ProtocolMessage: pm})
		if _, err := cl.cfg.Store.GetMessage(h.ctx, h.inst, chat.String(), id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("protocol message %s stored as a message: %v", id, err)
		}
	}
	change("P1", &wae2e.ProtocolMessage{Type: wae2e.ProtocolMessage_MESSAGE_EDIT.Enum(), EditedMessage: &wae2e.Message{Conversation: proto.String("hello")}})
	m, err := cl.cfg.Store.GetMessage(h.ctx, h.inst, chat.String(), "E1")
	must(t, err)
	var got wae2e.Message
	must(t, proto.Unmarshal(m.Content, &got))
	if got.GetConversation() != "hello" || m.EditedAt == nil {
		t.Fatalf("after edit: %q, edited_at %v", got.GetConversation(), m.EditedAt)
	}
	change("P2", &wae2e.ProtocolMessage{Type: wae2e.ProtocolMessage_REVOKE.Enum()})
	if m, err = cl.cfg.Store.GetMessage(h.ctx, h.inst, chat.String(), "E1"); err != nil || m.Type != store.RevokedType {
		t.Fatalf("after revoke: %+v, %v", m, err)
	}
}

// TestHistoryChunkDropped: a chunk that keeps failing is dropped after
// maxHistoryAttempts instead of staying in the queue.
func TestHistoryChunkDropped(t *testing.T) {
	h, _, _ := loggedIn(t, nil)
	s, err := h.client.activeSession()
	must(t, err)
	n, err := proto.Marshal(&wae2e.HistorySyncNotification{InitialHistBootstrapInlinePayload: []byte("not zlib")})
	must(t, err)
	st := h.client.cfg.Store
	must(t, st.EnqueueHistory(h.ctx, h.inst, n))
	for range maxHistoryAttempts {
		if _, err := s.processNextHistory(); err == nil {
			t.Fatal("a broken chunk was processed")
		}
	}
	if left, err := st.HistoryQueueLength(h.ctx, h.inst); err != nil || left != 0 {
		t.Fatalf("%d chunks left after %d attempts (%v)", left, maxHistoryAttempts, err)
	}
}

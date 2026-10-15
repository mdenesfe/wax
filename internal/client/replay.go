package client

import (
	"context"

	wabin "github.com/mdenesfe/wax/internal/binary"
)

// replayBatch bounds how many deferred stanzas are processed per login, so
// a large backlog cannot stall the live stream for long.
const replayBatch = 500

// replayDeferred processes stanzas stored by earlier versions that could
// not handle them, oldest first. It runs on the handler goroutine before
// live stanzas, preserving order. Replayed stanzas were acknowledged when
// they were stored, so no acks are sent again; delivery receipts are.
func (s *session) replayDeferred() {
	items, err := s.c.cfg.Store.ListDeferred(s.ctx, s.c.cfg.InstanceID, replayBatch)
	if err != nil {
		s.log.Warn("could not load deferred stanzas", "error", err)
		return
	}
	if len(items) == 0 {
		return
	}
	done, kept := 0, 0
	for _, it := range items {
		if s.ctx.Err() != nil {
			return
		}
		if len(it.Raw) < 2 {
			_ = s.c.cfg.Store.DeleteDeferred(s.ctx, s.c.cfg.InstanceID, it.ID)
			continue
		}
		n, err := wabin.Unmarshal(it.Raw[1:]) // skip the flags byte
		if err != nil {
			s.log.Warn("dropping undecodable deferred stanza", "id", it.StanzaID, "error", err)
			_ = s.c.cfg.Store.DeleteDeferred(s.ctx, s.c.cfg.InstanceID, it.ID)
			continue
		}
		s.replaying, s.redeferred = true, false
		s.handleSafely(n)
		s.replaying = false
		if s.redeferred {
			kept++ // still unsupported; keep it for a later version
			continue
		}
		if err := s.c.cfg.Store.DeleteDeferred(context.WithoutCancel(s.ctx), s.c.cfg.InstanceID, it.ID); err != nil {
			s.log.Warn("could not delete replayed stanza", "error", err)
		}
		done++
	}
	s.log.Info("replayed deferred stanzas", "processed", done, "still_unsupported", kept)
}

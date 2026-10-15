package client

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/pairing"
	"github.com/mdenesfe/wax/internal/store"
)

// handleNode dispatches a server-initiated node. Unknown tags are logged and
// ignored; they never end the session.
func (s *session) handleNode(n *binary.Node) {
	if d := ClassifyDisconnect(n); d.Action != ActionNone {
		s.log.Warn("server ended the stream", "tag", n.Tag, "action", d.Action.String(), "code", d.Code, "detail", d.Detail)
		s.end(sessionResult{action: d.Action, code: d.Code, detail: d.Detail,
			err: fmt.Errorf("%s code=%s %s", n.Tag, d.Code, d.Detail)})
		return
	}
	switch n.Tag {
	case "xmlstreamend":
		s.end(sessionResult{action: ActionReconnect, err: errors.New("server closed the stream")})
	case "success":
		s.handleSuccess(n)
	case "iq":
		s.handleIQ(n)
	case "notification":
		s.handleNotification(n)
	case "ib":
		s.handleIB(n)
	case "message":
		s.handleMessage(n)
	case "receipt":
		s.handleReceipt(n)
	case "call":
		s.handleCall(n)
	case "presence":
		s.handlePresence(n)
	case "chatstate":
		s.handleChatState(n)
	default:
		s.log.Debug("ignoring unknown node", "tag", n.Tag)
	}
}

func (s *session) handleIQ(n *binary.Node) {
	typ, _ := n.AttrString("type")
	id, _ := n.AttrString("id")
	switch {
	case typ == "get":
		if xmlns, _ := n.AttrString("xmlns"); xmlns == "urn:xmpp:ping" {
			s.sendOrLog(iqResult(id, n))
			return
		}
	case typ == "set":
		if _, ok := n.Child("pair-device"); ok {
			s.handlePairDevice(n, id)
			return
		}
		if _, ok := n.Child("pair-success"); ok {
			s.handlePairSuccess(n)
			return
		}
	}
	s.log.Debug("unhandled iq", "type", typ, "node", n.String())
}

func iqResult(id string, req *binary.Node) binary.Node {
	to := binary.ServerJID
	if from, ok := req.AttrJID("from"); ok {
		to = from
	}
	return binary.Node{Tag: "iq", Attrs: binary.Attrs{"to": to, "type": "result", "id": id}}
}

func (s *session) sendOrLog(n binary.Node) {
	if err := s.sendNode(n); err != nil {
		s.log.Warn("send failed", "tag", n.Tag, "error", err)
	}
}

// --- QR pairing -------------------------------------------------------------

type qrState struct {
	done chan struct{} // closed when pairing succeeds or the session ends
}

func (s *session) handlePairDevice(n *binary.Node, id string) {
	s.sendOrLog(iqResult(id, n))
	if s.dev.IsPaired() {
		s.log.Warn("server sent pair-device to an already paired device; ignoring")
		return
	}
	refs, err := pairing.ParsePairDevice(n)
	if err != nil {
		s.log.Error("bad pair-device", "error", err)
		return
	}
	if s.pairing != nil {
		close(s.pairing.done) // a fresh set of refs replaces the old rotation
	}
	st := &qrState{done: make(chan struct{})}
	s.pairing = st
	ct := pairing.ClientTypeFor(s.c.cfg.Device.Platform)
	s.goSafe("qr-rotator", func() {
		for i, ref := range refs {
			timeout := pairing.NextRefTimeout
			if i == 0 {
				timeout = pairing.FirstRefTimeout
			}
			s.c.emit(QRCode{
				Content: pairing.QRContent(ref, s.dev.NoiseKey.Pub, s.dev.IdentityKey.Pub, s.dev.AdvSecret, ct),
				Timeout: timeout, Index: i + 1, Total: len(refs),
			})
			select {
			case <-time.After(timeout):
			case <-st.done:
				return
			case <-s.ctx.Done():
				return
			}
		}
		s.c.emit(QRTimeout{})
		s.end(sessionResult{action: ActionFatal, err: ErrQRTimeout})
	})
}

func (s *session) handlePairSuccess(n *binary.Node) {
	if s.pairing != nil {
		close(s.pairing.done)
		s.pairing = nil
	}
	ps, err := pairing.ParsePairSuccess(n)
	if err != nil {
		id, _ := n.AttrString("id")
		s.failPairing(id, err)
		return
	}
	res, err := pairing.Verify(ps, s.dev.IdentityKey, s.dev.AdvSecret)
	if err != nil {
		s.failPairing(ps.IQID, err)
		return
	}

	dev := *s.dev
	dev.JID, dev.LID = ps.JID, ps.LID
	dev.Account = res.Account
	dev.Platform, dev.BusinessName = ps.Platform, ps.BusinessName
	dev.PairedAt = time.Now()
	// The primary device's identity is stored under its device-0 address,
	// preferring the LID (WhatsApp's primary addressing for Signal).
	primary := ps.LID.ToNonAD()
	if primary.IsEmpty() {
		primary = ps.JID.ToNonAD()
	}
	// Persist before confirming: if the process dies between the two, the
	// server never completes pairing and the next login fails cleanly
	// (logged out → new QR). The reverse order could leave the phone
	// showing a device whose keys we never saved.
	err = s.c.cfg.Store.InTx(s.ctx, func(tx *store.Store) error {
		if err := tx.SavePairing(s.ctx, &dev); err != nil {
			return err
		}
		return tx.PutIdentity(s.ctx, dev.InstanceID, signalAddress(primary), res.PrimaryIdentity)
	})
	if err != nil {
		s.failPairing(ps.IQID, fmt.Errorf("persist pairing: %w", err))
		return
	}
	if err := s.sendNode(res.ConfirmNode(ps.IQID)); err != nil {
		// Leave the stored pairing: login will either succeed (server got
		// it) or fail with 401, which deletes it.
		s.log.Error("failed to send pairing confirmation", "error", err)
	}
	*s.dev = dev
	s.log.Info("paired successfully", "jid", binary.MaskJID(dev.JID), "platform", dev.Platform)
	s.c.emit(PairSuccess{JID: dev.JID, LID: dev.LID, Platform: dev.Platform, BusinessName: dev.BusinessName})
	// The server now ends the stream with 515 and we reconnect with a login
	// payload; nothing else to do here.
}

func (s *session) failPairing(iqID string, err error) {
	s.log.Error("pairing failed", "error", err)
	if iqID != "" {
		s.sendOrLog(pairing.ErrorNode(iqID, err))
	}
	s.c.emit(PairError{Err: err})
	s.end(sessionResult{action: ActionFatal, err: fmt.Errorf("%w: %w", ErrPairing, err)})
}

// --- login --------------------------------------------------------------------

func (s *session) handleSuccess(n *binary.Node) {
	if !s.dev.IsPaired() {
		s.log.Warn("got <success> on an unpaired connection; ignoring")
		return
	}
	s.loggedInAt = time.Now()
	s.log.Info("logged in")
	// Old stanzas first, before anything new from the server is handled.
	s.replayDeferred()
	if !s.dev.LID.IsEmpty() {
		s.c.learnLID(s.ctx, s.dev.JID.ToNonAD(), s.dev.LID.ToNonAD())
	}
	lid, ok := n.AttrJID("lid")
	if ok && lid.Device == 0 {
		// Our device index is the same under both identities; the server
		// may send the LID without it.
		lid.Device = s.dev.JID.Device
	}
	if ok && !lid.IsEmpty() && lid != s.dev.LID {
		if err := s.c.cfg.Store.UpdateLID(s.ctx, s.dev.InstanceID, lid); err != nil {
			s.log.Warn("failed to store LID", "error", err)
		} else {
			s.dev.LID = lid
		}
	}
	// Post-login setup does IQ round trips, so it must not block the
	// handler goroutine (responses are matched by the reader).
	s.goSafe("post-login", func() {
		s.goSafe("history-worker", s.historyWorker)
		if err := s.c.cfg.Store.PurgeOld(s.ctx, s.c.cfg.InstanceID, 30*24*time.Hour, 7*24*time.Hour); err != nil {
			s.log.Warn("purging old buffers failed", "error", err)
		}
		if err := s.c.ensurePreKeys(s.ctx, s); err != nil {
			s.log.Warn("prekey maintenance failed; will retry on next connect", "error", err)
		}
		// Tell the server we are an active client so it delivers messages.
		if _, err := s.sendIQ(s.ctx, binary.Node{
			Tag:     "iq",
			Attrs:   binary.Attrs{"to": binary.ServerJID, "type": "set", "xmlns": "passive"},
			Content: []binary.Node{{Tag: "active"}},
		}, 0); err != nil {
			s.log.Warn("failed to set active", "error", err)
		}
		s.ready.Store(true)
		s.c.emit(Connected{JID: s.dev.JID})
		// Bring app state up to date (contacts, chat settings, push name).
		s.c.syncAllAppState(s.ctx, s)
	})
}

func (s *session) handleIB(n *binary.Node) {
	for _, c := range n.Children() {
		switch c.Tag {
		case "offline_preview":
			s.log.Info("offline queue preview", "attrs", c.String())
		case "offline":
			s.log.Info("offline queue delivered", "attrs", c.String())
		default:
			s.log.Debug("ib child", "tag", c.Tag)
		}
	}
}

// --- notifications ------------------------------------------------------------

func (s *session) handleNotification(n *binary.Node) {
	typ, _ := n.AttrString("type")
	from, _ := n.AttrJID("from")
	if typ == "encrypt" && from == binary.ServerJID {
		if cnt, ok := n.Child("count"); ok {
			s.ack(n)
			left, _ := cnt.AttrInt("value")
			s.log.Info("server reports low prekey count", "left", left)
			s.goSafe("prekey-refill", func() {
				if err := s.c.uploadPreKeys(s.ctx, s, false); err != nil {
					s.log.Warn("prekey refill failed", "error", err)
				}
			})
			return
		}
	}
	switch typ {
	case "w:gp2":
		s.handleGroupNotification(n)
		return
	case "server_sync":
		// App state changed on another device: sync the named collections.
		var names []string
		for _, coll := range n.ChildrenByTag("collection") {
			if name, ok := coll.AttrString("name"); ok {
				names = append(names, name)
			}
		}
		s.ack(n)
		s.goSafe("appstate-notify", func() {
			for _, name := range names {
				if err := s.c.syncAppState(s.ctx, s, name, false); err != nil && !errors.Is(err, errMissingKeys) {
					s.log.Warn("app state sync failed", "collection", name, "error", err)
				}
			}
		})
		return
	case "account_sync":
		var changes []string
		for _, ch := range n.Children() {
			changes = append(changes, ch.Tag)
			if ch.Tag == "devices" {
				s.c.devCache.invalidate(s.dev.JID.ToNonAD())
				s.c.devCache.invalidate(s.dev.LID.ToNonAD())
			}
		}
		s.c.emit(AccountSync{Changes: changes})
		s.ack(n)
		return
	case "disappearing_mode", "mex", "picture", "status", "privacy_token":
		// Informational for us; acknowledged so they stop occupying the
		// offline queue. (privacy_token storage is future work.)
		s.log.Debug("notification acknowledged", "type", typ)
		s.ack(n)
		return
	case "devices":
		// A user added or removed a device: drop the cached list.
		s.c.devCache.invalidate(from)
		s.ack(n)
		return
	case "encrypt":
		if _, ok := n.Child("identity"); ok && from.User != "" {
			// The user re-registered: their old sessions are useless.
			if err := s.c.cfg.Store.DeleteUserSessions(s.ctx, s.c.cfg.InstanceID, signalAddressName(from.ToNonAD())); err != nil {
				s.log.Warn("failed to drop sessions after identity change", "error", err)
				return // unacked: retried on redelivery
			}
			s.c.devCache.invalidate(from)
			s.c.emit(IdentityChanged{JID: from})
			s.ack(n)
			return
		}
	}
	// Other notifications are handled in later phases.
	s.deferStanza(n, "notification type "+typ+" not implemented yet")
}

// deferStanza stores a stanza we cannot process yet and acknowledges it only
// after the write succeeded. Leaving stanzas unacknowledged is not an
// option: the server delivers its offline queue in windows and stops at
// unacknowledged stanzas, which would also hold back messages we can handle.
func (s *session) deferStanza(n *binary.Node, reason string) {
	if s.replaying {
		s.redeferred = true // already stored (and acked); keep the row
		return
	}
	id, _ := n.AttrString("id")
	raw, err := binary.Marshal(*n)
	if err == nil {
		err = s.c.cfg.Store.PutDeferred(s.ctx, s.c.cfg.InstanceID, n.Tag, id, reason, raw)
	}
	if err != nil {
		s.log.Warn("could not defer stanza; leaving it unacknowledged", "tag", n.Tag, "id", id, "error", err)
		return
	}
	s.log.Debug("deferred stanza", "tag", n.Tag, "id", id, "reason", reason)
	s.ack(n)
}

// ack acknowledges a stanza so the server removes it from our queue.
// Replayed stanzas were acknowledged when they were deferred.
func (s *session) ack(n *binary.Node) {
	if s.replaying {
		return
	}
	attrs := binary.Attrs{"class": n.Tag, "id": n.Attrs["id"], "to": n.Attrs["from"]}
	for _, k := range []string{"participant", "recipient"} {
		if v, ok := n.Attrs[k]; ok {
			attrs[k] = v
		}
	}
	if t, ok := n.Attrs["type"]; ok && n.Tag != "message" {
		attrs["type"] = t
	}
	s.sendOrLog(binary.Node{Tag: "ack", Attrs: attrs})
}

// --- keepalive ----------------------------------------------------------------

func (s *session) keepAliveLoop() {
	defer s.wg.Done()
	defer s.recoverInto("keepalive")
	cfg := s.c.cfg
	lastSuccess := time.Now()
	failures := 0
	for {
		interval := cfg.KeepAliveMin
		if span := cfg.KeepAliveMax - cfg.KeepAliveMin; span > 0 {
			interval += rand.N(span)
		}
		if !sleepCtx(s.ctx, interval) {
			return
		}
		_, err := s.sendIQ(s.ctx, binary.Node{
			Tag:     "iq",
			Attrs:   binary.Attrs{"to": binary.ServerJID, "type": "get", "xmlns": "w:p"},
			Content: []binary.Node{{Tag: "ping"}},
		}, cfg.KeepAliveTimeout)
		if s.ctx.Err() != nil {
			return
		}
		if err == nil {
			if failures > 0 {
				s.log.Info("keepalive restored", "after_failures", failures)
			}
			failures, lastSuccess = 0, time.Now()
			continue
		}
		failures++
		s.log.Warn("keepalive failed", "error", err, "failures", failures, "since_success", time.Since(lastSuccess).Round(time.Second))
		s.c.emit(KeepAliveTimeout{Failures: failures, LastSuccess: lastSuccess})
		if time.Since(lastSuccess) > cfg.KeepAliveMaxFailure {
			// A half-open TCP connection can look alive for a long time;
			// tearing it down is the only way to recover.
			s.end(sessionResult{action: ActionReconnect, err: fmt.Errorf("keepalive: no response for %s: %w", time.Since(lastSuccess).Round(time.Second), err)})
			return
		}
	}
}

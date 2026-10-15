package client

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/proto"

	wabin "github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/media"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/proto/wahistorysync"
	"github.com/mdenesfe/wax/internal/store"
)

const (
	// maxHistoryAttempts bounds retries of one history chunk (the blob may
	// have expired on the media server).
	maxHistoryAttempts = 5
	// maxHistorySize bounds a decompressed history chunk.
	maxHistorySize = 256 << 20
)

// enqueueHistory stores a history sync notification durably before the
// carrying message is acknowledged; the worker downloads it later.
func (s *session) enqueueHistory(n *wae2e.HistorySyncNotification) error {
	raw, err := proto.Marshal(n)
	if err != nil {
		return err
	}
	if err := s.c.cfg.Store.EnqueueHistory(s.ctx, s.c.cfg.InstanceID, raw); err != nil {
		return err
	}
	s.kickHistory()
	return nil
}

func (s *session) kickHistory() {
	select {
	case s.c.history <- struct{}{}:
	default:
	}
}

// historyWorker processes queued history chunks one at a time.
func (s *session) historyWorker() {
	for {
		processed, err := s.processNextHistory()
		if err != nil && s.ctx.Err() == nil {
			s.log.Warn("history sync chunk failed; will retry", "error", err)
		}
		if processed && err == nil {
			continue // drain the queue
		}
		wait := time.Minute
		if err != nil {
			wait = 15 * time.Second
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.c.history:
		case <-time.After(wait):
		}
	}
}

func (s *session) processNextHistory() (bool, error) {
	c := s.c
	job, err := c.cfg.Store.NextHistoryJob(s.ctx, c.cfg.InstanceID, maxHistoryAttempts)
	if err != nil || job == nil {
		return false, err
	}
	fail := func(err error) (bool, error) {
		_ = c.cfg.Store.FailHistoryJob(context.WithoutCancel(s.ctx), job.ID, err.Error())
		return true, err
	}
	var n wae2e.HistorySyncNotification
	if err := proto.Unmarshal(job.Notification, &n); err != nil {
		_ = c.cfg.Store.DeleteHistoryJob(s.ctx, job.ID)
		return true, fmt.Errorf("drop invalid history notification: %w", err)
	}
	compressed := n.GetInitialHistBootstrapInlinePayload()
	if compressed == nil {
		compressed, err = c.DownloadMedia(s.ctx, n.GetDirectPath(), n.GetMediaKey(), n.GetFileEncSHA256(), n.GetFileSHA256(), media.History)
		if err != nil {
			return fail(fmt.Errorf("download: %w", err))
		}
	}
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return fail(fmt.Errorf("zlib: %w", err))
	}
	raw, err := io.ReadAll(io.LimitReader(zr, maxHistorySize+1))
	zr.Close()
	if err != nil {
		return fail(fmt.Errorf("inflate: %w", err))
	}
	if len(raw) > maxHistorySize {
		return fail(errors.New("history chunk too large"))
	}
	var hs wahistorysync.HistorySync
	if err := proto.Unmarshal(raw, &hs); err != nil {
		return fail(fmt.Errorf("decode: %w", err))
	}
	ev := HistorySync{Type: hs.GetSyncType().String(), Chunk: hs.GetChunkOrder(), Progress: hs.GetProgress(), PushNames: len(hs.GetPushnames())}
	err = c.cfg.Store.InTx(s.ctx, func(tx *store.Store) error {
		inst := c.cfg.InstanceID
		for _, m := range hs.GetPhoneNumberToLidMappings() {
			pn, e1 := wabin.ParseJID(m.GetPnJID())
			lid, e2 := wabin.ParseJID(m.GetLidJID())
			if e1 == nil && e2 == nil && pn.User != "" && lid.User != "" {
				if _, err := tx.PutLIDMapping(s.ctx, inst, pn.User, lid.User); err != nil {
					return err
				}
			}
		}
		for _, p := range hs.GetPushnames() {
			if p.GetID() != "" && p.GetPushname() != "" {
				if err := tx.PutPushName(s.ctx, inst, p.GetID(), p.GetPushname()); err != nil {
					return err
				}
			}
		}
		if len(hs.GetNctSalt()) > 0 {
			if err := tx.PutNCTSalt(s.ctx, inst, hs.GetNctSalt()); err != nil {
				return err
			}
		}
		for _, conv := range hs.GetConversations() {
			chat := conv.GetID()
			if chat == "" {
				continue
			}
			ev.Conversations++
			u := store.ChatUpdate{}
			if name := firstNonEmpty(conv.GetName(), conv.GetDisplayName()); name != "" {
				u.Name = &name
			}
			if ts := conv.GetConversationTimestamp(); ts > 0 {
				t := time.Unix(int64(ts), 0)
				u.LastMessageAt = &t
			}
			unread := int(conv.GetUnreadCount())
			u.UnreadCount = &unread
			archived, pinned := conv.GetArchived(), conv.GetPinned() > 0
			u.Archived, u.Pinned = &archived, &pinned
			if end := conv.GetMuteEndTime(); end > 0 {
				t := time.Unix(int64(end), 0)
				u.MutedUntil = &t
			}
			if err := tx.UpdateChat(s.ctx, inst, chat, u); err != nil {
				return err
			}
			for _, hm := range conv.GetMessages() {
				wmi := hm.GetMessage()
				key := wmi.GetKey()
				if key.GetID() == "" {
					continue
				}
				var content []byte
				if wmi.GetMessage() != nil {
					if content, err = proto.Marshal(wmi.GetMessage()); err != nil {
						return err
					}
				}
				sender := key.GetParticipant()
				if sender == "" {
					sender = wmi.GetParticipant()
				}
				if sender == "" && !key.GetFromMe() {
					sender = chat
				}
				typ := "text"
				if wmi.GetMessage() == nil {
					typ = fmt.Sprintf("stub:%d", wmi.GetMessageStubType())
				} else if mt := mediaType(wmi.GetMessage()); mt != "" {
					typ = mt
				}
				if err := tx.PutMessage(s.ctx, inst, &store.StoredMessage{
					Chat: chat, ID: key.GetID(), Sender: sender, FromMe: key.GetFromMe(),
					SentAt: time.Unix(int64(wmi.GetMessageTimestamp()), 0), PushName: wmi.GetPushName(),
					Type: typ, Content: content, Source: "history",
				}); err != nil {
					return err
				}
				ev.Messages++
			}
		}
		return tx.DeleteHistoryJob(s.ctx, job.ID)
	})
	if err != nil {
		return fail(fmt.Errorf("store: %w", err))
	}
	s.log.Info("history sync chunk stored", "type", ev.Type, "chunk", ev.Chunk, "progress", ev.Progress,
		"conversations", ev.Conversations, "messages", ev.Messages, "push_names", ev.PushNames)
	c.emit(ev)
	return true, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// storeLiveMessage persists an incoming or outgoing message for listing.
func (c *Client) storeLiveMessage(ctx context.Context, chat, sender wabin.JID, id string, fromMe bool, at time.Time, pushName string, msg *wae2e.Message) {
	content, err := proto.Marshal(msg)
	if err != nil {
		return
	}
	typ := messageType(msg)
	if mt := mediaType(msg); mt != "" {
		typ = mt
	}
	err = c.cfg.Store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.PutMessage(ctx, c.cfg.InstanceID, &store.StoredMessage{
			Chat: chat.String(), ID: id, Sender: sender.String(), FromMe: fromMe, SentAt: at,
			PushName: pushName, Type: typ, Content: content, Source: "live",
		}); err != nil {
			return err
		}
		return tx.UpdateChat(ctx, c.cfg.InstanceID, chat.String(), store.ChatUpdate{LastMessageAt: &at})
	})
	if err != nil {
		c.log.Warn("could not store message", "id", id, "error", err)
	}
}

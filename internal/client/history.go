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

	"github.com/mdenesfe/wax/internal/media"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/proto/wahistorysync"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/wabin"
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

// processNextHistory downloads, decodes and stores the oldest queued
// chunk. It reports whether a job was found.
func (s *session) processNextHistory() (bool, error) {
	c := s.c
	job, err := c.cfg.Store.NextHistoryJob(s.ctx, c.cfg.InstanceID, maxHistoryAttempts)
	if err != nil || job == nil {
		return false, err
	}
	var n wae2e.HistorySyncNotification
	if err := proto.Unmarshal(job.Notification, &n); err != nil {
		_ = c.cfg.Store.DeleteHistoryJob(s.ctx, job.ID)
		return true, fmt.Errorf("drop invalid history notification: %w", err)
	}
	hs, err := s.loadHistory(&n)
	if err == nil {
		var ev HistorySync
		ev, err = s.storeHistory(job.ID, hs)
		if err == nil {
			s.log.Info("history sync chunk stored", "type", ev.Type, "chunk", ev.Chunk, "progress", ev.Progress,
				"conversations", ev.Conversations, "messages", ev.Messages, "push_names", ev.PushNames)
			c.emit(ev)
			return true, nil
		}
	}
	_ = c.cfg.Store.FailHistoryJob(context.WithoutCancel(s.ctx), job.ID, err.Error())
	return true, err
}

// loadHistory fetches (inline or from the media server), inflates and
// decodes one history chunk.
func (s *session) loadHistory(n *wae2e.HistorySyncNotification) (*wahistorysync.HistorySync, error) {
	compressed := n.GetInitialHistBootstrapInlinePayload()
	if compressed == nil {
		var err error
		compressed, err = s.c.DownloadMedia(s.ctx, n.GetDirectPath(), n.GetMediaKey(), n.GetFileEncSHA256(), n.GetFileSHA256(), media.History)
		if err != nil {
			return nil, fmt.Errorf("download: %w", err)
		}
	}
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("zlib: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, maxHistorySize+1))
	if err != nil {
		return nil, fmt.Errorf("inflate: %w", err)
	}
	if len(raw) > maxHistorySize {
		return nil, errors.New("history chunk too large")
	}
	var hs wahistorysync.HistorySync
	if err := proto.Unmarshal(raw, &hs); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &hs, nil
}

// storeHistory persists a decoded chunk and deletes its job atomically, so
// a crash never stores a chunk twice or loses it.
func (s *session) storeHistory(jobID int64, hs *wahistorysync.HistorySync) (HistorySync, error) {
	ev := HistorySync{Type: hs.GetSyncType().String(), Chunk: hs.GetChunkOrder(), Progress: hs.GetProgress(), PushNames: len(hs.GetPushnames())}
	inst := s.c.cfg.InstanceID
	// All writes go in one batch: one round trip and one implicit
	// transaction instead of a round trip per row.
	err := s.c.cfg.Store.Batch(s.ctx, func(tx *store.Store) error {
		for _, m := range hs.GetPhoneNumberToLidMappings() {
			if err := storeLIDMapping(s.ctx, tx, inst, m.GetPnJID(), m.GetLidJID()); err != nil {
				return err
			}
		}
		for _, p := range hs.GetPushnames() {
			if p.GetID() == "" || p.GetPushname() == "" {
				continue
			}
			if err := tx.PutPushName(s.ctx, inst, p.GetID(), p.GetPushname()); err != nil {
				return err
			}
		}
		if len(hs.GetNctSalt()) > 0 {
			if err := tx.PutNCTSalt(s.ctx, inst, hs.GetNctSalt()); err != nil {
				return err
			}
		}
		for _, conv := range hs.GetConversations() {
			if conv.GetID() == "" {
				continue
			}
			n, err := s.storeConversation(tx, conv)
			if err != nil {
				return err
			}
			ev.Conversations++
			ev.Messages += n
		}
		return tx.DeleteHistoryJob(s.ctx, jobID)
	})
	if err != nil {
		return ev, fmt.Errorf("store: %w", err)
	}
	return ev, nil
}

// storeConversation stores a conversation's chat metadata and messages and
// returns the number of messages stored.
func (s *session) storeConversation(tx *store.Store, conv *wahistorysync.Conversation) (int, error) {
	inst, chat := s.c.cfg.InstanceID, conv.GetID()
	unread := int(conv.GetUnreadCount())
	archived, pinned := conv.GetArchived(), conv.GetPinned() > 0
	u := store.ChatUpdate{UnreadCount: &unread, Archived: &archived, Pinned: &pinned,
		LastMessageAt: unixPtr(conv.GetConversationTimestamp()), MutedUntil: unixPtr(conv.GetMuteEndTime())}
	if name := firstNonEmpty(conv.GetName(), conv.GetDisplayName()); name != "" {
		u.Name = &name
	}
	if err := tx.UpdateChat(s.ctx, inst, chat, u); err != nil {
		return 0, err
	}
	stored := 0
	for _, hm := range conv.GetMessages() {
		m, err := historyMessage(chat, hm.GetMessage())
		if err != nil {
			return stored, err
		}
		if m == nil {
			continue
		}
		if err := tx.PutMessage(s.ctx, inst, m); err != nil {
			return stored, err
		}
		stored++
	}
	return stored, nil
}

// historyMessage converts a history message for storage; nil means skip.
func historyMessage(chat string, wmi *wahistorysync.WebMessageInfo) (*store.StoredMessage, error) {
	key := wmi.GetKey()
	if key.GetID() == "" {
		return nil, nil
	}
	m := &store.StoredMessage{
		Chat: chat, ID: key.GetID(), Sender: firstNonEmpty(key.GetParticipant(), wmi.GetParticipant()), FromMe: key.GetFromMe(),
		SentAt: time.Unix(int64(wmi.GetMessageTimestamp()), 0), PushName: wmi.GetPushName(), Type: "text", Source: "history",
	}
	if m.Sender == "" && !m.FromMe {
		m.Sender = chat
	}
	if wmi.GetMessage() == nil {
		m.Type = fmt.Sprintf("stub:%d", wmi.GetMessageStubType())
		return m, nil
	}
	if mt := mediaType(wmi.GetMessage()); mt != "" {
		m.Type = mt
	}
	var err error
	m.Content, err = proto.Marshal(wmi.GetMessage())
	return m, err
}

// unixPtr converts a non-zero Unix timestamp to a *time.Time.
func unixPtr(ts uint64) *time.Time {
	if ts == 0 {
		return nil
	}
	t := time.Unix(int64(ts), 0)
	return &t
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
	// One round trip: both writes are queued and sent together.
	err = c.cfg.Store.Batch(ctx, func(tx *store.Store) error {
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

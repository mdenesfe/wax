package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/ratelimit"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/wabin"
	"github.com/mdenesfe/wax/proto/wae2e"
)

// Outbox policy.
const (
	outboxMaxAttempts = 10
)

// MediaPayload is the queued form of a media send.
type MediaPayload struct {
	Kind     client.MediaKind `json:"kind"` // image, video, audio, voice, document
	MimeType string           `json:"mimetype"`
	Caption  string           `json:"caption,omitempty"`
	FileName string           `json:"file_name,omitempty"`
	Data     []byte           `json:"data"`
	// Context is a serialized wae2e.ContextInfo (reply, mentions).
	Context []byte `json:"context,omitempty"`
	// Optional previews (see client.SendMediaRequest).
	Thumbnail []byte `json:"thumbnail,omitempty"`
	Width     uint32 `json:"width,omitempty"`
	Height    uint32 `json:"height,omitempty"`
	Seconds   uint32 `json:"seconds,omitempty"`
	Waveform  []byte `json:"waveform,omitempty"`
}

// sendBucket paces an instance's sends: rate per minute, burst of 3.
func sendBucket(perMinute int) *ratelimit.Bucket {
	if perMinute <= 0 {
		perMinute = 20
	}
	return ratelimit.New(float64(perMinute)/60, 3)
}

// errInvalidItem marks queued items that can never be sent.
var errInvalidItem = errors.New("invalid outbox item")

// permanent reports errors that retrying cannot fix.
func permanent(err error) bool {
	for _, target := range []error{errInvalidItem, wabin.ErrInvalidJID, client.ErrUnsupportedChat, client.ErrServerRejected, client.ErrNotOnWhatsApp} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// outboxLoop sends this instance's queued messages in order, paced by the
// instance's rate limit, while the client is logged in.
func (in *Instance) outboxLoop(ctx context.Context) {
	defer in.m.recoverLoop("outbox", in.ID)
	bucket := sendBucket(in.config().RatePerMinute)
	for {
		wait := in.sendOne(ctx, bucket)
		select {
		case <-ctx.Done():
			return
		case <-in.outboxKick:
		case <-time.After(wait):
		}
	}
}

func (in *Instance) sendOne(ctx context.Context, bucket *ratelimit.Bucket) time.Duration {
	tm := in.m.cfg.Timing
	cl := in.client()
	if cl == nil || !cl.IsLoggedIn() {
		return tm.Idle // nothing can be sent; Connected kicks the loop
	}
	st := in.m.store
	it, err := st.NextOutbox(ctx, in.ID)
	if err != nil {
		in.log.Warn("outbox read failed", "error", err)
		return tm.OutboxRetry.Min
	}
	if it == nil {
		due, err := st.NextOutboxDue(ctx, in.ID)
		if err == nil && due != nil {
			return max(time.Until(*due), time.Millisecond)
		}
		return tm.Idle
	}
	if err := bucket.Wait(ctx); err != nil {
		return 0
	}
	to, err := wabin.ParseJID(it.Chat)
	var res *client.SendResult
	if err == nil {
		sendCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		res, err = in.send(sendCtx, cl, to, it)
		cancel()
	}
	if ctx.Err() != nil {
		return 0
	}
	switch {
	case err == nil:
		ts := res.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		in.m.met.sent.Inc(in.TenantID, in.Name, "sent")
		if merr := st.MarkOutboxSent(ctx, it.ID, ts); merr != nil {
			in.log.Error("could not mark outbox item sent", "id", it.ID, "error", merr)
		}
		in.emitWebhook("send_status", map[string]any{"outbox_id": it.ID, "client_id": it.ClientID, "message_id": it.MessageID,
			"chat": it.Chat, "status": "sent", "timestamp": ts})
	case errors.Is(err, client.ErrNotLoggedIn):
		// Disconnected mid-send: not the message's fault; Connected kicks
		// the loop again.
		return tm.Idle
	case permanent(err) || it.Attempts+1 >= outboxMaxAttempts:
		in.m.met.sent.Inc(in.TenantID, in.Name, "failed")
		_ = st.MarkOutboxFailed(ctx, it.ID, err.Error())
		in.log.Warn("message send failed", "outbox_id", it.ID, "error", err)
		in.emitWebhook("send_status", map[string]any{"outbox_id": it.ID, "client_id": it.ClientID, "message_id": it.MessageID,
			"chat": it.Chat, "status": "failed", "error": err.Error()})
	default:
		delay := tm.OutboxRetry.Delay(it.Attempts + 1)
		_ = st.MarkOutboxRetry(ctx, it.ID, time.Now().Add(delay), err.Error())
		in.log.Warn("message send will be retried", "outbox_id", it.ID, "retry_in", delay, "error", err)
	}
	return 0
}

func (in *Instance) send(ctx context.Context, cl *client.Client, to wabin.JID, it *store.OutboxItem) (*client.SendResult, error) {
	switch it.Kind {
	case "message":
		var msg wae2e.Message
		if err := proto.Unmarshal(it.Payload, &msg); err != nil {
			return nil, fmt.Errorf("%w: message: %w", errInvalidItem, err)
		}
		return cl.SendMessageWithID(ctx, to, &msg, it.MessageID)
	case "media":
		var mp MediaPayload
		if err := json.Unmarshal(it.Payload, &mp); err != nil {
			return nil, fmt.Errorf("%w: media: %w", errInvalidItem, err)
		}
		if !mp.Kind.Valid() {
			return nil, fmt.Errorf("%w: media kind %q", errInvalidItem, mp.Kind)
		}
		req := client.SendMediaRequest{Kind: mp.Kind, Data: mp.Data, MimeType: mp.MimeType, Caption: mp.Caption, FileName: mp.FileName,
			ID: it.MessageID, Thumbnail: mp.Thumbnail, Width: mp.Width, Height: mp.Height, Seconds: mp.Seconds, Waveform: mp.Waveform}
		if mp.Context != nil {
			req.Context = &wae2e.ContextInfo{}
			if err := proto.Unmarshal(mp.Context, req.Context); err != nil {
				return nil, fmt.Errorf("%w: media context: %w", errInvalidItem, err)
			}
		}
		return cl.SendMedia(ctx, to, req)
	}
	return nil, fmt.Errorf("%w: kind %q", errInvalidItem, it.Kind)
}

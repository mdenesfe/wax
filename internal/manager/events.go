package manager

import (
	"encoding/base64"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/pairing"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/wabin"
)

// Envelope is the JSON body of every webhook delivery.
type Envelope struct {
	Event     string    `json:"event"`
	Instance  string    `json:"instance"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data"`
}

// MessageJSON is the webhook/API representation of a message.
type MessageJSON struct {
	ID        string        `json:"id"`
	Chat      string        `json:"chat"`
	Sender    string        `json:"sender,omitempty"`
	SenderAlt string        `json:"sender_alt,omitempty"`
	FromMe    bool          `json:"from_me"`
	IsGroup   bool          `json:"is_group"`
	PushName  string        `json:"push_name,omitempty"`
	Timestamp time.Time     `json:"timestamp"`
	Type      string        `json:"type"`
	Text      string        `json:"text,omitempty"`
	Media     *MediaJSON    `json:"media,omitempty"`
	Reaction  *ReactionJSON `json:"reaction,omitempty"`
	QuotedID  string        `json:"quoted_id,omitempty"`
	Raw       any           `json:"raw,omitempty"` // protojson of the message
	Source    string        `json:"source,omitempty"`
}

// MediaJSON describes attached media; Download is an API path.
type MediaJSON struct {
	Kind       string `json:"kind"`
	MimeType   string `json:"mimetype,omitempty"`
	Caption    string `json:"caption,omitempty"`
	FileName   string `json:"file_name,omitempty"`
	FileLength uint64 `json:"file_length,omitempty"`
	Download   string `json:"download"`
}

// ReactionJSON is an emoji reaction to a message.
type ReactionJSON struct {
	MessageID string `json:"message_id"`
	Emoji     string `json:"emoji"`
}

var protoJSON = protojson.MarshalOptions{UseProtoNames: false, EmitUnpopulated: false}

// rawJSON renders a message as protojson for the "raw" field (bytes become
// base64 per protojson rules).
func rawJSON(m *wae2e.Message) any {
	b, err := protoJSON.Marshal(m)
	if err != nil {
		return nil
	}
	return jsonRaw(b)
}

type jsonRaw []byte

func (r jsonRaw) MarshalJSON() ([]byte, error) { return r, nil }

// mediaMessage is the accessor set shared by the generated media message
// types.
type mediaMessage interface {
	GetMimetype() string
	GetFileLength() uint64
	GetContextInfo() *wae2e.ContextInfo
}

// mediaOf returns the API kind and the media sub-message of m, if any.
func mediaOf(m *wae2e.Message) (string, mediaMessage) {
	switch {
	case m.GetImageMessage() != nil:
		return "image", m.GetImageMessage()
	case m.GetVideoMessage() != nil:
		return "video", m.GetVideoMessage()
	case m.GetAudioMessage() != nil:
		if m.GetAudioMessage().GetPTT() {
			return "voice", m.GetAudioMessage()
		}
		return "audio", m.GetAudioMessage()
	case m.GetDocumentMessage() != nil:
		return "document", m.GetDocumentMessage()
	case m.GetStickerMessage() != nil:
		return "sticker", m.GetStickerMessage()
	}
	return "", nil
}

// DescribeMessage builds the JSON view of a message.
func DescribeMessage(instance string, chat wabin.JID, id string, m *wae2e.Message) (text, typ string, media *MediaJSON, reaction *ReactionJSON, quoted string) {
	typ = "text"
	switch {
	case m.GetConversation() != "":
		text = m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		text = m.GetExtendedTextMessage().GetText()
		quoted = m.GetExtendedTextMessage().GetContextInfo().GetStanzaID()
	case m.GetReactionMessage() != nil:
		typ = "reaction"
		reaction = &ReactionJSON{MessageID: m.GetReactionMessage().GetKey().GetID(), Emoji: m.GetReactionMessage().GetText()}
	case m.GetProtocolMessage() != nil:
		typ = "protocol"
	}
	kind, x := mediaOf(m)
	if x == nil {
		return
	}
	typ = kind
	media = &MediaJSON{Kind: kind, MimeType: x.GetMimetype(), FileLength: x.GetFileLength(),
		Download: "/v1/instances/" + instance + "/chats/" + chat.String() + "/messages/" + id + "/media"}
	if c, ok := x.(interface{ GetCaption() string }); ok {
		text, media.Caption = c.GetCaption(), c.GetCaption()
	}
	if f, ok := x.(interface{ GetFileName() string }); ok {
		media.FileName = f.GetFileName()
	}
	if q := x.GetContextInfo().GetStanzaID(); q != "" {
		quoted = q
	}
	return
}

func jidStr(j wabin.JID) string {
	if j.IsEmpty() {
		return ""
	}
	return j.String()
}

// WebhookEvents lists every webhook event name, as documented in the
// OpenAPI contract (api/openapi.json). Tests keep both in sync.
var WebhookEvents = []string{
	"message", "undecryptable_message", "receipt", "send_status",
	"qr", "qr_timeout", "paired", "pair_error", "connected", "disconnected", "logged_out", "replaced", "client_outdated",
	"call", "presence", "chat_presence", "group_update", "history_sync", "identity_changed",
	"app_state_error", "app_state_unverified",
}

// toWebhook converts a client event into (event name, data). ok is false
// for events that are not delivered to webhooks.
func toWebhook(instance string, e client.Event) (string, any, bool) {
	switch e := e.(type) {
	case client.Message:
		text, typ, media, reaction, quoted := DescribeMessage(instance, e.Info.Chat, e.Info.ID, e.Message)
		return "message", MessageJSON{
			ID: e.Info.ID, Chat: jidStr(e.Info.Chat), Sender: jidStr(e.Info.Sender), SenderAlt: jidStr(e.Info.SenderAlt),
			FromMe: e.Info.IsFromMe, IsGroup: e.Info.IsGroup, PushName: e.Info.PushName, Timestamp: e.Info.Timestamp,
			Type: typ, Text: text, Media: media, Reaction: reaction, QuotedID: quoted, Raw: rawJSON(e.Message), Source: "live",
		}, true
	case client.UndecryptableMessage:
		return "undecryptable_message", map[string]any{"id": e.Info.ID, "chat": jidStr(e.Info.Chat), "sender": jidStr(e.Info.Sender), "error": e.Err.Error()}, true
	case client.Receipt:
		t := e.Type
		if t == "" {
			t = "delivered"
		}
		return "receipt", map[string]any{"ids": e.IDs, "chat": jidStr(e.Chat), "sender": jidStr(e.Sender), "type": t, "timestamp": e.Timestamp}, true
	case client.QRCode:
		png, _ := pairing.RenderPNG(e.Content)
		return "qr", map[string]any{"code": e.Content, "png_base64": base64.StdEncoding.EncodeToString(png), "timeout_seconds": int(e.Timeout.Seconds()), "index": e.Index, "total": e.Total}, true
	case client.QRTimeout:
		return "qr_timeout", map[string]any{}, true
	case client.PairSuccess:
		return "paired", map[string]any{"jid": jidStr(e.JID), "lid": jidStr(e.LID), "platform": e.Platform, "business_name": e.BusinessName}, true
	case client.PairError:
		return "pair_error", map[string]any{"error": e.Err.Error()}, true
	case client.Connected:
		return "connected", map[string]any{"jid": jidStr(e.JID)}, true
	case client.Disconnected:
		d := map[string]any{"action": e.Action.String(), "retry_in_seconds": e.RetryIn.Seconds()}
		if e.Err != nil {
			d["error"] = e.Err.Error()
		}
		return "disconnected", d, true
	case client.LoggedOut:
		return "logged_out", map[string]any{"code": e.Code, "detail": e.Detail}, true
	case client.StreamReplaced:
		return "replaced", map[string]any{}, true
	case client.ClientOutdated:
		return "client_outdated", map[string]any{"version": e.Version}, true
	case client.CallEvent:
		return "call", map[string]any{"type": e.Type, "from": jidStr(e.From), "call_id": e.CallID, "creator": jidStr(e.Creator),
			"group": jidStr(e.Group), "video": e.Video, "reason": e.Reason, "timestamp": e.Timestamp}, true
	case client.Presence:
		return "presence", map[string]any{"from": jidStr(e.From), "available": e.Available, "last_seen": e.LastSeen}, true
	case client.ChatPresence:
		return "chat_presence", map[string]any{"chat": jidStr(e.Chat), "sender": jidStr(e.Sender), "state": e.State, "media": e.Media}, true
	case client.GroupUpdate:
		return "group_update", map[string]any{"group": jidStr(e.JID), "changes": e.Changes}, true
	case client.HistorySync:
		return "history_sync", map[string]any{"type": e.Type, "chunk": e.Chunk, "progress": e.Progress,
			"conversations": e.Conversations, "messages": e.Messages, "push_names": e.PushNames}, true
	case client.IdentityChanged:
		return "identity_changed", map[string]any{"jid": jidStr(e.JID)}, true
	case client.AppStateSyncError:
		return "app_state_error", map[string]any{"collection": e.Collection, "error": e.Err.Error()}, true
	case client.AppStateUnverified:
		return "app_state_unverified", map[string]any{"collection": e.Collection, "version": e.Version}, true
	}
	return "", nil, false
}

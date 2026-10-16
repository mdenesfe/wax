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
	ID        string `json:"id"`
	Chat      string `json:"chat"`
	Sender    string `json:"sender,omitempty"`
	SenderAlt string `json:"sender_alt,omitempty"`
	// SenderPhone, SenderName (address book) and SenderUsername describe
	// the sender as resolved across its phone number and LID; set in
	// message listings.
	SenderPhone    string        `json:"sender_phone,omitempty"`
	SenderName     string        `json:"sender_name,omitempty"`
	SenderUsername string        `json:"sender_username,omitempty"`
	FromMe         bool          `json:"from_me"`
	IsGroup        bool          `json:"is_group"`
	PushName       string        `json:"push_name,omitempty"`
	Timestamp      time.Time     `json:"timestamp"`
	Type           string        `json:"type"`
	Text           string        `json:"text,omitempty"`
	Media          *MediaJSON    `json:"media,omitempty"`
	Reaction       *ReactionJSON `json:"reaction,omitempty"`
	QuotedID       string        `json:"quoted_id,omitempty"`
	// Quoted is the message this one replies to, as the sender quoted it.
	Quoted *QuotedJSON `json:"quoted,omitempty"`
	// Mentions are the users tagged in the text (which contains "@<user>"
	// for each).
	Mentions        []PersonJSON `json:"mentions,omitempty"`
	Forwarded       bool         `json:"forwarded,omitempty"`
	ForwardingScore uint32       `json:"forwarding_score,omitempty"` // how often it was forwarded; WhatsApp says "many times" from 5
	ViewOnce        bool         `json:"view_once,omitempty"`
	Stub            *StubJSON    `json:"stub,omitempty"`
	Raw             any          `json:"raw,omitempty"` // protojson of the message
	Source          string       `json:"source,omitempty"`
}

// MediaJSON describes attached media; Download is an API path.
type MediaJSON struct {
	Kind       string `json:"kind"`
	MimeType   string `json:"mimetype,omitempty"`
	Caption    string `json:"caption,omitempty"`
	FileName   string `json:"file_name,omitempty"`
	FileLength uint64 `json:"file_length,omitempty"`
	Seconds    uint32 `json:"seconds,omitempty"`  // audio and video length
	Waveform   []byte `json:"waveform,omitempty"` // voice notes: 64 levels 0-100
	Download   string `json:"download"`
}

// PersonJSON is a user; message listings add what is known about it.
type PersonJSON struct {
	JID      string `json:"jid"`
	Phone    string `json:"phone,omitempty"`
	Name     string `json:"name,omitempty"`
	PushName string `json:"push_name,omitempty"`
	Username string `json:"username,omitempty"`
}

// QuotedJSON is the message a reply refers to.
type QuotedJSON struct {
	ID     string      `json:"id"`
	Sender *PersonJSON `json:"sender,omitempty"`
	Type   string      `json:"type"`
	Text   string      `json:"text,omitempty"`
}

// StubJSON is a system message from the history (group changes, security
// notices). Name is known for common types; parameters depend on the type
// (often the users concerned).
type StubJSON struct {
	Type       int      `json:"type"`
	Name       string   `json:"name,omitempty"`
	Parameters []string `json:"parameters,omitempty"`
	// People are the users among the parameters.
	People []PersonJSON `json:"people,omitempty"`
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

// DescribeMessage builds the content fields of a message's JSON view
// (type, text, media, reaction, reply, mentions, forwarding, view once);
// the caller adds the envelope (id, chat, sender, time).
func DescribeMessage(instance string, chat wabin.JID, id string, m *wae2e.Message) MessageJSON {
	m, viewOnce := client.Unwrap(m)
	out := MessageJSON{Type: "text", ViewOnce: viewOnce}
	var ci *wae2e.ContextInfo
	switch {
	case m.GetConversation() != "":
		out.Text = m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		out.Text = m.GetExtendedTextMessage().GetText()
		ci = m.GetExtendedTextMessage().GetContextInfo()
	case m.GetReactionMessage() != nil:
		out.Type = "reaction"
		out.Reaction = &ReactionJSON{MessageID: m.GetReactionMessage().GetKey().GetID(), Emoji: m.GetReactionMessage().GetText()}
	case m.GetProtocolMessage() != nil:
		out.Type = "protocol"
	}
	if kind, x := mediaOf(m); x != nil {
		out.Type = kind
		out.Media = &MediaJSON{Kind: kind, MimeType: x.GetMimetype(), FileLength: x.GetFileLength(),
			Download: "/v1/instances/" + instance + "/chats/" + chat.String() + "/messages/" + id + "/media"}
		if c, ok := x.(interface{ GetCaption() string }); ok {
			out.Text, out.Media.Caption = c.GetCaption(), c.GetCaption()
		}
		if f, ok := x.(interface{ GetFileName() string }); ok {
			out.Media.FileName = f.GetFileName()
		}
		if d, ok := x.(interface{ GetSeconds() uint32 }); ok {
			out.Media.Seconds = d.GetSeconds()
		}
		out.Media.Waveform = m.GetAudioMessage().GetWaveform()
		ci = x.GetContextInfo()
	}
	describeContext(&out, instance, chat, ci)
	return out
}

// describeContext adds what a message's context info says: the quoted
// message, mentions and forwarding.
func describeContext(out *MessageJSON, instance string, chat wabin.JID, ci *wae2e.ContextInfo) {
	if ci == nil {
		return
	}
	if q := ci.GetStanzaID(); q != "" {
		out.QuotedID = q
		out.Quoted = &QuotedJSON{ID: q, Type: "text"}
		if p := ci.GetParticipant(); p != "" {
			out.Quoted.Sender = &PersonJSON{JID: p}
		}
		if qm := ci.GetQuotedMessage(); qm != nil {
			d := DescribeMessage(instance, chat, q, qm)
			out.Quoted.Type, out.Quoted.Text = d.Type, d.Text
		}
	}
	for _, j := range ci.GetMentionedJID() {
		out.Mentions = append(out.Mentions, PersonJSON{JID: j})
	}
	out.Forwarded, out.ForwardingScore = ci.GetIsForwarded(), ci.GetForwardingScore()
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
		out := DescribeMessage(instance, e.Info.Chat, e.Info.ID, e.Message)
		out.ID, out.Chat, out.Sender, out.SenderAlt = e.Info.ID, jidStr(e.Info.Chat), jidStr(e.Info.Sender), jidStr(e.Info.SenderAlt)
		out.FromMe, out.IsGroup, out.PushName, out.Timestamp = e.Info.IsFromMe, e.Info.IsGroup, e.Info.PushName, e.Info.Timestamp
		out.ViewOnce = out.ViewOnce || e.ViewOnce
		out.Raw, out.Source = rawJSON(e.Message), "live"
		return "message", out, true
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

package client

import (
	"time"

	"github.com/mdenesfe/wax/internal/wabin"
	"github.com/mdenesfe/wax/proto/wae2e"
	"github.com/mdenesfe/wax/proto/wasyncaction"
)

// Event is emitted to Config.OnEvent. Handlers run synchronously on the
// session's handler goroutine and must return quickly.
type Event interface{ eventName() string }

// QRCode asks the user to scan Content within Timeout. A new QRCode replaces
// the previous one.
type QRCode struct {
	Content      string
	Timeout      time.Duration
	Index, Total int
}

// QRTimeout means every QR ref expired without being scanned. Run returns
// ErrQRTimeout afterwards.
type QRTimeout struct{}

// PairSuccess reports a completed pairing. The client reconnects and logs in
// on its own right after this event.
type PairSuccess struct {
	JID, LID     wabin.JID
	Platform     string
	BusinessName string
}

// PairError reports a rejected pair-success (bad HMAC/signature, DB error).
type PairError struct{ Err error }

// Connected means the client is logged in and ready.
type Connected struct{ JID wabin.JID }

// Disconnected is emitted whenever a connection ends. RetryIn is the backoff
// delay before the next attempt (zero for an immediate reconnect).
type Disconnected struct {
	Action  DisconnectAction
	Err     error
	RetryIn time.Duration
}

// LoggedOut means the session was revoked (e.g. removed from the phone). The
// stored device has been deleted; a new QR pairing is required.
type LoggedOut struct{ Code, Detail string }

// StreamReplaced means another connection took over this session.
type StreamReplaced struct{}

// ClientOutdated means the server rejected our WhatsApp Web version (405).
type ClientOutdated struct{ Version string }

// TemporaryBan reports a temporary ban (failure 402).
type TemporaryBan struct{ Detail string }

// KeepAliveTimeout reports a failed keepalive ping.
type KeepAliveTimeout struct {
	Failures    int
	LastSuccess time.Time
}

// Message is a decrypted incoming message (including messages sent from our
// other devices, with Info.IsFromMe set).
type Message struct {
	Info       MessageInfo
	Message    *wae2e.Message // content with wrappers (device sent, ephemeral, ...) removed
	Raw        *wae2e.Message // as decrypted
	RetryCount int
	ViewOnce   bool // sent as "view once"
}

// UndecryptableMessage reports a message we could not decrypt. A retry
// receipt was sent; the sender's device will usually resend it.
type UndecryptableMessage struct {
	Info MessageInfo
	Err  error
}

// Receipt reports delivery/read/played/retry receipts for our messages.
type Receipt struct {
	IDs       []string
	Chat      wabin.JID
	Sender    wabin.JID
	Type      string // "" (delivered), "read", "played", "sender", "retry", ...
	Timestamp time.Time
}

// IdentityChanged reports that a contact's identity key changed (reinstall
// or new phone). Their sessions were reset.
type IdentityChanged struct{ JID wabin.JID }

// GroupUpdate reports a change to a group (members, subject, settings).
// Changes lists the notification child tags (add, remove, subject, ...).
type GroupUpdate struct {
	JID     wabin.JID
	Changes []string
}

// Presence reports a contact's online status (after SubscribePresence).
type Presence struct {
	From      wabin.JID
	Available bool
	LastSeen  time.Time
}

// ChatPresence reports typing ("composing") or "paused" in a chat.
type ChatPresence struct {
	Chat, Sender wabin.JID
	State        string // composing, paused
	Media        string // "audio" while recording a voice note
}

// CallEvent reports call signalling: Type is "offer" (incoming call),
// "offer_notice", "accept", "reject", "terminate", ...
type CallEvent struct {
	Type      string
	From      wabin.JID
	CallID    string
	Creator   wabin.JID
	Group     wabin.JID
	Video     bool
	Reason    string
	Platform  string
	Timestamp time.Time
}

// AppStateChange is one change from app state sync (contact, mute, pin,
// archive, push name, ...). Index[0] names the kind, Index[1] usually the chat.
type AppStateChange struct {
	Collection   string
	Index        []string
	Action       *wasyncaction.SyncActionValue
	Removed      bool
	FromFullSync bool
}

// ChatUpdate reports a chat's archive, pin, mute or read state as the
// phone set it: from app state (live, or a full sync after linking) or from history
// sync. Only the fields that the change carries are set. Chat is the chat
// as WhatsApp names it (a phone number or LID JID); ChatAlt is its other
// address, when known.
type ChatUpdate struct {
	Chat, ChatAlt    wabin.JID
	Archived, Pinned *bool
	// Read is true when the chat was marked read, false when it was
	// marked unread.
	Read *bool
	// Muted is set for a mute change; MutedUntil is then the end of the
	// mute (year 9999: always), nil when unmuted.
	Muted      *bool
	MutedUntil *time.Time
	Timestamp  time.Time
	// FromHistory marks values from history sync (the state when the
	// account was linked); FromFullSync values from a full app state sync.
	FromHistory, FromFullSync bool
}

// AppStateSyncComplete reports a finished full sync of a collection.
type AppStateSyncComplete struct {
	Collection string
	Version    uint64
}

// AppStateUnverified reports a collection whose server LTHash could not be
// verified although every record was authenticated individually (see
// appstate.HashState). Its data is used; the phone is asked for a
// recovery copy.
type AppStateUnverified struct {
	Collection string
	Version    uint64
}

// AppStateSyncError reports a failed collection sync (it is retried on the
// next server_sync notification or reconnect).
type AppStateSyncError struct {
	Collection string
	Err        error
}

// HistorySync reports a processed history sync chunk.
type HistorySync struct {
	Type          string
	Chunk         uint32
	Progress      uint32
	Conversations int
	Messages      int
	PushNames     int
}

// AccountSync reports a change to our own account (devices, privacy, ...).
type AccountSync struct{ Changes []string }

func (AppStateChange) eventName() string       { return "appstate_change" }
func (AppStateSyncComplete) eventName() string { return "appstate_sync_complete" }
func (ChatUpdate) eventName() string           { return "chat_update" }
func (AppStateSyncError) eventName() string    { return "appstate_sync_error" }
func (AppStateUnverified) eventName() string   { return "appstate_unverified" }
func (HistorySync) eventName() string          { return "history_sync" }
func (AccountSync) eventName() string          { return "account_sync" }
func (GroupUpdate) eventName() string          { return "group_update" }
func (Presence) eventName() string             { return "presence" }
func (ChatPresence) eventName() string         { return "chat_presence" }
func (CallEvent) eventName() string            { return "call" }
func (Message) eventName() string              { return "message" }
func (UndecryptableMessage) eventName() string { return "undecryptable_message" }
func (Receipt) eventName() string              { return "receipt" }
func (IdentityChanged) eventName() string      { return "identity_changed" }
func (QRCode) eventName() string               { return "qr" }
func (QRTimeout) eventName() string            { return "qr_timeout" }
func (PairSuccess) eventName() string          { return "pair_success" }
func (PairError) eventName() string            { return "pair_error" }
func (Connected) eventName() string            { return "connected" }
func (Disconnected) eventName() string         { return "disconnected" }
func (LoggedOut) eventName() string            { return "logged_out" }
func (StreamReplaced) eventName() string       { return "stream_replaced" }
func (ClientOutdated) eventName() string       { return "client_outdated" }
func (TemporaryBan) eventName() string         { return "temporary_ban" }
func (KeepAliveTimeout) eventName() string     { return "keepalive_timeout" }

package client

import (
	"time"

	wabin "github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
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

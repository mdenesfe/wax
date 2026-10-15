package client

import (
	"time"

	"github.com/mdenesfe/wax/internal/binary"
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
	JID, LID     binary.JID
	Platform     string
	BusinessName string
}

// PairError reports a rejected pair-success (bad HMAC/signature, DB error).
type PairError struct{ Err error }

// Connected means the client is logged in and ready.
type Connected struct{ JID binary.JID }

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

func (QRCode) eventName() string           { return "qr" }
func (QRTimeout) eventName() string        { return "qr_timeout" }
func (PairSuccess) eventName() string      { return "pair_success" }
func (PairError) eventName() string        { return "pair_error" }
func (Connected) eventName() string        { return "connected" }
func (Disconnected) eventName() string     { return "disconnected" }
func (LoggedOut) eventName() string        { return "logged_out" }
func (StreamReplaced) eventName() string   { return "stream_replaced" }
func (ClientOutdated) eventName() string   { return "client_outdated" }
func (TemporaryBan) eventName() string     { return "temporary_ban" }
func (KeepAliveTimeout) eventName() string { return "keepalive_timeout" }

// Package wax is a WhatsApp Web multi-device client: it links to a phone
// as a companion device and sends and receives messages, end to end
// encrypted, in Go.
//
// A Client is one WhatsApp account. Its keys, sessions and history live in
// PostgreSQL, sealed with a master key, so a restarted program logs in
// again without a new QR code:
//
//	c, err := wax.Open(ctx, wax.Config{
//		DatabaseURL: os.Getenv("WAX_DATABASE_URL"),
//		MasterKey:   os.Getenv("WAX_MASTER_KEY"),
//		Account:     "main",
//		OnEvent: func(e wax.Event) {
//			switch e := e.(type) {
//			case wax.QRCode:
//				fmt.Println("scan:", e.Content)
//			case wax.Message:
//				fmt.Println(e.Info.Sender, e.Message.GetConversation())
//			}
//		},
//	})
//	if err != nil { ... }
//	defer c.Close()
//	err = c.Run(ctx) // connects, pairs if needed, and serves until ctx ends
//
// Message contents are the protobuf types of package
// github.com/mdenesfe/wax/proto/wae2e.
//
// The API is young: until version 1.0 it may change between releases.
// How the protocol works is documented in docs/protocol.
package wax

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/media"
	"github.com/mdenesfe/wax/internal/pairing"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/wabin"
	"github.com/mdenesfe/wax/internal/waversion"
	"github.com/mdenesfe/wax/proto/wae2e"
)

// JID is a WhatsApp address: a user (phone number or LID), a device or a
// group, for example 905550000001@s.whatsapp.net.
type JID = wabin.JID

// ParseJID parses a JID like "905550000001@s.whatsapp.net" or "1234@lid".
func ParseJID(s string) (JID, error) { return wabin.ParseJID(s) }

// ParseRecipient accepts a JID or a phone number ("+90 555 000 00 01").
func ParseRecipient(s string) (JID, error) { return wabin.ParseRecipient(s) }

// QRTerminal renders a QRCode's content for a terminal, with block
// characters.
func QRTerminal(content string) (string, error) { return pairing.RenderTerminal(content) }

// QRPNG renders a QRCode's content as a PNG image.
func QRPNG(content string) ([]byte, error) { return pairing.RenderPNG(content) }

// WebMToOgg rewraps a WebM Opus recording (what browsers record) as the
// Ogg Opus file WhatsApp expects for voice notes, without re-encoding. It
// also returns the length in seconds.
func WebMToOgg(webm []byte) (ogg []byte, seconds uint32, err error) { return media.WebMToOgg(webm) }

// Events. OnEvent receives them; switch on the concrete type.
type (
	Event                = client.Event
	QRCode               = client.QRCode               // a QR code to show; codes rotate until one is scanned
	QRTimeout            = client.QRTimeout            // no code was scanned; Run returns ErrQRTimeout
	PairSuccess          = client.PairSuccess          // the phone linked this device
	PairError            = client.PairError            // linking failed
	Connected            = client.Connected            // logged in and ready to send
	Disconnected         = client.Disconnected         // the connection dropped; Run reconnects on its own
	LoggedOut            = client.LoggedOut            // the device was unlinked; Run returns ErrLoggedOut
	StreamReplaced       = client.StreamReplaced       // another connection took over this device
	ClientOutdated       = client.ClientOutdated       // WhatsApp rejected the client version
	TemporaryBan         = client.TemporaryBan         // the account is temporarily banned
	KeepAliveTimeout     = client.KeepAliveTimeout     // pings went unanswered
	Message              = client.Message              // an incoming (or own, from another device) message
	MessageInfo          = client.MessageInfo          // a message's envelope: chat, sender, id, time
	UndecryptableMessage = client.UndecryptableMessage // a message that could not be decrypted (a retry was asked for)
	Receipt              = client.Receipt              // delivery or read receipts
	IdentityChanged      = client.IdentityChanged      // a contact's security code changed
	GroupUpdate          = client.GroupUpdate          // a group's metadata or members changed
	Presence             = client.Presence             // a contact came online or went offline
	ChatPresence         = client.ChatPresence         // typing or recording in a chat
	CallEvent            = client.CallEvent            // an incoming call offer or its end
	AppStateChange       = client.AppStateChange       // a setting synced from the phone (mute, pin, contact, ...)
	ChatUpdate           = client.ChatUpdate           // a chat was archived, pinned or marked read on the phone
	AppStateSyncComplete = client.AppStateSyncComplete // a settings collection is up to date
	AppStateUnverified   = client.AppStateUnverified   // a settings collection could not be fully verified
	AppStateSyncError    = client.AppStateSyncError    // a settings collection failed to sync
	HistorySync          = client.HistorySync          // a chunk of past conversations was stored
	AccountSync          = client.AccountSync          // account settings changed on another device
)

// Results and request types.
type (
	SendResult       = client.SendResult
	SendMediaRequest = client.SendMediaRequest
	MediaKind        = client.MediaKind
	GroupInfo        = client.GroupInfo
	GroupParticipant = client.GroupParticipant
	UserProfile      = client.UserProfile
	PhoneCheck       = client.PhoneCheck
	Business         = client.Business
	BusinessHours    = client.BusinessHours
	Picture          = client.Picture
)

// Media kinds for SendMedia.
const (
	KindImage     = client.KindImage
	KindVideo     = client.KindVideo
	KindAudio     = client.KindAudio
	KindVoiceNote = client.KindVoiceNote // Ogg Opus
	KindDocument  = client.KindDocument
	KindSticker   = client.KindSticker // WebP
)

// Errors. Run returns ErrLoggedOut and ErrQRTimeout when a new pairing is
// needed: call Run again for a new QR code.
var (
	ErrLoggedOut       = client.ErrLoggedOut
	ErrQRTimeout       = client.ErrQRTimeout
	ErrReplaced        = client.ErrReplaced
	ErrClientOutdated  = client.ErrClientOutdated
	ErrTempBanned      = client.ErrTempBanned
	ErrPairing         = client.ErrPairing
	ErrNotLoggedIn     = client.ErrNotLoggedIn
	ErrNotOnWhatsApp   = client.ErrNotOnWhatsApp
	ErrServerRejected  = client.ErrServerRejected
	ErrUnsupportedChat = client.ErrUnsupportedChat
	ErrGroupNotFound   = client.ErrGroupNotFound
	ErrNoMedia         = client.ErrNoMedia
	ErrNoPicture       = client.ErrNoPicture
	ErrRateLimited     = client.ErrRateLimited
	ErrNotSticker      = client.ErrNotSticker
	// ErrNotRunning means Run has not been called yet.
	ErrNotRunning = errors.New("wax: client is not running")
)

// Config configures one account.
type Config struct {
	// DatabaseURL is a PostgreSQL connection string. The schema is
	// created and migrated on Open.
	DatabaseURL string
	// MasterKey is the base64 encoding of 32 random bytes (for example
	// `openssl rand -base64 32`). It seals every key and session at rest;
	// losing it means linking the account again.
	MasterKey string
	// Account names the account; one database holds any number of them.
	Account string
	// Version is the WhatsApp Web version to announce, like
	// "2.3000.1012345678". Empty: the current one is looked up on Open,
	// with a built-in fallback.
	Version string
	// PushName is the display name used when announcing presence; it is
	// also learned from the phone.
	PushName string
	// OnEvent receives events on the client's handler goroutine: return
	// quickly, and do not call back into the Client synchronously (start
	// a goroutine to reply to a message).
	OnEvent func(Event)
	// Log receives the client's logs (nil: discarded).
	Log *slog.Logger
}

// Client is one WhatsApp account.
type Client struct {
	cfg     Config
	st      *store.Store
	version waversion.Version
	log     *slog.Logger
	current atomic.Pointer[client.Client]
}

// Open connects to the database (creating or migrating the schema) and
// prepares the account. It does not connect to WhatsApp; Run does.
func Open(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Account == "" {
		return nil, errors.New("wax: Config.Account is required")
	}
	key, err := store.ParseMasterKey(cfg.MasterKey)
	if err != nil {
		return nil, fmt.Errorf("wax: master key: %w", err)
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	version := waversion.Default
	if cfg.Version != "" {
		if version, err = waversion.Parse(cfg.Version); err != nil {
			return nil, fmt.Errorf("wax: version: %w", err)
		}
	} else if v, err := waversion.FetchLatest(ctx, nil); err == nil {
		version = v
	} else {
		log.Warn("version lookup failed, using the built-in version", "error", err)
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, key)
	if err != nil {
		return nil, fmt.Errorf("wax: %w", err)
	}
	return &Client{cfg: cfg, st: st, version: version, log: log}, nil
}

// Close releases the database connections. Stop Run (cancel its context)
// first.
func (c *Client) Close() { c.st.Close() }

// Run connects to WhatsApp and serves the account until ctx ends,
// reconnecting with backoff when the connection drops. An account that is
// not linked yet shows QR codes (QRCode events) to scan from the phone.
//
// Run returns ErrLoggedOut when the device is unlinked and ErrQRTimeout
// when no code was scanned; call it again to pair anew. It returns nil
// (or the context's error) when ctx ends.
func (c *Client) Run(ctx context.Context) error {
	cl, err := client.New(client.Config{
		Store: c.st, InstanceID: c.cfg.Account, Device: client.DefaultDeviceConfig(c.version),
		Log: c.log, OnEvent: c.cfg.OnEvent, PushName: c.cfg.PushName,
	})
	if err != nil {
		return fmt.Errorf("wax: %w", err)
	}
	c.current.Store(cl)
	return cl.Run(ctx)
}

// active is the running session's client.
func (c *Client) active() (*client.Client, error) {
	if cl := c.current.Load(); cl != nil {
		return cl, nil
	}
	return nil, ErrNotRunning
}

// IsLoggedIn reports whether the account is connected and logged in.
func (c *Client) IsLoggedIn() bool {
	cl := c.current.Load()
	return cl != nil && cl.IsLoggedIn()
}

// OwnJID returns the account's phone number and LID addresses once linked.
func (c *Client) OwnJID() (jid, lid JID, ok bool) {
	if cl := c.current.Load(); cl != nil {
		return cl.OwnJID()
	}
	return JID{}, JID{}, false
}

// SendText sends a text message to a user or group.
func (c *Client) SendText(ctx context.Context, to JID, text string) (*SendResult, error) {
	cl, err := c.active()
	if err != nil {
		return nil, err
	}
	return cl.SendText(ctx, to, text)
}

// SendMessage sends any message (replies, mentions, ...) built from the
// wae2e protobuf types.
func (c *Client) SendMessage(ctx context.Context, to JID, msg *wae2e.Message) (*SendResult, error) {
	cl, err := c.active()
	if err != nil {
		return nil, err
	}
	return cl.SendMessage(ctx, to, msg)
}

// SendMedia uploads a file and sends it as a photo, video, audio, voice
// note (Ogg Opus) or document.
func (c *Client) SendMedia(ctx context.Context, to JID, req SendMediaRequest) (*SendResult, error) {
	cl, err := c.active()
	if err != nil {
		return nil, err
	}
	return cl.SendMedia(ctx, to, req)
}

// Download fetches and decrypts the media of a message.
func (c *Client) Download(ctx context.Context, msg *wae2e.Message) ([]byte, error) {
	cl, err := c.active()
	if err != nil {
		return nil, err
	}
	return cl.Download(ctx, msg)
}

// MarkRead sends read receipts for messages of a chat (sender: the
// author, in groups).
func (c *Client) MarkRead(ctx context.Context, chat, sender JID, ids []string) error {
	cl, err := c.active()
	if err != nil {
		return err
	}
	return cl.MarkRead(ctx, chat, sender, ids, time.Now())
}

// SendPresence announces the account as available (online) or not.
func (c *Client) SendPresence(ctx context.Context, available bool) error {
	cl, err := c.active()
	if err != nil {
		return err
	}
	return cl.SendPresence(ctx, available)
}

// SendChatPresence shows (or stops showing) typing or recording in a chat.
func (c *Client) SendChatPresence(ctx context.Context, chat JID, composing, recording bool) error {
	cl, err := c.active()
	if err != nil {
		return err
	}
	return cl.SendChatPresence(ctx, chat, composing, recording)
}

// SubscribePresence asks for a contact's online status (Presence events).
func (c *Client) SubscribePresence(ctx context.Context, jid JID) error {
	cl, err := c.active()
	if err != nil {
		return err
	}
	return cl.SubscribePresence(ctx, jid)
}

// RejectCall declines an incoming call.
func (c *Client) RejectCall(ctx context.Context, from JID, callID string) error {
	cl, err := c.active()
	if err != nil {
		return err
	}
	return cl.RejectCall(ctx, from, callID)
}

// GroupInfo returns a group's metadata and members.
func (c *Client) GroupInfo(ctx context.Context, group JID) (*GroupInfo, error) {
	cl, err := c.active()
	if err != nil {
		return nil, err
	}
	return cl.GetGroupInfo(ctx, group)
}

// JoinedGroups lists the groups and communities the account is in.
func (c *Client) JoinedGroups(ctx context.Context) ([]*GroupInfo, error) {
	cl, err := c.active()
	if err != nil {
		return nil, err
	}
	return cl.JoinedGroups(ctx)
}

// Profile returns a user's about text, devices and business profile.
// WhatsApp allows only a few lookups in a row (ErrRateLimited).
func (c *Client) Profile(ctx context.Context, user JID) (*UserProfile, error) {
	cl, err := c.active()
	if err != nil {
		return nil, err
	}
	return cl.Profile(ctx, user)
}

// CheckPhone reports whether a phone number (international format) has
// a WhatsApp account. Lookups share Profile's limit (ErrRateLimited).
func (c *Client) CheckPhone(ctx context.Context, phone string) (PhoneCheck, error) {
	cl, err := c.active()
	if err != nil {
		return PhoneCheck{}, err
	}
	return cl.CheckPhone(ctx, phone)
}

// ProfilePicture returns the small profile picture of a user or group
// (ErrNoPicture when there is none or it is hidden).
func (c *Client) ProfilePicture(ctx context.Context, jid JID) (*Picture, error) {
	cl, err := c.active()
	if err != nil {
		return nil, err
	}
	return cl.ProfilePicture(ctx, jid)
}

// Logout unlinks the device from the phone and deletes its keys.
func (c *Client) Logout(ctx context.Context) error {
	cl, err := c.active()
	if err != nil {
		return err
	}
	return cl.Logout(ctx)
}

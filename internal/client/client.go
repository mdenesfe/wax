package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/transport"
)

// Errors that end Run. Everything else is retried internally.
var (
	ErrLoggedOut      = errors.New("client: session logged out; new QR pairing required")
	ErrReplaced       = errors.New("client: session replaced by another connection")
	ErrClientOutdated = errors.New("client: WhatsApp Web version rejected as outdated")
	ErrTempBanned     = errors.New("client: account temporarily banned")
	ErrQRTimeout      = errors.New("client: QR code was not scanned in time")
	ErrPairing        = errors.New("client: pairing failed")
	ErrFatal          = errors.New("client: unrecoverable connect failure")
)

// Config configures a Client.
type Config struct {
	Store      *store.Store
	InstanceID string
	Device     DeviceConfig
	Log        *slog.Logger

	URL        string                  // empty: production endpoint
	HTTPClient *http.Client            // optional (proxy etc.)
	Verifier   *transport.CertVerifier // nil: WhatsApp's root

	// OnEvent receives events. Optional. It runs on the handler goroutine:
	// keep it fast and never call back into the Client synchronously.
	OnEvent func(Event)

	Backoff Backoff // zero: DefaultBackoff

	// Keepalive timing; zero values use the defaults below.
	KeepAliveMin, KeepAliveMax time.Duration // ping interval range (20-30 s)
	KeepAliveTimeout           time.Duration // response deadline (10 s)
	KeepAliveMaxFailure        time.Duration // reconnect after this long without a pong (90 s)

	// DumpNodes logs every received node at debug level (masked unless
	// DumpUnmasked is also set).
	DumpNodes    bool
	DumpUnmasked bool
}

// Client manages one WhatsApp linked-device session: it connects, pairs if
// needed, logs in and keeps the connection alive across failures.
type Client struct {
	cfg Config
	log *slog.Logger

	preKeyMu sync.Mutex // serializes prekey uploads

	mu      sync.Mutex
	current *session // nil while disconnected
}

// New validates the configuration and returns a Client. Call Run to start it.
func New(cfg Config) (*Client, error) {
	if cfg.Store == nil || cfg.InstanceID == "" {
		return nil, errors.New("client: Store and InstanceID are required")
	}
	if cfg.Device.Version.IsZero() {
		return nil, errors.New("client: Device.Version is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Backoff == (Backoff{}) {
		cfg.Backoff = DefaultBackoff
	}
	setDefault(&cfg.KeepAliveMin, 20*time.Second)
	setDefault(&cfg.KeepAliveMax, 30*time.Second)
	setDefault(&cfg.KeepAliveTimeout, 10*time.Second)
	setDefault(&cfg.KeepAliveMaxFailure, 90*time.Second)
	return &Client{cfg: cfg, log: cfg.Log.With("instance", cfg.InstanceID)}, nil
}

func setDefault(d *time.Duration, v time.Duration) {
	if *d <= 0 {
		*d = v
	}
}

// emit delivers an event, isolating the client from panics in the handler.
func (c *Client) emit(e Event) {
	if c.cfg.OnEvent == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("event handler panicked", "event", e.eventName(), "panic", r, "stack", string(debug.Stack()))
		}
	}()
	c.cfg.OnEvent(e)
}

// Run connects and keeps the session alive until ctx is cancelled or a
// terminal condition occurs (logged out, replaced, outdated, banned, QR
// timeout, pairing failure). Transient failures are retried with backoff.
func (c *Client) Run(ctx context.Context) error {
	attempt := 0
	for {
		dev, err := c.loadOrCreateDevice(ctx)
		if err != nil {
			// The database is unavailable: retry like a network failure,
			// since a DB restart must not kill long-running instances.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			attempt++
			delay := c.cfg.Backoff.Delay(attempt)
			c.log.Error("cannot load device, retrying", "error", err, "retry_in", delay)
			if !sleepCtx(ctx, delay) {
				return ctx.Err()
			}
			continue
		}

		res := c.runSession(ctx, dev)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if res.loggedInFor >= 30*time.Second {
			attempt = 0 // the connection was healthy; start backoff over
		}

		switch res.action {
		case ActionReconnectNow:
			attempt = 0
			c.emit(Disconnected{Action: res.action, Err: res.err})
			c.log.Info("reconnecting immediately", "reason", res.err)
			continue
		case ActionReconnect:
			attempt++
			delay := c.cfg.Backoff.Delay(attempt)
			c.emit(Disconnected{Action: res.action, Err: res.err, RetryIn: delay})
			c.log.Warn("connection lost, reconnecting", "error", res.err, "attempt", attempt, "retry_in", delay.Round(time.Millisecond))
			if !sleepCtx(ctx, delay) {
				return ctx.Err()
			}
			continue
		case ActionLoggedOut:
			c.emit(Disconnected{Action: res.action, Err: res.err})
			if err := c.cfg.Store.DeleteDevice(context.WithoutCancel(ctx), c.cfg.InstanceID); err != nil {
				c.log.Error("failed to delete logged out device", "error", err)
			}
			c.emit(LoggedOut{Code: res.code, Detail: res.detail})
			return fmt.Errorf("%w (code %s %s)", ErrLoggedOut, res.code, res.detail)
		case ActionReplaced:
			c.emit(Disconnected{Action: res.action, Err: res.err})
			c.emit(StreamReplaced{})
			return ErrReplaced
		case ActionUpgradeRequired:
			c.emit(Disconnected{Action: res.action, Err: res.err})
			c.emit(ClientOutdated{Version: c.cfg.Device.Version.String()})
			return fmt.Errorf("%w: %s", ErrClientOutdated, c.cfg.Device.Version)
		case ActionTempBanned:
			c.emit(Disconnected{Action: res.action, Err: res.err})
			c.emit(TemporaryBan{Detail: res.detail})
			return fmt.Errorf("%w: %s", ErrTempBanned, res.detail)
		default: // ActionFatal and session-defined terminal errors
			c.emit(Disconnected{Action: res.action, Err: res.err})
			if res.err == nil {
				res.err = ErrFatal
			}
			return res.err
		}
	}
}

func (c *Client) loadOrCreateDevice(ctx context.Context) (*store.Device, error) {
	dev, err := c.cfg.Store.LoadDevice(ctx, c.cfg.InstanceID)
	if err == nil {
		return dev, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	dev, err = store.NewDevice(c.cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	if err := c.cfg.Store.CreateDevice(ctx, dev); err != nil {
		return nil, err
	}
	c.log.Info("created new unpaired device")
	return dev, nil
}

// payloadFor returns the login payload for a paired device and the
// registration payload otherwise.
func (c *Client) payloadFor(dev *store.Device) ([]byte, error) {
	if dev.IsPaired() {
		user, err := strconv.ParseUint(dev.JID.User, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("client: stored JID %s is not numeric: %w", dev.JID, err)
		}
		return LoginPayload(c.cfg.Device, user, uint32(dev.JID.Device))
	}
	return RegistrationPayload(c.cfg.Device, RegistrationKeys{
		RegistrationID:  dev.RegistrationID,
		IdentityKey:     dev.IdentityKey.Pub,
		SignedPreKeyID:  dev.SignedPreKey.ID,
		SignedPreKey:    dev.SignedPreKey.Key.Pub,
		SignedPreKeySig: dev.SignedPreKey.Signature,
	})
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

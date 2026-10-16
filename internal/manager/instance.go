package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/store"
)

// Status is an instance's connection state.
type Status string

// Instance states.
const (
	StatusStarting   Status = "starting"
	StatusQR         Status = "qr"         // waiting for the QR code to be scanned
	StatusQRTimeout  Status = "qr_timeout" // codes expired; call Connect
	StatusConnecting Status = "connecting" // (re)connecting
	StatusConnected  Status = "connected"  // logged in
	StatusLoggedOut  Status = "logged_out" // unlinked; a new QR is shown
	StatusReplaced   Status = "replaced"   // another connection took over; call Connect
	StatusOutdated   Status = "outdated"   // version rejected; retried later
	StatusBanned     Status = "banned"     // temporary ban; retried later
	StatusFailed     Status = "failed"     // unexpected error; retried later
	StatusStopped    Status = "stopped"
	StatusUnassigned Status = "unassigned" // no node runs it (none alive, or all at capacity)
)

// Instance is one WhatsApp account connection managed by the Manager.
// ID is the internal key; TenantID and Name identify it through the API
// (Name is what the tenant sees, also in webhooks).
type Instance struct {
	ID       string
	TenantID string
	Name     string
	// running is set for instances this node runs (it holds the lease);
	// others are detached views of the configuration.
	running bool

	m   *Manager
	log *slog.Logger

	cfgMu sync.RWMutex
	cfg   store.Instance

	cl     atomic.Pointer[client.Client]
	status atomic.Value // Status
	qr     atomic.Value // string: latest QR content

	connect    chan struct{} // manual (re)connect request
	hookKick   chan struct{}
	outboxKick chan struct{}

	cancel context.CancelFunc
	done   sync.WaitGroup
}

func newInstance(m *Manager, cfg store.Instance) *Instance {
	in := &Instance{
		ID: cfg.ID, TenantID: cfg.TenantID, Name: cfg.Name, m: m, cfg: cfg,
		log:        m.log.With("instance", cfg.ID),
		connect:    make(chan struct{}, 1),
		hookKick:   make(chan struct{}, 1),
		outboxKick: make(chan struct{}, 1),
	}
	in.setStatus(StatusStarting)
	return in
}

// Status returns the current state.
func (in *Instance) Status() Status { return in.status.Load().(Status) }

// QR returns the latest QR code content ("" if none is pending).
func (in *Instance) QR() string {
	if in.Status() != StatusQR {
		return ""
	}
	s, _ := in.qr.Load().(string)
	return s
}

// setStatus records a state change and asks the lease loop to publish it.
func (in *Instance) setStatus(s Status) {
	in.status.Store(s)
	if in.running {
		in.setStatusMetric(s)
	}
	kick(in.m.publish)
}

// Running reports whether this node runs the instance.
func (in *Instance) Running() bool { return in.running }

func (in *Instance) client() *client.Client { return in.cl.Load() }

func (in *Instance) config() store.Instance {
	in.cfgMu.RLock()
	defer in.cfgMu.RUnlock()
	return in.cfg
}

func kick(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// start launches the connection, outbox and webhook goroutines.
func (in *Instance) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	in.cancel = cancel
	for _, loop := range []func(context.Context){in.runLoop, in.outboxLoop, in.deliveryLoop} {
		in.done.Add(1)
		go func() {
			defer in.done.Done()
			loop(ctx)
		}()
	}
}

// stop ends all goroutines and waits for them.
func (in *Instance) stop() {
	if in.cancel != nil {
		in.cancel()
	}
	in.done.Wait()
	in.setStatus(StatusStopped)
}

// runLoop keeps the client running and decides what to do after it ends.
func (in *Instance) runLoop(ctx context.Context) {
	for ctx.Err() == nil {
		err := in.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		status, wait := classifyRunError(err)
		in.setStatus(status)
		in.log.Warn("client stopped", "status", status, "error", err, "retry_in", wait)
		if !in.waitForRetry(ctx, wait) {
			return
		}
	}
}

// classifyRunError maps why Run returned to a status and how long to wait
// before running again (0 = only on a manual Connect).
func classifyRunError(err error) (Status, time.Duration) {
	switch {
	case errors.Is(err, client.ErrLoggedOut):
		return StatusLoggedOut, time.Second // start a new pairing right away
	case errors.Is(err, client.ErrQRTimeout):
		return StatusQRTimeout, 0
	case errors.Is(err, client.ErrReplaced):
		return StatusReplaced, 0
	case errors.Is(err, client.ErrClientOutdated):
		return StatusOutdated, 10 * time.Minute
	case errors.Is(err, client.ErrTempBanned):
		return StatusBanned, time.Hour
	}
	return StatusFailed, 30 * time.Second
}

// waitForRetry sleeps for wait (or until Connect when wait is 0).
func (in *Instance) waitForRetry(ctx context.Context, wait time.Duration) bool {
	var timer <-chan time.Time
	if wait > 0 {
		timer = time.After(wait)
	}
	select {
	case <-ctx.Done():
		return false
	case <-in.connect:
	case <-timer:
	}
	return true
}

// runOnce runs one client lifetime. A panic is converted into an error so
// it cannot take down other instances or the process.
func (in *Instance) runOnce(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			in.log.Error("instance panicked", "panic", r)
		}
	}()
	cl, err := client.New(client.Config{
		Store:      in.m.store,
		InstanceID: in.ID,
		Device:     client.DefaultDeviceConfig(in.m.Version()),
		Version:    in.m.Version,
		Log:        in.m.log, // the client adds the instance attribute itself
		OnEvent:    in.onEvent,
		URL:        in.m.cfg.ClientURL,
		Verifier:   in.m.cfg.Verifier,
		HeavySlots: in.m.heavy,
	})
	if err != nil {
		return err
	}
	in.cl.Store(cl)
	in.setStatus(StatusConnecting)
	return cl.Run(ctx)
}

// onEvent updates the status and queues the webhook. It runs on the
// client's handler goroutine before the stanza is acknowledged, so a queued
// webhook is never lost.
func (in *Instance) onEvent(e client.Event) {
	in.countEvent(e)
	switch e := e.(type) {
	case client.QRCode:
		in.qr.Store(e.Content)
		in.setStatus(StatusQR)
	case client.Connected:
		in.setStatus(StatusConnected)
		kick(in.outboxKick)
	case client.Disconnected:
		in.setStatus(StatusConnecting)
	}
	name, data, ok := toWebhook(in.Name, e)
	if ok {
		in.emitWebhook(name, data)
	}
}

// webhookEnqueueTimeout bounds the database write of one webhook event.
const webhookEnqueueTimeout = 5 * time.Second

// emitWebhook queues an event for this instance's webhook (if configured).
func (in *Instance) emitWebhook(event string, data any) {
	if in.config().WebhookURL == "" {
		return
	}
	body, err := json.Marshal(Envelope{Event: event, Instance: in.Name, Timestamp: time.Now().UTC(), Data: data})
	if err != nil {
		in.log.Error("webhook payload encoding failed", "event", event, "error", err)
		return
	}
	// Bounded: events are emitted from the client's handler goroutine, which
	// must not hang on a slow database (nor block shutdown).
	ctx, cancel := context.WithTimeout(context.Background(), webhookEnqueueTimeout)
	defer cancel()
	if err := in.m.store.EnqueueWebhook(ctx, in.ID, event, body); err != nil {
		in.log.Error("webhook enqueue failed", "event", event, "error", err)
		return
	}
	kick(in.hookKick)
}

// Client returns the instance's current client (nil before the first run).
func (in *Instance) Client() *client.Client { return in.client() }

// Config returns the instance configuration.
func (in *Instance) Config() store.Instance { return in.config() }

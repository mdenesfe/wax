// Package manager runs many WhatsApp instances in one process. Each
// instance has its own client, send queue (outbox) and webhook delivery,
// isolated from the others: a panic or failure in one never affects another.
package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdenesfe/wax/internal/backoff"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/transport"
	"github.com/mdenesfe/wax/internal/waversion"
)

// Errors.
var (
	ErrNotFound     = errors.New("manager: instance not found")
	ErrExists       = errors.New("manager: instance already exists")
	ErrInvalidID    = errors.New("manager: instance id must be 1-64 characters of a-z, 0-9, '-' or '_'")
	ErrNotConnected = errors.New("manager: instance is not connected")
)

var idPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// Config configures the Manager.
type Config struct {
	Store   *store.Store
	Log     *slog.Logger
	Version waversion.Version // initial WhatsApp Web version
	// CheckVersion enables the periodic live version check.
	CheckVersion bool

	// For tests: alternative server endpoint and certificate root.
	ClientURL string
	Verifier  *transport.CertVerifier
	// WebhookHTTP overrides the webhook HTTP client (tests).
	WebhookHTTP *http.Client
	// Timing overrides retry and polling delays; zero fields use
	// DefaultTiming (tests use millisecond values).
	Timing Timing
}

// Timing holds the delays of the outbox and webhook loops.
type Timing struct {
	WebhookRetry backoff.Policy // failed webhook deliveries
	OutboxRetry  backoff.Policy // failed sends and queue read errors
	// Idle is how long an idle loop sleeps before re-checking its queue.
	// New work wakes it immediately, so this is only a safety net.
	Idle time.Duration
}

// DefaultTiming is the production timing.
var DefaultTiming = Timing{
	WebhookRetry: backoff.Policy{Min: 5 * time.Second, Max: 4 * time.Hour},
	OutboxRetry:  backoff.Policy{Min: 5 * time.Second, Max: 30 * time.Minute},
	Idle:         time.Minute,
}

// withDefaults fills zero fields from DefaultTiming.
func (t Timing) withDefaults() Timing {
	if t.WebhookRetry == (backoff.Policy{}) {
		t.WebhookRetry = DefaultTiming.WebhookRetry
	}
	if t.OutboxRetry == (backoff.Policy{}) {
		t.OutboxRetry = DefaultTiming.OutboxRetry
	}
	if t.Idle == 0 {
		t.Idle = DefaultTiming.Idle
	}
	return t
}

// Manager owns all instances.
type Manager struct {
	cfg      Config
	store    *store.Store
	log      *slog.Logger
	hookHTTP *http.Client
	version  atomic.Value // waversion.Version

	mu        sync.Mutex
	instances map[string]*Instance
	ctx       context.Context
}

// New creates a Manager. Call Start to run the stored instances.
func New(cfg Config) *Manager {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Version.IsZero() {
		cfg.Version = waversion.Default
	}
	cfg.Timing = cfg.Timing.withDefaults()
	m := &Manager{cfg: cfg, store: cfg.Store, log: cfg.Log, hookHTTP: cfg.WebhookHTTP, instances: map[string]*Instance{}}
	if m.hookHTTP == nil {
		m.hookHTTP = newWebhookClient()
	}
	m.version.Store(cfg.Version)
	return m
}

// Version returns the WhatsApp Web version used for new connections.
func (m *Manager) Version() waversion.Version { return m.version.Load().(waversion.Version) }

// Start runs every stored instance plus the maintenance loops. It returns
// once they are launched; they stop when ctx ends (see Stop).
func (m *Manager) Start(ctx context.Context) error {
	list, err := m.store.ListInstances(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.ctx = ctx
	for _, cfg := range list {
		in := newInstance(m, cfg)
		m.instances[cfg.ID] = in
		in.start(ctx)
	}
	m.mu.Unlock()
	m.log.Info("manager started", "instances", len(list), "version", m.Version().String())
	if m.cfg.CheckVersion {
		go m.watchVersion(ctx)
	}
	go m.maintenance(ctx)
	return nil
}

// Stop stops all instances and waits for them.
func (m *Manager) Stop() {
	m.mu.Lock()
	list := make([]*Instance, 0, len(m.instances))
	for _, in := range m.instances {
		list = append(list, in)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, in := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in.stop()
		}()
	}
	wg.Wait()
}

// Create registers and starts a new instance.
func (m *Manager) Create(ctx context.Context, cfg store.Instance) (*Instance, error) {
	if !idPattern.MatchString(cfg.ID) {
		return nil, ErrInvalidID
	}
	if err := m.store.CreateInstance(ctx, &cfg); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, ErrExists
		}
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil {
		return nil, errors.New("manager: not started")
	}
	in := newInstance(m, cfg)
	m.instances[cfg.ID] = in
	in.start(m.ctx)
	return in, nil
}

// Get returns an instance.
func (m *Manager) Get(id string) (*Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.instances[id]
	if !ok {
		return nil, ErrNotFound
	}
	return in, nil
}

// List returns all instances sorted by id.
func (m *Manager) List() []*Instance {
	m.mu.Lock()
	out := make([]*Instance, 0, len(m.instances))
	for _, in := range m.instances {
		out = append(out, in)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Update changes an instance's webhook and rate settings.
func (m *Manager) Update(ctx context.Context, cfg store.Instance) error {
	in, err := m.Get(cfg.ID)
	if err != nil {
		return err
	}
	if err := m.store.UpdateInstance(ctx, &cfg); err != nil {
		return err
	}
	in.cfgMu.Lock()
	in.cfg.WebhookURL, in.cfg.WebhookSecret, in.cfg.RatePerMinute = cfg.WebhookURL, cfg.WebhookSecret, cfg.RatePerMinute
	in.cfgMu.Unlock()
	return nil
}

// Delete unlinks the device (best effort), stops the instance and removes
// all of its data.
func (m *Manager) Delete(ctx context.Context, id string) error {
	in, err := m.Get(id)
	if err != nil {
		return err
	}
	if cl := in.client(); cl != nil && cl.IsLoggedIn() {
		lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := cl.Logout(lctx); err != nil {
			in.log.Warn("logout before delete failed", "error", err)
		}
		cancel()
	}
	in.stop()
	m.mu.Lock()
	delete(m.instances, id)
	m.mu.Unlock()
	return m.store.DeleteInstance(ctx, id)
}

// Connect asks a waiting instance (QR timeout, replaced) to run again.
func (m *Manager) Connect(id string) error {
	in, err := m.Get(id)
	if err != nil {
		return err
	}
	kick(in.connect)
	return nil
}

// recoverLoop logs a panic in an instance background loop; the loop's
// goroutine ends but the process and other instances keep running.
func (m *Manager) recoverLoop(loop, instance string) {
	if r := recover(); r != nil {
		m.log.Error("background loop panicked", "loop", loop, "instance", instance, "panic", r, "stack", string(debug.Stack()))
	}
}

// watchVersion adopts newer WhatsApp Web versions for future connections.
func (m *Manager) watchVersion(ctx context.Context) {
	defer m.recoverLoop("version", "")
	waversion.Watch(ctx, m.log, nil, 6*time.Hour, m.Version, func(v waversion.Version) {
		m.version.Store(v)
		m.log.Info("using new WhatsApp Web version for future connections", "version", v.String())
	})
}

// maintenance purges delivered webhooks.
func (m *Manager) maintenance(ctx context.Context) {
	defer m.recoverLoop("maintenance", "")
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := m.store.PurgeWebhooks(ctx, 7*24*time.Hour); err != nil && ctx.Err() == nil {
			m.log.Warn("webhook purge failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Enqueue queues an outgoing message for an instance and wakes its worker.
func (m *Manager) Enqueue(ctx context.Context, it *store.OutboxItem) (bool, error) {
	in, err := m.Get(it.Instance)
	if err != nil {
		return false, err
	}
	created, err := m.store.Enqueue(ctx, it)
	if err != nil {
		return false, fmt.Errorf("enqueue: %w", err)
	}
	kick(in.outboxKick)
	return created, nil
}

// Package manager runs WhatsApp instances. Each instance has its own
// client, send queue (outbox) and webhook delivery, isolated from the
// others: a panic or failure in one never affects another.
//
// Several nodes (processes) can share one database. Each instance runs on
// exactly one node, which holds a lease on it (see lease.go); nodes take
// over the instances of a node that stops or fails, and spread instances
// evenly between them.
package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdenesfe/wax/internal/backoff"
	"github.com/mdenesfe/wax/internal/metrics"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/transport"
	"github.com/mdenesfe/wax/internal/wabin"
	"github.com/mdenesfe/wax/internal/waversion"
)

// Errors.
var (
	ErrNotFound     = errors.New("manager: instance not found")
	ErrExists       = errors.New("manager: instance already exists")
	ErrInvalidID    = errors.New("manager: ids must be 1-64 characters of a-z, 0-9, '-' or '_'")
	ErrNotConnected = errors.New("manager: instance is not connected")
	ErrNotRunning   = errors.New("manager: instance is not running on any node")
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
	// AllowPrivateWebhooks permits webhook URLs that resolve to loopback or
	// private addresses (a receiver on the same host or LAN). Off by
	// default to prevent server-side request forgery.
	AllowPrivateWebhooks bool
	// Timing overrides retry and polling delays; zero fields use
	// DefaultTiming (tests use millisecond values).
	Timing Timing

	// NodeID identifies this node among those sharing the database
	// (default: random). Address is where the other nodes reach this node's
	// API, to forward requests for instances it runs.
	NodeID  string
	Address string
	// MaxInstances caps the instances this node runs (0: no cap).
	MaxInstances int
	// HeavyWork is how many history sync chunks this node processes at
	// once, across all instances (0: 2).
	HeavyWork int
	// Metrics receives the manager's metrics (nil: disabled).
	Metrics *metrics.Registry
}

// Timing holds the delays of the outbox and webhook loops.
type Timing struct {
	WebhookRetry backoff.Policy // failed webhook deliveries
	OutboxRetry  backoff.Policy // failed sends and queue read errors
	// LeaseTTL is how long an instance stays with a node that stopped
	// renewing; it bounds failover time. Leases renew every LeaseTTL/3.
	LeaseTTL time.Duration
	// Idle is how long an idle loop sleeps before re-checking its queue.
	// New work wakes it immediately, so this is only a safety net.
	Idle time.Duration
}

// DefaultTiming is the production timing.
var DefaultTiming = Timing{
	WebhookRetry: backoff.Policy{Min: 5 * time.Second, Max: 4 * time.Hour},
	OutboxRetry:  backoff.Policy{Min: 5 * time.Second, Max: 30 * time.Minute},
	Idle:         time.Minute,
	LeaseTTL:     30 * time.Second,
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
	if t.LeaseTTL == 0 {
		t.LeaseTTL = DefaultTiming.LeaseTTL
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
	instances map[string]*Instance // running on this node, by internal id
	ctx       context.Context

	heavy    chan struct{} // shared HeavySlots of this node's clients
	met      instruments
	publish  chan struct{}      // an instance's state changed: publish it soon
	stopped  chan struct{}      // closed when the lease loop has ended
	stopLoop context.CancelFunc // ends the lease loop (nil before Start)
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
	m := &Manager{cfg: cfg, store: cfg.Store, log: cfg.Log, hookHTTP: cfg.WebhookHTTP, instances: map[string]*Instance{},
		publish: make(chan struct{}, 1), stopped: make(chan struct{})}
	if m.cfg.HeavyWork <= 0 {
		m.cfg.HeavyWork = 2
	}
	m.heavy = make(chan struct{}, m.cfg.HeavyWork)
	m.met = newInstruments(cfg.Metrics)
	cfg.Metrics.OnCollect(m.collect)
	if m.cfg.NodeID == "" {
		m.cfg.NodeID = randomNodeID()
	}
	if m.hookHTTP == nil {
		m.hookHTTP = newWebhookClient(cfg.AllowPrivateWebhooks)
	}
	m.version.Store(cfg.Version)
	return m
}

// Version returns the WhatsApp Web version used for new connections.
func (m *Manager) Version() waversion.Version { return m.version.Load().(waversion.Version) }

// Start joins the cluster: it takes leases on instances (all of them when
// it is the only node) and keeps doing so in the background until ctx
// ends. It returns once the first round has run.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	l := newLeaser(m)
	if err := l.tick(ctx); err != nil {
		return err
	}
	m.log.Info("manager started", "node", m.cfg.NodeID, "instances", m.runningCount(), "version", m.Version().String())
	lctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.stopLoop = cancel
	m.mu.Unlock()
	go l.loop(lctx)
	if m.cfg.CheckVersion {
		go m.watchVersion(ctx)
	}
	go m.maintenance(ctx)
	return nil
}

// Stop stops this node's instances, releases their leases so other nodes
// take them over at once, and leaves the cluster.
func (m *Manager) Stop() {
	// End the lease loop first, so it cannot start instances meanwhile.
	m.mu.Lock()
	stopLoop := m.stopLoop
	m.stopLoop = nil
	m.mu.Unlock()
	if stopLoop == nil {
		return // not started, or already stopped
	}
	stopLoop()
	<-m.stopped
	m.mu.Lock()
	list := make([]*Instance, 0, len(m.instances))
	for _, in := range m.instances {
		list = append(list, in)
	}
	m.instances = map[string]*Instance{}
	m.mu.Unlock()
	stopAll(list)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ids := make([]string, len(list))
	for i, in := range list {
		ids[i] = in.ID
	}
	if len(ids) > 0 {
		if err := m.store.ReleaseLeases(ctx, m.cfg.NodeID, ids); err != nil {
			m.log.Warn("could not release leases; they expire on their own", "error", err)
		}
	}
	if err := m.store.DeleteNode(ctx, m.cfg.NodeID); err != nil {
		m.log.Warn("could not leave the cluster", "error", err)
	}
}

// stopAll stops instances in parallel and waits for them.
func stopAll(list []*Instance) {
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

// InternalID is the internal key of a new instance. Ids match idPattern,
// which excludes ':', so the result is unambiguous.
func InternalID(tenant, name string) string { return tenant + ":" + name }

// Create registers a new instance named cfg.Name for tenant cfg.TenantID
// (cfg.ID is assigned here) and starts it on this node when it can take
// the lease; otherwise another node picks it up.
func (m *Manager) Create(ctx context.Context, cfg store.Instance) (*Instance, error) {
	if !idPattern.MatchString(cfg.Name) || !idPattern.MatchString(cfg.TenantID) {
		return nil, ErrInvalidID
	}
	cfg.ID = InternalID(cfg.TenantID, cfg.Name)
	if err := m.store.CreateInstance(ctx, &cfg); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, ErrExists
		}
		return nil, err
	}
	ids, err := m.store.AcquireLeases(ctx, m.cfg.NodeID, m.cfg.Address, m.cfg.Timing.LeaseTTL, 1, cfg.ID)
	if err != nil || len(ids) == 0 {
		return newInstance(m, cfg), nil // runs wherever the next lease round puts it
	}
	m.met.lease.Inc("acquired")
	return m.run(cfg)
}

// run starts an instance whose lease this node holds.
func (m *Manager) run(cfg store.Instance) (*Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil {
		return nil, errors.New("manager: not started")
	}
	if in, ok := m.instances[cfg.ID]; ok {
		return in, nil
	}
	in := newInstance(m, cfg)
	in.running = true
	m.instances[cfg.ID] = in
	in.start(m.ctx)
	return in, nil
}

// Get returns an instance running on this node.
func (m *Manager) Get(id string) (*Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.instances[id]
	if !ok {
		return nil, ErrNotFound
	}
	return in, nil
}

// Resolve returns a tenant's instance by name: the running one when this
// node owns it, otherwise a detached one (configuration only; Running is
// false). Instances of other tenants are not found, so their existence is
// not revealed.
func (m *Manager) Resolve(ctx context.Context, tenant, name string) (*Instance, error) {
	cfg, err := m.store.InstanceByName(ctx, tenant, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if in, err := m.Get(cfg.ID); err == nil {
		return in, nil
	}
	return newInstance(m, *cfg), nil
}

// Owner returns the node running an instance (ErrNotFound if none).
func (m *Manager) Owner(ctx context.Context, in *Instance) (*store.Lease, error) {
	l, err := m.store.LeaseOwner(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	return l, err
}

// NodeID returns this node's id.
func (m *Manager) NodeID() string { return m.cfg.NodeID }

// View is an instance as shown through the API.
type View struct {
	Name          string
	Status        Status
	JID, LID      wabin.JID
	WebhookURL    string
	RatePerMinute int
	QRPending     bool
}

// Views lists a tenant's instances (only the one named name, if not "")
// across the cluster: state comes from this node for instances it runs and
// from the owners' published state otherwise.
func (m *Manager) Views(ctx context.Context, tenant, name string) ([]View, error) {
	rows, err := m.store.InstanceViews(ctx, tenant, name)
	if err != nil {
		return nil, err
	}
	out := make([]View, len(rows))
	for i, r := range rows {
		v := View{Name: r.Name, Status: Status(r.Status), WebhookURL: r.WebhookURL, RatePerMinute: r.RatePerMinute, QRPending: r.QRPending}
		if v.Status == "" {
			v.Status = StatusUnassigned
		}
		v.JID, _ = wabin.ParseJID(r.JID)
		v.LID, _ = wabin.ParseJID(r.LID)
		if in, err := m.Get(r.ID); err == nil { // live state is authoritative
			v.Status, v.QRPending = in.Status(), in.QR() != ""
		}
		v.JID, v.LID = v.JID.ToNonAD(), v.LID.ToNonAD()
		out[i] = v
	}
	return out, nil
}

func (m *Manager) runningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.instances)
}

// Update changes an instance's webhook and rate settings.
func (m *Manager) Update(ctx context.Context, in *Instance, cfg store.Instance) error {
	if err := m.store.UpdateInstance(ctx, &cfg); err != nil {
		return err
	}
	in.cfgMu.Lock()
	in.cfg.WebhookURL, in.cfg.WebhookSecret, in.cfg.RatePerMinute = cfg.WebhookURL, cfg.WebhookSecret, cfg.RatePerMinute
	in.cfgMu.Unlock()
	return nil
}

// Delete unlinks the device (best effort, when it runs here), stops the
// instance and removes all of its data. Requests for an instance running
// on another node are forwarded there by the API first.
func (m *Manager) Delete(ctx context.Context, in *Instance) error {
	if cl := in.client(); cl != nil && cl.IsLoggedIn() {
		lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := cl.Logout(lctx); err != nil {
			in.log.Warn("logout before delete failed", "error", err)
		}
		cancel()
	}
	if in.running {
		in.stop()
		m.mu.Lock()
		delete(m.instances, in.ID)
		m.mu.Unlock()
	}
	return m.store.DeleteInstance(ctx, in.ID)
}

// Connect asks an instance waiting here (QR timeout, replaced) to run
// again. An instance no node runs is picked up by the next lease round.
func (m *Manager) Connect(in *Instance) {
	if in.running {
		kick(in.connect)
	}
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

// Enqueue queues an outgoing message and wakes the instance's worker if
// it runs here (the API forwards sends to the owner node).
func (m *Manager) Enqueue(ctx context.Context, in *Instance, it *store.OutboxItem) (bool, error) {
	it.Instance = in.ID
	created, err := m.store.Enqueue(ctx, it)
	if err != nil {
		return false, fmt.Errorf("enqueue: %w", err)
	}
	if in.running {
		kick(in.outboxKick)
	}
	return created, nil
}

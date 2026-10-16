package manager

import (
	"context"
	"errors"
	"runtime"
	"time"

	"github.com/mdenesfe/wax/internal/client"
	"github.com/mdenesfe/wax/internal/metrics"
	"github.com/mdenesfe/wax/internal/signal"
)

// instruments are the manager's metrics. Every method is a no-op when
// metrics are disabled (nil registry). Per-instance series carry the
// tenant and the instance name, and are reported by the node running the
// instance.
type instruments struct {
	received   *metrics.CounterVec   // tenant, instance
	sent       *metrics.CounterVec   // tenant, instance, result
	decrypt    *metrics.CounterVec   // tenant, instance, kind
	reconnects *metrics.CounterVec   // tenant, instance
	status     *metrics.GaugeVec     // tenant, instance, status
	hooks      *metrics.CounterVec   // tenant, instance, result
	hookTime   *metrics.HistogramVec // tenant
	lease      *metrics.CounterVec   // event
}

func newInstruments(r *metrics.Registry) instruments {
	return instruments{
		received:   r.Counter("wax_messages_received_total", "Messages received and decrypted.", "tenant", "instance"),
		sent:       r.Counter("wax_messages_sent_total", "Outbox items finished, by result (sent, failed).", "tenant", "instance", "result"),
		decrypt:    r.Counter("wax_decrypt_failures_total", "Undecryptable messages; recoverable ones are fixed by retry receipts.", "tenant", "instance", "kind"),
		reconnects: r.Counter("wax_disconnects_total", "Connections that ended (followed by a reconnect unless stopped).", "tenant", "instance"),
		status:     r.Gauge("wax_instance_status", "1 for the current status of each instance running on this node.", "tenant", "instance", "status"),
		hooks:      r.Counter("wax_webhook_deliveries_total", "Webhook delivery attempts, by result (ok, retry, dead).", "tenant", "instance", "result"),
		hookTime:   r.Histogram("wax_webhook_delivery_seconds", "Duration of webhook HTTP requests.", metrics.DefaultBuckets, "tenant"),
		lease:      r.Counter("wax_lease_events_total", "Lease events on this node (acquired, released, lost, fenced).", "event"),
	}
}

// setStatusMetric records an instance's status (one series set to 1).
func (in *Instance) setStatusMetric(s Status) {
	g := in.m.met.status
	g.Forget(in.TenantID, in.Name) // drop the previous status of this instance
	if s != StatusStopped {
		g.Set(1, in.TenantID, in.Name, string(s))
	}
}

// countEvent updates the metrics for a client event.
func (in *Instance) countEvent(e client.Event) {
	m := &in.m.met
	switch e := e.(type) {
	case client.Message:
		m.received.Inc(in.TenantID, in.Name)
	case client.UndecryptableMessage:
		kind := "error"
		if errors.Is(e.Err, signal.ErrNoSession) || errors.Is(e.Err, signal.ErrMissingPreKey) {
			kind = "recoverable"
		}
		m.decrypt.Inc(in.TenantID, in.Name, kind)
	case client.Disconnected:
		m.reconnects.Inc(in.TenantID, in.Name)
	}
}

// collect refreshes gauges derived from other state, before each scrape.
func (m *Manager) collect(r *metrics.Registry) {
	ids := m.RunningIDs()
	r.Gauge("wax_instances_running", "Instances running on this node.").Set(float64(len(ids)))
	r.Gauge("wax_goroutines", "Goroutines of this process.").Set(float64(runtime.NumGoroutine()))
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	r.Gauge("wax_heap_bytes", "Heap memory in use.").Set(float64(ms.HeapAlloc))

	pool := m.store.Pool()
	pg := r.Gauge("wax_db_connections", "Database pool connections (max, total, acquired, idle).", "state")
	pg.Set(float64(pool.Max), "max")
	pg.Set(float64(pool.Total), "total")
	pg.Set(float64(pool.Acquired), "acquired")
	pg.Set(float64(pool.Idle), "idle")

	outbox := r.Gauge("wax_outbox_pending", "Messages waiting to be sent.", "tenant", "instance")
	hooks := r.Gauge("wax_webhooks_pending", "Webhook events waiting for delivery.", "tenant", "instance")
	outbox.Reset()
	hooks.Reset()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	depths, err := m.store.QueueDepths(ctx, ids)
	if err != nil {
		m.log.Warn("metrics: queue depths unavailable", "error", err)
		return
	}
	for _, id := range ids {
		in, err := m.Get(id)
		if err != nil {
			continue
		}
		d := depths[id]
		outbox.Set(float64(d.Outbox), in.TenantID, in.Name)
		hooks.Set(float64(d.Webhooks), in.TenantID, in.Name)
	}
}

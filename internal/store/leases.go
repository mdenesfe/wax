package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Leases decide which node runs an instance. All expiry checks use the
// database clock (now()), so clock differences between nodes do not matter.

// LeaseState is what the owner publishes about an instance.
type LeaseState struct {
	Status    string
	QRPending bool
}

// Lease is the current owner of an instance.
type Lease struct {
	Node    string
	Address string
}

// InstanceView is an instance as listed through the API: configuration,
// the owner's published state (if a node holds a live lease) and the
// paired account.
type InstanceView struct {
	Instance
	Status    string // "" when no node runs it
	QRPending bool
	JID, LID  string
}

func ttlSeconds(d time.Duration) float64 { return d.Seconds() }

// HeartbeatNode records that a node is alive and where it is reachable.
func (s *Store) HeartbeatNode(ctx context.Context, node, address string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO wax_nodes (id, address, heartbeat_at) VALUES ($1, $2, now())
		ON CONFLICT (id) DO UPDATE SET address = excluded.address, heartbeat_at = now()`, node, address)
	if err != nil {
		return fmt.Errorf("store: heartbeat: %w", err)
	}
	return nil
}

// DeleteNode removes a node that shuts down.
func (s *Store) DeleteNode(ctx context.Context, node string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM wax_nodes WHERE id = $1`, node); err != nil {
		return fmt.Errorf("store: delete node: %w", err)
	}
	return nil
}

// ClusterSize returns the number of nodes that sent a heartbeat within
// alive, and the number of instances.
func (s *Store) ClusterSize(ctx context.Context, alive time.Duration) (nodes, instances int, err error) {
	err = s.db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM wax_nodes WHERE heartbeat_at > now() - make_interval(secs => $1)),
		(SELECT count(*) FROM wax_instances)`, ttlSeconds(alive)).Scan(&nodes, &instances)
	if err != nil {
		return 0, 0, fmt.Errorf("store: cluster size: %w", err)
	}
	return nodes, instances, nil
}

// RenewLeases extends every unexpired lease of node and returns the
// instances it still owns. An expired lease is never renewed: another node
// may already be taking it over.
func (s *Store) RenewLeases(ctx context.Context, node string, ttl time.Duration) ([]string, error) {
	rows, err := s.db.Query(ctx, `UPDATE wax_instance_leases SET expires_at = now() + make_interval(secs => $2)
		WHERE node_id = $1 AND expires_at > now() RETURNING instance_id`, node, ttlSeconds(ttl))
	if err != nil {
		return nil, fmt.Errorf("store: renew leases: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// AcquireLeases takes up to limit instances that have no live lease (only
// the instance named by only, if it is not ""). Instances already locked by
// another node's acquisition are skipped, and a lease is only taken over
// once it has expired, so two nodes never own the same instance.
func (s *Store) AcquireLeases(ctx context.Context, node, address string, ttl time.Duration, limit int, only string) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		WITH candidates AS (
			SELECT i.id FROM wax_instances i
			LEFT JOIN wax_instance_leases l ON l.instance_id = i.id
			WHERE (l.instance_id IS NULL OR l.expires_at <= now()) AND ($5 = '' OR i.id = $5)
			ORDER BY i.id
			LIMIT $4
			FOR UPDATE OF i SKIP LOCKED
		)
		INSERT INTO wax_instance_leases (instance_id, node_id, address, expires_at)
		SELECT id, $1, $2, now() + make_interval(secs => $3) FROM candidates
		ON CONFLICT (instance_id) DO UPDATE
			SET node_id = excluded.node_id, address = excluded.address, expires_at = excluded.expires_at,
			    status = 'starting', qr_pending = false
			WHERE wax_instance_leases.expires_at <= now()
		RETURNING instance_id`, node, address, ttlSeconds(ttl), limit, only)
	if err != nil {
		return nil, fmt.Errorf("store: acquire leases: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ReleaseLeases gives up node's leases on the given instances.
func (s *Store) ReleaseLeases(ctx context.Context, node string, ids []string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM wax_instance_leases WHERE node_id = $1 AND instance_id = ANY($2)`, node, ids); err != nil {
		return fmt.Errorf("store: release leases: %w", err)
	}
	return nil
}

// PublishLeaseStates stores the owner's view of its instances in one
// round trip. Rows of leases the node no longer holds are left alone.
func (s *Store) PublishLeaseStates(ctx context.Context, node string, states map[string]LeaseState) error {
	if len(states) == 0 {
		return nil
	}
	return s.Batch(ctx, func(b *Store) error {
		for id, st := range states {
			if _, err := b.db.Exec(ctx, `UPDATE wax_instance_leases SET status = $3, qr_pending = $4 WHERE instance_id = $1 AND node_id = $2`,
				id, node, st.Status, st.QRPending); err != nil {
				return err
			}
		}
		return nil
	})
}

// LeaseOwner returns the node holding a live lease on an instance, or
// ErrNotFound when no node runs it.
func (s *Store) LeaseOwner(ctx context.Context, instanceID string) (*Lease, error) {
	var l Lease
	err := s.db.QueryRow(ctx, `SELECT node_id, address FROM wax_instance_leases WHERE instance_id = $1 AND expires_at > now()`,
		instanceID).Scan(&l.Node, &l.Address)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: lease owner: %w", err)
	}
	return &l, nil
}

// InstanceViews lists a tenant's instances (only the one named name, if it
// is not ""), with the state published by their owners.
func (s *Store) InstanceViews(ctx context.Context, tenant, name string) ([]InstanceView, error) {
	rows, err := s.db.Query(ctx, `
		SELECT i.id, i.name, i.webhook_url, i.rate_per_minute, i.created_at,
		       COALESCE(l.status, ''), COALESCE(l.qr_pending, false), d.jid, d.lid
		FROM wax_instances i
		LEFT JOIN wax_instance_leases l ON l.instance_id = i.id AND l.expires_at > now()
		LEFT JOIN wax_devices d ON d.instance_id = i.id
		WHERE i.tenant_id = $1 AND ($2 = '' OR i.name = $2)
		ORDER BY i.name`, tenant, name)
	if err != nil {
		return nil, fmt.Errorf("store: instance views: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (InstanceView, error) {
		v := InstanceView{Instance: Instance{TenantID: tenant}}
		var jid, lid *string
		err := r.Scan(&v.ID, &v.Name, &v.WebhookURL, &v.RatePerMinute, &v.CreatedAt, &v.Status, &v.QRPending, &jid, &lid)
		if jid != nil {
			v.JID = *jid
		}
		if lid != nil {
			v.LID = *lid
		}
		return v, err
	})
}

// QueueDepth is the number of pending items of an instance's queues.
type QueueDepth struct {
	Outbox, Webhooks int
}

// QueueDepths returns the pending outbox and webhook items of the given
// instances in one query (instances with empty queues are absent).
func (s *Store) QueueDepths(ctx context.Context, ids []string) (map[string]QueueDepth, error) {
	rows, err := s.db.Query(ctx, `
		SELECT instance_id, sum(o), sum(w) FROM (
			SELECT instance_id, 1 AS o, 0 AS w FROM wax_outbox WHERE status = 'pending' AND instance_id = ANY($1)
			UNION ALL
			SELECT instance_id, 0, 1 FROM wax_webhook_queue WHERE status = 'pending' AND instance_id = ANY($1)
		) q GROUP BY instance_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: queue depths: %w", err)
	}
	defer rows.Close()
	out := map[string]QueueDepth{}
	for rows.Next() {
		var id string
		var d QueueDepth
		if err := rows.Scan(&id, &d.Outbox, &d.Webhooks); err != nil {
			return nil, err
		}
		out[id] = d
	}
	return out, rows.Err()
}

// PoolStats reports the database connection pool.
type PoolStats struct {
	Max, Total, Acquired, Idle int
}

// Pool returns current connection pool statistics.
func (s *Store) Pool() PoolStats {
	st := s.pool.Stat()
	return PoolStats{Max: int(st.MaxConns()), Total: int(st.TotalConns()), Acquired: int(st.AcquiredConns()), Idle: int(st.IdleConns())}
}

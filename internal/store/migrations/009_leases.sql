-- Nodes of a multi-node deployment announce themselves here.
CREATE TABLE wax_nodes (
    id           TEXT PRIMARY KEY,
    address      TEXT NOT NULL,           -- where other nodes reach its API
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Which node runs an instance, until when. Times are database time
-- (now()), so node clocks never matter. The owner also publishes the
-- instance's state, so any node can answer listings.
CREATE TABLE wax_instance_leases (
    instance_id TEXT PRIMARY KEY REFERENCES wax_instances (id) ON DELETE CASCADE,
    node_id     TEXT        NOT NULL,
    address     TEXT        NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'starting',
    qr_pending  BOOLEAN     NOT NULL DEFAULT false
);
CREATE INDEX wax_instance_leases_node ON wax_instance_leases (node_id);

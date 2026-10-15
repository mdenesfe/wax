-- Instance configuration (an instance exists before it is paired, so this
-- is separate from wax_devices).
CREATE TABLE wax_instances (
    id              TEXT PRIMARY KEY,
    webhook_url     TEXT    NOT NULL DEFAULT '',
    webhook_secret  BYTEA,            -- sealed
    rate_per_minute INT     NOT NULL DEFAULT 20,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Outgoing messages. A row is created by the API before anything is sent,
-- so a crash never loses an accepted message; the WhatsApp message id is
-- fixed at enqueue time so retries cannot duplicate it.
CREATE TABLE wax_outbox (
    id              BIGSERIAL PRIMARY KEY,
    instance_id     TEXT    NOT NULL REFERENCES wax_instances (id) ON DELETE CASCADE,
    client_id       TEXT,             -- caller's idempotency key
    chat            TEXT    NOT NULL,
    message_id      TEXT    NOT NULL,
    payload         BYTEA   NOT NULL, -- sealed serialized wae2e.Message or media request
    kind            TEXT    NOT NULL, -- message | media
    status          TEXT    NOT NULL DEFAULT 'pending', -- pending | sent | failed
    attempts        INT     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT    NOT NULL DEFAULT '',
    server_ts       TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX wax_outbox_client_id ON wax_outbox (instance_id, client_id) WHERE client_id IS NOT NULL;
CREATE INDEX wax_outbox_due ON wax_outbox (instance_id, next_attempt_at) WHERE status = 'pending';

-- Webhook deliveries, written before any attempt so events survive crashes.
CREATE TABLE wax_webhook_queue (
    id              BIGSERIAL PRIMARY KEY,
    instance_id     TEXT    NOT NULL REFERENCES wax_instances (id) ON DELETE CASCADE,
    event           TEXT    NOT NULL,
    payload         BYTEA   NOT NULL, -- sealed JSON body
    status          TEXT    NOT NULL DEFAULT 'pending', -- pending | delivered | dead
    attempts        INT     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT    NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX wax_webhook_due ON wax_webhook_queue (instance_id, id) WHERE status = 'pending';

-- Devices may now exist only for registered instances; older rows predate
-- this table, so the constraint is not enforced retroactively.

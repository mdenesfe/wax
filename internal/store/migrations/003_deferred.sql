-- Stanzas we cannot process yet (e.g. group messages before sender-key
-- support). They are stored verbatim BEFORE being acknowledged, so the
-- server's offline queue keeps flowing (it delivers in windows and stalls
-- on unacknowledged stanzas) without losing anything. A later version
-- replays them in id order.
CREATE TABLE wax_deferred_stanzas (
    id           BIGSERIAL PRIMARY KEY,
    instance_id  TEXT  NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    tag          TEXT  NOT NULL,
    stanza_id    TEXT  NOT NULL,
    reason       TEXT  NOT NULL,
    raw          BYTEA NOT NULL, -- sealed binary-encoded node
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (instance_id, tag, stanza_id)
);

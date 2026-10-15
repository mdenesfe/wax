-- App state sync keys shared by the primary device.
CREATE TABLE wax_appstate_keys (
    instance_id  TEXT   NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    key_id       BYTEA  NOT NULL,
    key_data     BYTEA  NOT NULL, -- sealed
    fingerprint  BYTEA,
    key_ts       BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (instance_id, key_id)
);

-- Per-collection version and LTHash.
CREATE TABLE wax_appstate_versions (
    instance_id  TEXT   NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    collection   TEXT   NOT NULL,
    version      BIGINT NOT NULL,
    hash         BYTEA  NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, collection)
);

-- index MAC -> value MAC of every live record (needed to update the LTHash).
CREATE TABLE wax_appstate_macs (
    instance_id  TEXT  NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    collection   TEXT  NOT NULL,
    index_mac    BYTEA NOT NULL,
    value_mac    BYTEA NOT NULL,
    PRIMARY KEY (instance_id, collection, index_mac)
);

-- Contacts (from app state and history sync).
CREATE TABLE wax_contacts (
    instance_id   TEXT NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    jid           TEXT NOT NULL,
    first_name    TEXT NOT NULL DEFAULT '',
    full_name     TEXT NOT NULL DEFAULT '',
    push_name     TEXT NOT NULL DEFAULT '',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, jid)
);

-- Chats with their settings (app state) and history metadata.
CREATE TABLE wax_chats (
    instance_id      TEXT    NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    jid              TEXT    NOT NULL,
    name             TEXT    NOT NULL DEFAULT '',
    last_message_at  TIMESTAMPTZ,
    unread_count     INT     NOT NULL DEFAULT 0,
    archived         BOOLEAN NOT NULL DEFAULT false,
    pinned           BOOLEAN NOT NULL DEFAULT false,
    muted_until      TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, jid)
);

-- Messages (live and from history sync). Content is a sealed wae2e.Message.
CREATE TABLE wax_messages (
    instance_id  TEXT    NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    chat         TEXT    NOT NULL,
    message_id   TEXT    NOT NULL,
    sender       TEXT    NOT NULL DEFAULT '',
    from_me      BOOLEAN NOT NULL DEFAULT false,
    sent_at      TIMESTAMPTZ NOT NULL,
    push_name    TEXT    NOT NULL DEFAULT '',
    type         TEXT    NOT NULL DEFAULT '',
    content      BYTEA,
    source       TEXT    NOT NULL DEFAULT 'live', -- live | history
    PRIMARY KEY (instance_id, chat, message_id)
);
CREATE INDEX wax_messages_chat_time ON wax_messages (instance_id, chat, sent_at DESC);

-- History sync notifications waiting to be downloaded and processed.
CREATE TABLE wax_history_queue (
    id           BIGSERIAL PRIMARY KEY,
    instance_id  TEXT  NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    notification BYTEA NOT NULL, -- sealed wae2e.HistorySyncNotification
    attempts     INT   NOT NULL DEFAULT 0,
    last_error   TEXT  NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Our own NCT salt (for cstoken, later) and other singletons.
ALTER TABLE wax_devices ADD COLUMN nct_salt BYTEA;

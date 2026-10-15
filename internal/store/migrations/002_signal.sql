-- Signal sessions, one row per remote device address ("user[_agent].device").
CREATE TABLE wax_sessions (
    instance_id  TEXT  NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    address      TEXT  NOT NULL,
    record       BYTEA NOT NULL, -- sealed serialized signal.Record
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, address)
);

-- Phone number user <-> LID user mapping, learned from the server.
CREATE TABLE wax_lid_map (
    instance_id  TEXT NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    pn_user      TEXT NOT NULL,
    lid_user     TEXT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, pn_user)
);
CREATE INDEX wax_lid_map_lid ON wax_lid_map (instance_id, lid_user);

-- Decrypted incoming payloads, keyed by a hash of the ciphertext. Written in
-- the same transaction as the session update, so a crash between decrypting
-- and acknowledging cannot lose a message: the redelivered ciphertext finds
-- its plaintext here. plaintext is cleared once the message was handled; the
-- row then only serves to drop duplicates, and old rows are purged.
CREATE TABLE wax_event_buffer (
    instance_id  TEXT  NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    hash         BYTEA NOT NULL,
    plaintext    BYTEA,          -- sealed; NULL once handled
    server_ts    TIMESTAMPTZ,
    inserted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, hash)
);
CREATE INDEX wax_event_buffer_age ON wax_event_buffer (inserted_at);

-- Recently sent messages, kept to answer retry receipts (a recipient that
-- could not decrypt asks us to re-encrypt and resend).
CREATE TABLE wax_sent_messages (
    instance_id  TEXT  NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    chat         TEXT  NOT NULL,
    message_id   TEXT  NOT NULL,
    plaintext    BYTEA NOT NULL, -- sealed serialized wae2e.Message
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, chat, message_id)
);
CREATE INDEX wax_sent_messages_age ON wax_sent_messages (created_at);

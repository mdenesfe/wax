-- Persistent identity of each instance (one linked device per instance).
-- Columns marked "sealed" hold AES-GCM ciphertext (see sealer.go).
CREATE TABLE wax_devices (
    instance_id        TEXT PRIMARY KEY,
    registration_id    BIGINT  NOT NULL,
    noise_key          BYTEA   NOT NULL, -- sealed X25519 private key
    identity_seed      BYTEA   NOT NULL, -- sealed signing key seed
    signed_prekey_id   BIGINT  NOT NULL,
    signed_prekey      BYTEA   NOT NULL, -- sealed X25519 private key
    signed_prekey_sig  BYTEA   NOT NULL,
    adv_secret         BYTEA   NOT NULL, -- sealed
    next_prekey_id     BIGINT  NOT NULL DEFAULT 1,

    -- Filled in by pairing; NULL while unpaired.
    jid                TEXT,
    lid                TEXT,
    account            BYTEA,            -- ADVSignedDeviceIdentity incl. our device signature
    platform           TEXT    NOT NULL DEFAULT '',
    business_name      TEXT    NOT NULL DEFAULT '',
    push_name          TEXT    NOT NULL DEFAULT '',
    paired_at          TIMESTAMPTZ,

    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One-time prekeys. uploaded=false keys are generated but not yet on the server.
CREATE TABLE wax_prekeys (
    instance_id  TEXT    NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    key_id       BIGINT  NOT NULL,
    private_key  BYTEA   NOT NULL, -- sealed
    uploaded     BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, key_id)
);

-- Known identity keys of remote devices (trust-on-first-use), keyed by
-- Signal address. Pairing stores the primary device's identity here.
CREATE TABLE wax_identities (
    instance_id   TEXT  NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    address       TEXT  NOT NULL,
    identity_key  BYTEA NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, address)
);

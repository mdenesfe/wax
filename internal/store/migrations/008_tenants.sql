-- Tenants: every instance belongs to one, and API keys are per tenant.
CREATE TABLE wax_tenants (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Existing instances move to a "default" tenant. Their ids stay as they are:
-- sealed columns bind the instance id, so ids can never be renamed.
INSERT INTO wax_tenants (id, name)
SELECT 'default', 'default' WHERE EXISTS (SELECT 1 FROM wax_instances);

-- name is the id shown through the API, unique per tenant; id remains the
-- internal key ("tenant:name" for new instances).
ALTER TABLE wax_instances ADD COLUMN tenant_id TEXT REFERENCES wax_tenants (id);
ALTER TABLE wax_instances ADD COLUMN name TEXT;
UPDATE wax_instances SET tenant_id = 'default', name = id;
ALTER TABLE wax_instances ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE wax_instances ALTER COLUMN name SET NOT NULL;
CREATE UNIQUE INDEX wax_instances_tenant_name ON wax_instances (tenant_id, name);

-- API keys: only an HMAC of the secret part is stored (keyed with a key
-- derived from WAX_MASTER_KEY), so a database dump does not reveal keys.
CREATE TABLE wax_api_keys (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT  NOT NULL REFERENCES wax_tenants (id) ON DELETE CASCADE,
    mac        BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX wax_api_keys_tenant ON wax_api_keys (tenant_id);

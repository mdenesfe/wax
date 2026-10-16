-- Per-tenant limits; 0 means "no quota" / "use the server default".
ALTER TABLE wax_tenants ADD COLUMN max_instances       INT NOT NULL DEFAULT 0;
ALTER TABLE wax_tenants ADD COLUMN requests_per_second INT NOT NULL DEFAULT 0;

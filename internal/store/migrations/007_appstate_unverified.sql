-- Collections whose server LTHash cannot be verified from the delivered
-- records (see appstate.HashState).
ALTER TABLE wax_appstate_versions ADD COLUMN unverified BOOLEAN NOT NULL DEFAULT false;

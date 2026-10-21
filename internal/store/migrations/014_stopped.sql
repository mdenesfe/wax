-- A stopped instance keeps its device and data but runs on no node until
-- it is connected again.
ALTER TABLE wax_instances ADD COLUMN stopped BOOLEAN NOT NULL DEFAULT false;

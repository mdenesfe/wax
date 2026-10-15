-- Sender key chains per (group, sender address). Our own chain for a group
-- is stored under our own address and includes the signing key.
CREATE TABLE wax_sender_keys (
    instance_id  TEXT  NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    group_jid    TEXT  NOT NULL,
    sender       TEXT  NOT NULL, -- signal address of the sender device
    record       BYTEA NOT NULL, -- sealed signal.SenderKeyRecord
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, group_jid, sender)
);

-- Devices that already received our current sender key for a group, so the
-- distribution message is only sent to new devices. Cleared when our sender
-- key for the group is replaced.
CREATE TABLE wax_sender_key_sent (
    instance_id  TEXT NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    group_jid    TEXT NOT NULL,
    device       TEXT NOT NULL, -- signal address
    PRIMARY KEY (instance_id, group_jid, device)
);

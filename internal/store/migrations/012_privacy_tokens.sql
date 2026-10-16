-- Privacy ("trusted contact") tokens. A contact's token, sent along with
-- profile and picture queries and messages, lets its "my contacts" privacy
-- settings treat us as a contact. token is sealed. sent_at is when we last
-- issued our own token to that user.
CREATE TABLE wax_privacy_tokens (
    instance_id TEXT        NOT NULL REFERENCES wax_devices (instance_id) ON DELETE CASCADE,
    user_jid    TEXT        NOT NULL,
    token       BYTEA,
    token_at    TIMESTAMPTZ,
    sent_at     TIMESTAMPTZ,
    PRIMARY KEY (instance_id, user_jid)
);

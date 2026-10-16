-- Edited messages keep their id; edited_at marks them.
ALTER TABLE wax_messages ADD COLUMN edited_at TIMESTAMPTZ;

-- When the chat settings (archive, pin, mute) last came from app state.
-- History sync, which can be older, does not override them after that.
ALTER TABLE wax_chats ADD COLUMN settings_at TIMESTAMPTZ;

-- Sends keep their order per chat: a chat's oldest pending item goes first.
CREATE INDEX wax_outbox_chat_pending ON wax_outbox (instance_id, chat, id) WHERE status = 'pending';

-- WhatsApp usernames (@name) of contacts, from the address book in app
-- state (contact and lid_contact actions).
ALTER TABLE wax_contacts ADD COLUMN username TEXT NOT NULL DEFAULT '';

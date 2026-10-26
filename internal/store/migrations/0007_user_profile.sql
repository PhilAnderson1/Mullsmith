-- Settings page (spec §5): per-user account details. default_system_prompt
-- seeds NEW conversations created without an explicit prompt; conversations
-- keep their own copy once created, so editing the default later never
-- rewrites an existing chat's behaviour (the conversation column is the
-- record of what that chat was told).
ALTER TABLE users ADD COLUMN default_system_prompt TEXT NOT NULL DEFAULT '';

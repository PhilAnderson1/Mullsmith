-- 0008: email confirmation + password reset (email_features plan §3).
-- +no-foreign-keys
--
-- users.email becomes nullable: a never-confirmed sign-up holds its only
-- address in unconfirmed_email until the confirm link is clicked. A ''
-- placeholder is not an option — the UNIQUE index would collide on the
-- second pending sign-up (NULLs are distinct under UNIQUE, '' is not) —
-- so this is a full table rebuild. The runner honours the
-- "+no-foreign-keys" marker above by disabling FK enforcement on a pinned
-- connection OUTSIDE the transaction: enforcement inside a tx is a no-op
-- in SQLite, and DROP TABLE users would otherwise fire sessions'
-- ON DELETE CASCADE and silently delete every session (verified against
-- this app's exact DSN, modernc sqlite v1.61).
--
-- Upgrade-compatibility rule: NO statement here UPDATEs existing users —
-- enabling the feature later never touches existing accounts.

CREATE TABLE users_new (
  id                    TEXT PRIMARY KEY,
  email                 TEXT UNIQUE COLLATE NOCASE,  -- NULL until confirmed
  name                  TEXT NOT NULL,
  password_hash         TEXT NOT NULL,
  status                TEXT NOT NULL DEFAULT 'active',
  default_system_prompt TEXT NOT NULL DEFAULT '',
  unconfirmed_email     TEXT,                        -- NULL = nothing pending
  created_at            INTEGER NOT NULL,
  updated_at            INTEGER NOT NULL
);
INSERT INTO users_new (id, email, name, password_hash, status,
                       default_system_prompt, created_at, updated_at)
  SELECT id, email, name, password_hash, status,
         default_system_prompt, created_at, updated_at FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;

-- One-shot tokens for confirmation and reset links. Only the sha256 of the
-- raw URL token is ever stored (same doctrine as sessions.id_hash). "email"
-- is the address the mail went to: a confirm_email token authenticates
-- control of THAT mailbox, so confirmation only applies while it still
-- matches the account's pending target.
CREATE TABLE auth_tokens (
  kind       TEXT NOT NULL,             -- 'confirm_email' | 'reset_password'
  token_hash BLOB NOT NULL,             -- sha256 of raw token; raw never stored
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  email      TEXT NOT NULL,             -- address the mail went to
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER,
  PRIMARY KEY (kind, token_hash)
);
CREATE INDEX idx_authtokens_expiry ON auth_tokens(expires_at);

-- Current application schema. Development databases are initialized directly;
-- changes to this file do not upgrade an older schema. Recreate a development
-- database when its structure changes. IF NOT EXISTS makes startup repeatable.

CREATE TABLE IF NOT EXISTS users (
  id                    TEXT PRIMARY KEY,
  email                 TEXT UNIQUE COLLATE NOCASE, -- NULL until confirmed
  name                  TEXT NOT NULL,
  password_hash         TEXT NOT NULL,
  status                TEXT NOT NULL DEFAULT 'enabled', -- enabled | unconfirmed | disabled
  default_system_prompt TEXT NOT NULL DEFAULT '',
  unconfirmed_email     TEXT, -- NULL = no pending address
  created_at            INTEGER NOT NULL,
  updated_at            INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  id_hash      BLOB PRIMARY KEY, -- sha256 of the cookie token
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  remembered   INTEGER NOT NULL DEFAULT 0,
  last_seen_at INTEGER NOT NULL,
  user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS invites (
  code       TEXT PRIMARY KEY,
  created_by TEXT,
  used_by    TEXT,
  created_at INTEGER NOT NULL,
  used_at    INTEGER
);

-- Tokens are one-shot. Store only hashes, never the raw link token.
CREATE TABLE IF NOT EXISTS auth_tokens (
  kind       TEXT NOT NULL, -- confirm_email | reset_password
  token_hash BLOB NOT NULL,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  email      TEXT NOT NULL, -- the address the token authenticates
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER,
  PRIMARY KEY (kind, token_hash)
);
CREATE INDEX IF NOT EXISTS idx_authtokens_expiry ON auth_tokens(expires_at);

CREATE TABLE IF NOT EXISTS conversations (
  id            TEXT PRIMARY KEY,
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title         TEXT NOT NULL DEFAULT 'New chat',
  model         TEXT NOT NULL DEFAULT '', -- '' = server default
  system_prompt TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL, -- bumped when a message is posted
  thinking      TEXT NOT NULL DEFAULT '', -- '' = default thinking choice
  thinking_ctx  TEXT NOT NULL DEFAULT '' -- applicability fingerprint
);
CREATE INDEX IF NOT EXISTS idx_conv_user_updated ON conversations(user_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS messages (
  id                TEXT PRIMARY KEY,
  conversation_id   TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  role              TEXT NOT NULL, -- user | assistant | system | tool
  content           TEXT NOT NULL, -- empty while streaming or proposing tools
  status            TEXT NOT NULL DEFAULT 'complete',
  token_count       INTEGER,
  created_at        INTEGER NOT NULL,
  tool_call_id      TEXT NOT NULL DEFAULT '', -- model protocol ID for a tool result
  completion_tokens INTEGER,
  duration_ms       INTEGER, -- model round only, excluding tool execution
  synthetic         INTEGER NOT NULL DEFAULT 0, -- server-generated image-view turn
  generation_id     TEXT NOT NULL DEFAULT '', -- owns assistant rounds, tool results and synthetic turns
  status_note       TEXT NOT NULL DEFAULT '' -- durable terminal explanation
);
CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(conversation_id, created_at);

CREATE TABLE IF NOT EXISTS tool_calls (
  id          TEXT PRIMARY KEY,
  message_id  TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  llm_call_id TEXT NOT NULL DEFAULT '',
  tool        TEXT NOT NULL,
  arguments   TEXT NOT NULL,
  status      TEXT NOT NULL,
  result      TEXT, -- bounded model-facing output
  duration_ms INTEGER,
  created_at  INTEGER NOT NULL,
  finished_at INTEGER,
  display     TEXT NOT NULL DEFAULT '' -- bounded card JSON shared by live/history views
);
CREATE INDEX IF NOT EXISTS idx_tool_calls_message ON tool_calls(message_id);

-- The disk owns file state. These links record which uploads accompanied a
-- message; deleting a conversation removes the links, not the upload bytes.
CREATE TABLE IF NOT EXISTS attachments (
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  path       TEXT NOT NULL,
  PRIMARY KEY (message_id, path)
);

-- 0002: conversations + messages (spec §4).
CREATE TABLE conversations (
  id            TEXT PRIMARY KEY,
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title         TEXT NOT NULL DEFAULT 'New chat',
  model         TEXT NOT NULL DEFAULT '',      -- '' = server default model
  system_prompt TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,              -- bumped on new message (§6.1)
  deleted_at    INTEGER                        -- soft delete
);
CREATE INDEX idx_conv_user_updated ON conversations(user_id, deleted_at, updated_at DESC);

CREATE TABLE messages (
  id              TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  role            TEXT NOT NULL,               -- user | assistant | system | tool
  content         TEXT NOT NULL,               -- markdown/text; '' allowed mid-tool-loop
  status          TEXT NOT NULL DEFAULT 'complete', -- streaming|complete|error|aborted
  token_count     INTEGER,                     -- estimate, for context budgeting
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_messages_conv ON messages(conversation_id, created_at);

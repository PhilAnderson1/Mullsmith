-- M5: scratchpad file metadata (spec §4). Files are physical objects under
-- scratchpad/<user-id>/; this table is the authoritative index — kind upload
-- rows are user uploads (stored name is the file id, §9.4), artifact rows are
-- model-shared files whose rel_path the fs tools chose. Attachments join
-- messages to the uploads they carry.

CREATE TABLE files (
  id              TEXT PRIMARY KEY,
  user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  conversation_id TEXT,                  -- nullable provenance
  kind            TEXT NOT NULL,         -- upload | artifact
  rel_path        TEXT NOT NULL,         -- relative to scratchpad/<user-id>/
  media_type      TEXT NOT NULL DEFAULT 'application/octet-stream',
  name            TEXT NOT NULL,         -- display filename only, never a path
  size            INTEGER NOT NULL,
  sha256          BLOB NOT NULL,
  created_at      INTEGER NOT NULL,
  expires_at      INTEGER                -- artifacts get TTL; uploads NULL
);

CREATE INDEX idx_files_user ON files(user_id, kind, created_at DESC, id DESC);
CREATE INDEX idx_files_expiry ON files(kind, expires_at);

CREATE TABLE attachments (
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  file_id    TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  PRIMARY KEY (message_id, file_id)
);

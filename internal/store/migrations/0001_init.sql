-- 0001: users, sessions, invites (spec §4).
CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
  name          TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  status        TEXT NOT NULL DEFAULT 'active',   -- active | disabled
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
  id_hash      BLOB PRIMARY KEY,           -- sha256 of cookie token
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,           -- absolute cap; slid on renewal (§9.1)
  remembered   INTEGER NOT NULL DEFAULT 0, -- 1 = "stay logged in" persistent cookie
  last_seen_at INTEGER NOT NULL,           -- drives throttled sliding renewal (§9.1)
  user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_sessions_user    ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

CREATE TABLE invites (
  code       TEXT PRIMARY KEY,
  created_by TEXT,
  used_by    TEXT,
  created_at INTEGER NOT NULL,
  used_at    INTEGER
);

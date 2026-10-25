-- M4: tool-call audit trail and tool-result replay into model context
-- (spec §4, §7.1). messages.tool_call_id links role='tool' result rows to
-- the id the LLM assigned; tool_calls rows link to the assistant message
-- that proposed the call and carry the truncated result sent back to the
-- model.

CREATE TABLE tool_calls (
  id          TEXT PRIMARY KEY,
  message_id  TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  llm_call_id TEXT NOT NULL DEFAULT '',
  tool        TEXT NOT NULL,
  arguments   TEXT NOT NULL,
  status      TEXT NOT NULL,           -- pending_approval|approved|denied|running|ok|error
  result      TEXT,
  duration_ms INTEGER,
  created_at  INTEGER NOT NULL,
  finished_at INTEGER
);

CREATE INDEX idx_tool_calls_message ON tool_calls(message_id);

ALTER TABLE messages ADD COLUMN tool_call_id TEXT NOT NULL DEFAULT '';

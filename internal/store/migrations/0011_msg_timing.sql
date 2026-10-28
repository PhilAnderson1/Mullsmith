-- Per-message generation stats for the UI (context meter + tok/s readout).
-- completion_tokens is what the model produced this round (upstream
-- usage.completion_tokens, else an estimate — same doctrine as token_count);
-- duration_ms is the wall time the model round took, excluding tool runs.
-- Both NULL on rows predating this migration and on user/tool rows.
ALTER TABLE messages ADD COLUMN completion_tokens INTEGER;
ALTER TABLE messages ADD COLUMN duration_ms INTEGER;

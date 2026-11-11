-- Preserve endpoint-neutral assistant reasoning for compatible history replay.
ALTER TABLE messages ADD COLUMN reasoning_data TEXT NOT NULL DEFAULT '';

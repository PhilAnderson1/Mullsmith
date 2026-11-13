-- Separate user-visible answer latency from active token generation time.
ALTER TABLE messages ADD COLUMN first_answer_ms INTEGER;
ALTER TABLE messages ADD COLUMN generation_ms INTEGER;

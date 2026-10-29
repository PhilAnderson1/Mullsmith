-- Thinking-level selector (thinking-control-specification.md §5). The
-- choice is conversation state, not a global LLM setting: '' (and NULL,
-- pre-migration rows) means the built-in Default, which sends no
-- thinking override — so every existing conversation keeps behaving
-- exactly as before.
ALTER TABLE conversations ADD COLUMN thinking TEXT NOT NULL DEFAULT '';

-- Applicability context for the saved choice: a fingerprint of endpoint
-- URL, model ID, endpoint type and profile ID taken when the choice was
-- stored (never the API key). A mismatch — the operator repointed the
-- endpoint or the conversation picked another model — resets the
-- restored choice visibly to Default instead of silently applying a
-- choice that belongs to a different model.
ALTER TABLE conversations ADD COLUMN thinking_ctx TEXT NOT NULL DEFAULT '';

-- C7: make the live view reproducible from stored rows (three additive
-- columns, no backfill). The current renderer uses these fields directly;
-- historical card and turn inference is not supported.
--
-- 1) tool_calls.display is the DISPLAY payload: one bounded JSON object
--    {"summary": "...", "data": ...} holding exactly what the live
--    tool_end event carried. It used to be that only the bounded,
--    MIDDLE-ELIDED model-facing `result` was stored, and the reload path
--    re-parsed that string to rebuild cards — cutting JSON mid-structure
--    lost links and download cards. `result` keeps its old meaning; the
--    two are deliberately separate (spec §6.1).
ALTER TABLE tool_calls ADD COLUMN display TEXT NOT NULL DEFAULT '';

-- 2) messages.generation_id names the generation that produced an
--    assistant round. One user turn can span several model rounds and so
--    several rows; regenerate needs the TURN, not the single row it was
--    aimed at, or a retry quietly inherits the retired attempt's own tool
--    transcript from history.
ALTER TABLE messages ADD COLUMN generation_id TEXT NOT NULL DEFAULT '';

-- 3) messages.status_note is the server-authored terminal note a reload
--    needs to show what the live stream already announced: a generation
--    that died after its last round had SAVED cleanly (a failed next-round
--    placeholder, a tool-result write failure, a mid-loop overflow) used
--    to describe itself only in the live `done` event, so the failure
--    vanished on refresh while the transcript stayed `complete`.
ALTER TABLE messages ADD COLUMN status_note TEXT NOT NULL DEFAULT '';

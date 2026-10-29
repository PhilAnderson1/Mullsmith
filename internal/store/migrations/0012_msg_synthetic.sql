-- Rows the server inserted into a thread on its own behalf, not typed by
-- the human: today only the synthetic view_image turn (spec §7.2), which
-- must be role 'user' to carry its image on replay but should never
-- render as a "You" bubble. The client filters on this flag instead of
-- sniffing the content prefix. 0 (and NULL, pre-migration rows) = genuine.
ALTER TABLE messages ADD COLUMN synthetic INTEGER NOT NULL DEFAULT 0;

-- Rows written before the flag existed are identified by the note
-- prefix their creator always used; without this they would render as
-- "You" bubbles and lose their image on replay.
UPDATE messages SET synthetic = 1
 WHERE role = 'user' AND content LIKE '[view_image attached %';

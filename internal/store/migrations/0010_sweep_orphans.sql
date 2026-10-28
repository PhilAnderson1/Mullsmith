-- Sweep orphaned rows left by OUT-OF-BAND writes. The app's pool enforces
-- foreign_keys=1 and cascades verified end-to-end; but the sqlite3 CLI has
-- enforcement OFF by default, and hand-run DELETEs from it bypass FK rules
-- entirely — leaving children pointing at vanished parents (observed after
-- manual surgery: messages whose conversation row was CLI-deleted, and
-- tool_calls whose parent messages were CLI-deleted).
--
-- Orphans are unreachable by every app query and are pure dead weight.
-- Leaf tables first so message deletions cannot mint fresh orphans.
DELETE FROM attachments WHERE message_id NOT IN (SELECT id FROM messages);
DELETE FROM tool_calls WHERE message_id NOT IN (SELECT id FROM messages);
DELETE FROM messages WHERE conversation_id NOT IN (SELECT id FROM conversations);

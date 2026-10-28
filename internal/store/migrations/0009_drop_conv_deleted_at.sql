-- Conversation "delete" is a real delete (owner decision, post-audit1):
-- the button hard-deletes and the FK cascades take messages, tool calls
-- and attachment links with it. The UI already confirms every click,
-- abandons in-flight generations first, and backups cover regret — the
-- soft-delete tombstone bought nothing but phantom rows in every count.
--
-- Rows already tombstoned were deleted by a user's click, so they die
-- with the concept (cascade takes their messages): without this step
-- dropping the column would RESURRECT them as live chats.
DELETE FROM conversations WHERE deleted_at IS NOT NULL;
-- deleted_at is indexed, and SQLite refuses to drop an indexed column,
-- so the sidebar index is rebuilt in its remaining shape.
DROP INDEX idx_conv_user_updated;
ALTER TABLE conversations DROP COLUMN deleted_at;
CREATE INDEX idx_conv_user_updated ON conversations(user_id, updated_at DESC);

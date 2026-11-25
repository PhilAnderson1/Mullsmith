ALTER TABLE conversations ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0, 1));
CREATE INDEX idx_conv_user_pinned_updated ON conversations(user_id, pinned DESC, updated_at DESC, id DESC);

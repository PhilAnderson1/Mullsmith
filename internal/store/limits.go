package store

import (
	"context"
	"database/sql"
	"errors"
)

var (
	ErrConversationLimit = errors.New("conversation limit reached")
	ErrMessageLimit      = errors.New("message limit reached")
)

const MessageLimitNotice = "message limit reached; delete older conversations to make room"

// WithRecordLimits returns an immutable policy view of the same database.
// It shares the original connection pool and lifetime; it must not be closed
// separately. Non-positive limits disable counting for that resource.
func (d *DB) WithRecordLimits(conversations, messages int) *DB {
	return &DB{sqlDB: d.sqlDB, maxConversationsPerUser: conversations, maxMessagesPerUser: messages}
}

const conversationLimitQuery = `SELECT COUNT(*) FROM (
 SELECT 1 FROM conversations WHERE user_id = ? LIMIT ?)`

// Both sides of the join are covered by indexes: conversations(user_id,id)
// and messages(conversation_id,created_at). No message text is read. LIMIT
// stops enumeration as soon as enough entries establish a refusal.
const messageLimitQuery = `SELECT COUNT(*) FROM (
 SELECT 1 FROM conversations c JOIN messages m ON m.conversation_id = c.id
 WHERE c.user_id = ? LIMIT ?)`

func (d *DB) checkConversationLimit(ctx context.Context, tx *sql.Tx, userID string) error {
	if d.maxConversationsPerUser <= 0 {
		return nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, conversationLimitQuery, userID, d.maxConversationsPerUser).Scan(&count); err != nil {
		return err
	}
	if count >= d.maxConversationsPerUser {
		return ErrConversationLimit
	}
	return nil
}

func (d *DB) checkMessageLimit(ctx context.Context, tx *sql.Tx, userID string, incoming int) error {
	if d.maxMessagesPerUser <= 0 {
		return nil
	}
	if incoming > d.maxMessagesPerUser {
		return ErrMessageLimit
	}
	threshold := d.maxMessagesPerUser - incoming + 1
	var count int
	if err := tx.QueryRowContext(ctx, messageLimitQuery, userID, threshold).Scan(&count); err != nil {
		return err
	}
	if count >= threshold {
		return ErrMessageLimit
	}
	return nil
}

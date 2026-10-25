package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"gabkeep/internal/id"
)

// Message is a messages-table row.
type Message struct {
	ID             string
	ConversationID string
	Role           string // user | assistant | system | tool
	Content        string
	Status         string // streaming | complete | error | aborted
	TokenCount     *int64
	CreatedAt      time.Time
}

// AddMessage inserts a message and bumps the conversation's updated_at in one
// transaction (the sidebar orders by it, spec §6.1 step 6). userID guards
// against writing into someone else's thread (spec §9.2).
func (d *DB) AddMessage(ctx context.Context, userID string, m *Message) error {
	if m.ID == "" {
		m.ID = id.New()
	}
	if m.Status == "" {
		m.Status = "complete"
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM conversations WHERE id = ? AND user_id = ? AND deleted_at IS NULL`,
		m.ConversationID, userID).Scan(&one)
	if errors.Is(err, ErrNoRows) {
		return ErrConversationNotFound
	}
	if err != nil {
		return err
	}

	var tok any
	if m.TokenCount != nil {
		tok = *m.TokenCount
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages(id, conversation_id, role, content, status, token_count, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ConversationID, m.Role, m.Content, m.Status, tok, m.CreatedAt.Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET updated_at = ? WHERE id = ?`,
		m.CreatedAt.Unix(), m.ConversationID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListMessages returns one page of a conversation's history in chronological
// order. The first page (cursor "") is the NEWEST limit messages; the next
// cursor walks further back in time (ChatGPT-style "load earlier"). next is
// "" when no older messages exist. Ownership is enforced.
func (d *DB) ListMessages(ctx context.Context, userID, convID, cursor string, limit int) (items []Message, next string, err error) {
	if _, err := d.ConversationForUser(ctx, userID, convID); err != nil {
		return nil, "", err
	}
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	sb := strings.Builder{}
	sb.WriteString(`SELECT id, conversation_id, role, content, status, token_count, created_at
	                 FROM messages WHERE conversation_id = ?`)
	args := []any{convID}
	if cursor != "" {
		ts, mid, ok := decodeCursor(cursor)
		if !ok {
			return nil, "", errors.New("invalid cursor")
		}
		sb.WriteString(` AND (created_at < ? OR (created_at = ? AND id < ?))`)
		args = append(args, ts, ts, mid)
	}
	sb.WriteString(` ORDER BY created_at DESC, id DESC LIMIT ?`)
	args = append(args, limit+1)

	rows, err := d.sqlDB.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			m       Message
			tok     sql.NullInt64
			created int64
		)
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Status, &tok, &created); err != nil {
			return nil, "", err
		}
		if tok.Valid {
			v := tok.Int64
			m.TokenCount = &v
		}
		m.CreatedAt = time.Unix(created, 0)
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	// Flip the DESC page into chronological order for the client.
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	if hasMore && len(items) > 0 {
		oldest := items[0]
		next = encodeCursor(oldest.CreatedAt.Unix(), oldest.ID)
	}
	return items, next, nil
}

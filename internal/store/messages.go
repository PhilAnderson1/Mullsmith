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

// GetMessageForUser fetches one message with ownership enforced
// (message → conversation → user, spec §9.2).
func (d *DB) GetMessageForUser(ctx context.Context, userID, msgID string) (*Message, error) {
	var (
		m       Message
		tok     sql.NullInt64
		created int64
	)
	err := d.sqlDB.QueryRowContext(ctx,
		`SELECT m.id, m.conversation_id, m.role, m.content, m.status, m.token_count, m.created_at
		 FROM messages m JOIN conversations c ON c.id = m.conversation_id
		 WHERE m.id = ? AND c.user_id = ? AND c.deleted_at IS NULL`,
		msgID, userID).
		Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Status, &tok, &created)
	if errors.Is(err, ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}
	if tok.Valid {
		v := tok.Int64
		m.TokenCount = &v
	}
	m.CreatedAt = time.Unix(created, 0)
	return &m, nil
}

// IsLastMessageInConversation reports whether msgID is the newest row.
func (d *DB) IsLastMessageInConversation(ctx context.Context, convID, msgID string) (bool, error) {
	var last string
	err := d.sqlDB.QueryRowContext(ctx,
		`SELECT id FROM messages WHERE conversation_id = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, convID).Scan(&last)
	if errors.Is(err, ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return last == msgID, nil
}

func (d *DB) DeleteMessage(ctx context.Context, msgID string) error {
	res, err := d.sqlDB.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, msgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// FinalizeMessage writes the produced content and status once a generation
// ends (complete | aborted | error).
func (d *DB) FinalizeMessage(ctx context.Context, msgID, content, status string, tokens int64) error {
	_, err := d.sqlDB.ExecContext(ctx,
		`UPDATE messages SET content = ?, status = ?, token_count = ? WHERE id = ?`,
		content, status, tokens, msgID)
	return err
}

// SweepStreaming marks rows orphaned by a crash as aborted; run at startup.
func (d *DB) SweepStreaming(ctx context.Context) (int64, error) {
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE messages SET status = 'aborted' WHERE status = 'streaming'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ContextHistory returns the conversation's completed messages (chronological,
// excluding excludeID) for prompt assembly. A generous row cap; the token
// budget in agent.BuildContext does the real trimming.
func (d *DB) ContextHistory(ctx context.Context, convID, excludeID string) ([]Message, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT id, conversation_id, role, content, status, token_count, created_at
		 FROM messages
		 WHERE conversation_id = ? AND id <> ? AND status = 'complete'
		 ORDER BY created_at ASC, id ASC LIMIT 4000`,
		convID, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var (
			m       Message
			tok     sql.NullInt64
			created int64
		)
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Status, &tok, &created); err != nil {
			return nil, err
		}
		if tok.Valid {
			v := tok.Int64
			m.TokenCount = &v
		}
		m.CreatedAt = time.Unix(created, 0)
		out = append(out, m)
	}
	return out, rows.Err()
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

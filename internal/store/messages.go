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
	// CompletionTokens and DurationMS are the generation stats of an
	// assistant reply (upstream usage + round wall time; nil when the
	// endpoint never reported and nothing was produced). UI readouts
	// (context meter, tok/s) project these; context replay ignores them.
	CompletionTokens *int64
	DurationMS       *int64
	CreatedAt        time.Time
	// Synthetic marks rows the server inserted on its own behalf — today
	// only the view_image turn (§7.2): role 'user' so its image replays,
	// but never a "You" bubble. The client filters on this flag.
	Synthetic bool
	// ToolCallID is set on role='tool' rows: the call this result answers.
	ToolCallID string
	// Calls holds the audit rows for assistant messages that proposed tool
	// calls (context replay + reload rendering). Read-only projection.
	Calls []ToolCall
	// Attachments holds the uploads a user message was sent with (context
	// manifest + UI chips). Read-only projection; see attachments.go.
	Attachments []Attachment
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
		`SELECT 1 FROM conversations WHERE id = ? AND user_id = ?`,
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
		`INSERT INTO messages(id, conversation_id, role, content, status, token_count, created_at, tool_call_id, synthetic)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ConversationID, m.Role, m.Content, m.Status, tok, m.CreatedAt.Unix(), m.ToolCallID, m.Synthetic); err != nil {
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
		 WHERE m.id = ? AND c.user_id = ?`,
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
// ends (complete | aborted | error). Non-positive completion/duration store
// NULL: an absent statistic is never faked with a zero.
func (d *DB) FinalizeMessage(ctx context.Context, msgID, content, status string, tokens, completion, durationMS int64) error {
	var comp, dur any
	if completion > 0 {
		comp = completion
	}
	if durationMS > 0 {
		dur = durationMS
	}
	_, err := d.sqlDB.ExecContext(ctx,
		`UPDATE messages SET content = ?, status = ?, token_count = ?, completion_tokens = ?, duration_ms = ? WHERE id = ?`,
		content, status, tokens, comp, dur, msgID)
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
// excluding excludeID) for prompt assembly and the overflow estimate. Every
// stored turn is returned (the row cap is a runaway guard, not a policy):
// the request is append-only and the context-full gate (server.ctxwindow)
// answers before a send would cross the window. Assistant rows carry
// their Calls and tool rows their ToolCallID so the agent can replay the
// OpenAI tool protocol faithfully.
func (d *DB) ContextHistory(ctx context.Context, convID, excludeID string) ([]Message, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT id, conversation_id, role, content, status, token_count, created_at, tool_call_id, synthetic
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
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Status, &tok, &created, &m.ToolCallID, &m.Synthetic); err != nil {
			return nil, err
		}
		if tok.Valid {
			v := tok.Int64
			m.TokenCount = &v
		}
		m.CreatedAt = time.Unix(created, 0)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := d.attachCalls(ctx, out); err != nil {
		return nil, err
	}
	if err := d.attachFiles(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachCalls fills .Calls on assistant messages that have tool_calls rows.
func (d *DB) attachCalls(ctx context.Context, msgs []Message) error {
	var ids []string
	for _, m := range msgs {
		if m.Role == "assistant" {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	byMsg, err := d.ToolCallsForMessages(ctx, ids)
	if err != nil {
		return err
	}
	for i := range msgs {
		if c, ok := byMsg[msgs[i].ID]; ok {
			msgs[i].Calls = c
		}
	}
	return nil
}

// attachFiles fills .Attachments on user messages that carry uploads.
func (d *DB) attachFiles(ctx context.Context, msgs []Message) error {
	var ids []string
	for _, m := range msgs {
		if m.Role == "user" {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	byMsg, err := d.attachmentsForMessages(ctx, ids)
	if err != nil {
		return err
	}
	for i := range msgs {
		if as, ok := byMsg[msgs[i].ID]; ok {
			msgs[i].Attachments = as
		}
	}
	return nil
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
	sb.WriteString(`SELECT id, conversation_id, role, content, status, token_count,
	                        completion_tokens, duration_ms, created_at, tool_call_id, synthetic
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
			comp    sql.NullInt64
			dur     sql.NullInt64
			created int64
		)
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Status,
			&tok, &comp, &dur, &created, &m.ToolCallID, &m.Synthetic); err != nil {
			return nil, "", err
		}
		if tok.Valid {
			v := tok.Int64
			m.TokenCount = &v
		}
		if comp.Valid {
			v := comp.Int64
			m.CompletionTokens = &v
		}
		if dur.Valid {
			v := dur.Int64
			m.DurationMS = &v
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
	if err := d.attachCalls(ctx, items); err != nil {
		return nil, "", err
	}
	if err := d.attachFiles(ctx, items); err != nil {
		return nil, "", err
	}
	if hasMore && len(items) > 0 {
		oldest := items[0]
		next = encodeCursor(oldest.CreatedAt.Unix(), oldest.ID)
	}
	return items, next, nil
}

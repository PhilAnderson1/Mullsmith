package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"mullsmith/internal/id"
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

// ErrHistoryLimit means the conversation's completed history exceeds the
// hard row cap, so context replay would silently drop turns (finding 2).
// A successful-but-partial history is a lie the caller cannot detect, so
// the cap now fails the read loudly; the server turns this into an
// explicit refusal BEFORE accepting another turn rather than quietly
// truncating — the append-only doctrine has no silent exit.
var ErrHistoryLimit = errors.New("conversation history exceeds the context row cap")

// contextHistoryCap is the ContextHistory runaway guard (rows). A var so
// tests exercise the boundary without inserting thousands of messages.
var contextHistoryCap = 4000

// AddMessage inserts a message and bumps the conversation's updated_at in one
// transaction (the sidebar orders by it, spec §6.1 step 6). userID guards
// against writing into someone else's thread (spec §9.2).
func (d *DB) AddMessage(ctx context.Context, userID string, m *Message) error {
	return d.addMessage(ctx, userID, m, nil)
}

// AddMessageWithFiles is AddMessage plus the attachment links IN THE SAME
// transaction (finding 4): there is no window where a durable user message
// exists without its files — the divergence that left an orphan turn behind
// when the separate attach call failed after the request had already been
// answered with a failure the client rolled back.
func (d *DB) AddMessageWithFiles(ctx context.Context, userID string, m *Message, paths []string) error {
	return d.addMessage(ctx, userID, m, paths)
}

func (d *DB) addMessage(ctx context.Context, userID string, m *Message, paths []string) error {
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
	for _, p := range paths {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attachments(message_id, path) VALUES(?, ?)`, m.ID, p); err != nil {
			return err
		}
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
// Mutation decisions must NOT use it as a check-then-act guard: between
// this read and any following write a send can append. Regeneration uses
// BeginRegeneration, which re-checks newestness inside its own transaction
// (finding 4). It remains for read-only projections.
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

// ErrNotLastMessage marks a mutation that must land on the conversation's
// newest row and found it isn't — checkable only inside the transaction
// that performs the swap, so no send can interleave (finding 4).
var ErrNotLastMessage = errors.New("message is not the newest in its conversation")

// BeginRegeneration atomically retires the newest assistant reply and puts a
// fresh streaming placeholder in its place: newest-check, delete and insert
// share ONE transaction (finding 4 — the old flow deleted the reply first
// and created the replacement only later, so a failure in between could
// leave the conversation a turn short, and a send that raced the check
// turned "newest" into a stale verdict). The deleted row's tool_calls and
// attachments disappear by cascade, exactly as with DeleteMessage. The
// placeholder is status='streaming', so ContextHistory already ignores it
// and the regenerated request never quotes its own draft; FinalizeMessage
// completes it like any streamed reply. Ownership is enforced through the
// conversation row.
func (d *DB) BeginRegeneration(ctx context.Context, userID, convID, oldMsgID string) (*Message, error) {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM conversations WHERE id = ? AND user_id = ?`, convID, userID).Scan(&one)
	if errors.Is(err, ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}

	var oldConv, oldRole string
	err = tx.QueryRowContext(ctx,
		`SELECT conversation_id, role FROM messages WHERE id = ?`, oldMsgID).
		Scan(&oldConv, &oldRole)
	if errors.Is(err, ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}
	if oldConv != convID || oldRole != "assistant" {
		return nil, ErrConversationNotFound // indistinguishable (§9.2)
	}

	var last string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM messages WHERE conversation_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		convID).Scan(&last)
	if err != nil {
		return nil, err
	}
	if last != oldMsgID {
		return nil, ErrNotLastMessage
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, oldMsgID); err != nil {
		return nil, err
	}
	ph := &Message{
		ID: id.New(), ConversationID: convID, Role: "assistant",
		Status: "streaming", CreatedAt: time.Now(),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages(id, conversation_id, role, content, status, token_count, created_at, tool_call_id, synthetic)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ph.ID, ph.ConversationID, ph.Role, ph.Content, ph.Status, nil, ph.CreatedAt.Unix(), "", false); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET updated_at = ? WHERE id = ?`,
		ph.CreatedAt.Unix(), convID); err != nil {
		return nil, err
	}
	return ph, tx.Commit()
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

// ContextHistoryRows exposes the runaway row cap so admission can account
// for the incoming turn BEFORE persisting it (the cap itself lives with
// the query that enforces it).
func ContextHistoryRows() int { return contextHistoryCap }

// SetContextHistoryCap overrides the runaway row cap and returns the
// restore function. Test-only seam: it lets endpoint-level tests exercise
// the boundary without inserting thousands of rows. Production paths never
// call it — the cap is fixed policy.
func SetContextHistoryCap(n int) (restore func()) {
	old := contextHistoryCap
	contextHistoryCap = n
	return func() { contextHistoryCap = old }
}

// ExecForTest runs one raw statement against the store's database.
// Test-only fault-injection seam (e.g. a BEFORE INSERT trigger rejecting
// a class of writes); production code must never call it.
func (d *DB) ExecForTest(q string, args ...any) error {
	_, err := d.sqlDB.Exec(q, args...)
	return err
}

// ContextHistory returns the conversation's completed messages (chronological,
// excluding excludeID) for prompt assembly and the overflow estimate. The
// request is append-only: when the row cap is exceeded, ContextHistory does
// NOT return a silently-truncated oldest slice — it fails with
// ErrHistoryLimit (finding 2). The server's overflow gate turns that into an
// explicit refusal before another turn is accepted; the cap remains a
// runaway guard, not a history policy. Assistant rows carry
// their Calls and tool rows their ToolCallID so the agent can replay the
// OpenAI tool protocol faithfully.
func (d *DB) ContextHistory(ctx context.Context, convID, excludeID string) ([]Message, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT id, conversation_id, role, content, status, token_count, created_at, tool_call_id, synthetic
		 FROM messages
		 WHERE conversation_id = ? AND id <> ? AND status = 'complete'
		 ORDER BY created_at ASC, id ASC LIMIT ?`,
		convID, excludeID, contextHistoryCap+1) // sentinel row: cap+1 means OVER the cap
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
	if len(out) > contextHistoryCap {
		return nil, ErrHistoryLimit
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

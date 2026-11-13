package store

import (
	"context"
	"time"

	"mullsmith/internal/id"
)

// ToolCall is a tool_calls-table row: the audit trail of one model
// proposed tool invocation. Result holds the truncated JSON handed back to
// the model, exactly as sent. DurationMS and FinishedAt are the
// execution timing of the call: written at FinishToolCall and selected
// here as part of the audit record. No application consumer reads them by design —
// they exist for DB-level auditing, deliberately kept out of the history
// API projection and the live tool-end events.
type ToolCall struct {
	ID        string
	MessageID string
	LLMCallID string // id string assigned by the model endpoint
	Tool      string
	Arguments string // JSON as proposed
	Status    string // pending_approval | approved | denied | running | ok | error
	Result    string
	// Display is the card payload (C7): the bounded JSON
	// {"summary","data","truncated"} the live tool_end event carried, kept
	// separate from Result, which is the complete text the model saw.
	// Rebuilding a card out of that string could cut its JSON in half —
	// links and download cards vanished on reload.
	Display    string
	DurationMS int64
	CreatedAt  time.Time
	FinishedAt time.Time
}

// AddToolCall records a proposed call in its initial status.
func (d *DB) AddToolCall(ctx context.Context, tc *ToolCall) error {
	if tc.ID == "" {
		tc.ID = id.New()
	}
	if tc.CreatedAt.IsZero() {
		tc.CreatedAt = time.Now()
	}
	_, err := d.sqlDB.ExecContext(ctx,
		`INSERT INTO tool_calls(id, message_id, llm_call_id, tool, arguments, status, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		tc.ID, tc.MessageID, tc.LLMCallID, tc.Tool, tc.Arguments, tc.Status, tc.CreatedAt.Unix())
	return err
}

// UpdateToolCallStatus moves a call between lifecycle statuses before the
// final FinishToolCall (e.g. pending_approval → approved/denied). display
// carries the card payload for a verdict that IS terminal for the card —
// a denial never reaches FinishToolCall, so without it the reload lost the
// reason the live view showed. The supplied display replaces the stored
// payload; FinishToolCall replaces it again when an approved call finishes.
func (d *DB) UpdateToolCallStatus(ctx context.Context, id, status, display string) error {
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE tool_calls SET status = ?, display = ? WHERE id = ?`, status, display, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRowGone // audit row deleted (conversation removal) — C6
	}
	return nil
}

// FinishToolCall stores the terminal status plus the truncated model-facing
// result, the display payload the UI reloads from (C7) and the duration.
func (d *DB) FinishToolCall(ctx context.Context, id, status, result, display string, durationMS int64) error {
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE tool_calls SET status = ?, result = ?, display = ?, duration_ms = ?, finished_at = ?
		 WHERE id = ?`,
		status, result, display, durationMS, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRowGone // audit row deleted (conversation removal) — C6
	}
	return nil
}

// FinishToolCallWithResult closes a tool execution and replaces its already-
// reserved result message in one transaction. The reservation is inserted
// before execution so message capacity is a prerequisite of side effects;
// this transaction makes the final audit/result pair visible together.
func (d *DB) FinishToolCallWithResult(ctx context.Context, id, resultMessageID,
	status, result, display string, durationMS, tokenCount int64,
) error {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx,
		`UPDATE tool_calls SET status = ?, result = ?, display = ?, duration_ms = ?, finished_at = ?
		 WHERE id = ?`,
		status, result, display, durationMS, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRowGone
	}
	// Pair by the tool call's durable LLM id as well as the result row id.
	// A caller cannot accidentally finalize a reservation belonging to a
	// different call or conversation.
	res, err = tx.ExecContext(ctx,
		`UPDATE messages SET content = ?, status = 'complete', token_count = ?
		 WHERE id = ? AND role = 'tool'
		   AND tool_call_id = (SELECT llm_call_id FROM tool_calls WHERE id = ?)`,
		result, tokenCount, resultMessageID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRowGone
	}
	return tx.Commit()
}

// ToolCallsForMessages returns audit rows grouped by assistant message id,
// in proposal order (created_at, id). Missing ids simply get no entry.
func (d *DB) ToolCallsForMessages(ctx context.Context, msgIDs []string) (map[string][]ToolCall, error) {
	out := map[string][]ToolCall{}
	if len(msgIDs) == 0 {
		return out, nil
	}
	// #nosec G202 -- placeholders() emits only "?, ?, ..." literals; every
	// value is bound as a parameter.
	q := `SELECT id, message_id, llm_call_id, tool, arguments, status,
	             COALESCE(result, ''), COALESCE(display, ''), COALESCE(duration_ms, 0),
	             created_at, COALESCE(finished_at, 0)
	      FROM tool_calls WHERE message_id IN (` + placeholders(len(msgIDs)) + `)
	      ORDER BY created_at ASC, id ASC`
	args := make([]any, len(msgIDs))
	for i, s := range msgIDs {
		args[i] = s
	}
	rows, err := d.sqlDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			tc           ToolCall
			created, fin int64
		)
		if err := rows.Scan(&tc.ID, &tc.MessageID, &tc.LLMCallID, &tc.Tool, &tc.Arguments,
			&tc.Status, &tc.Result, &tc.Display, &tc.DurationMS, &created, &fin); err != nil {
			return nil, err
		}
		tc.CreatedAt = time.Unix(created, 0)
		if fin != 0 {
			tc.FinishedAt = time.Unix(fin, 0)
		}
		out[tc.MessageID] = append(out[tc.MessageID], tc)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	s := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		if i > 0 {
			s = append(s, ',')
		}
		s = append(s, '?')
	}
	return string(s)
}

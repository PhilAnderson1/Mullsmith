package store

import (
	"context"
	"time"

	"gabkeep/internal/id"
)

// ToolCall is a tool_calls-table row (§4): the audit trail of one model
// proposed tool invocation. Result holds the truncated JSON handed back to
// the model, exactly as sent (§7.1).
type ToolCall struct {
	ID         string
	MessageID  string
	LLMCallID  string // id string assigned by the model endpoint
	Tool       string
	Arguments  string // JSON as proposed
	Status     string // pending_approval | approved | denied | running | ok | error
	Result     string
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
// final FinishToolCall (e.g. pending_approval → approved/denied, → running).
func (d *DB) UpdateToolCallStatus(ctx context.Context, id, status string) error {
	_, err := d.sqlDB.ExecContext(ctx,
		`UPDATE tool_calls SET status = ? WHERE id = ?`, status, id)
	return err
}

// FinishToolCall stores the terminal status plus the truncated result and
// duration.
func (d *DB) FinishToolCall(ctx context.Context, id, status, result string, durationMS int64) error {
	_, err := d.sqlDB.ExecContext(ctx,
		`UPDATE tool_calls SET status = ?, result = ?, duration_ms = ?, finished_at = ?
		 WHERE id = ?`,
		status, result, durationMS, time.Now().Unix(), id)
	return err
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
	             COALESCE(result, ''), COALESCE(duration_ms, 0), created_at, COALESCE(finished_at, 0)
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
			&tc.Status, &tc.Result, &tc.DurationMS, &created, &fin); err != nil {
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

package store

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"gabkeep/internal/id"
)

// ErrConversationNotFound covers "does not exist", "not yours" and
// "soft-deleted" alike — the API answers 404 in all cases so conversation
// ids cannot be probed across users (spec §9.2).
var ErrConversationNotFound = errors.New("conversation not found")

// Conversation is a conversations-table row.
type Conversation struct {
	ID           string
	UserID       string
	Title        string
	Model        string // "" = server default
	SystemPrompt string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// --- opaque keyset cursors ---------------------------------------------------
// Format: base64url("<unix-seconds>:<id>"), treated as untrusted input.

func encodeCursor(ts int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(ts, 10) + ":" + id))
}

func decodeCursor(s string) (ts int64, idStr string, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) > 80 {
		return 0, "", false
	}
	sep := strings.LastIndexByte(string(b), ':')
	if sep <= 0 {
		return 0, "", false
	}
	n, err := strconv.ParseInt(string(b[:sep]), 10, 64)
	if err != nil {
		return 0, "", false
	}
	return n, string(b[sep+1:]), true
}

// escapeLike makes q safe for LIKE ... ESCAPE '\'.
func escapeLike(q string) string {
	q = strings.ReplaceAll(q, `\`, `\\`)
	q = strings.ReplaceAll(q, `%`, `\%`)
	q = strings.ReplaceAll(q, `_`, `\_`)
	return q
}

// --- conversations -----------------------------------------------------------

// DefaultTitle is the placeholder a conversation carries until its first
// exchange names it (auto-titling) or the user renames it.
const DefaultTitle = "New chat"

func (d *DB) CreateConversation(ctx context.Context, c *Conversation) error {
	if c.ID == "" {
		c.ID = id.New()
	}
	if c.Title == "" {
		c.Title = DefaultTitle
	}
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = c.CreatedAt
	_, err := d.sqlDB.ExecContext(ctx,
		`INSERT INTO conversations(id, user_id, title, model, system_prompt, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.UserID, c.Title, c.Model, c.SystemPrompt,
		c.CreatedAt.Unix(), c.UpdatedAt.Unix())
	return err
}

// ConversationForUser returns the live conversation owned by userID, or
// ErrConversationNotFound.
func (d *DB) ConversationForUser(ctx context.Context, userID, convID string) (*Conversation, error) {
	var (
		c                Conversation
		created, updated int64
	)
	err := d.sqlDB.QueryRowContext(ctx,
		`SELECT id, user_id, title, model, system_prompt, created_at, updated_at
		 FROM conversations WHERE id = ? AND user_id = ? AND deleted_at IS NULL`,
		convID, userID).
		Scan(&c.ID, &c.UserID, &c.Title, &c.Model, &c.SystemPrompt, &created, &updated)
	if errors.Is(err, ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt = time.Unix(created, 0)
	c.UpdatedAt = time.Unix(updated, 0)
	return &c, nil
}

// ConversationPatch carries optional updates; nil fields stay untouched.
type ConversationPatch struct {
	Title        *string
	Model        *string
	SystemPrompt *string
}

func (d *DB) UpdateConversation(ctx context.Context, userID, convID string, p ConversationPatch) (*Conversation, error) {
	var sets []string
	var args []any
	if p.Title != nil {
		sets = append(sets, `title = ?`)
		args = append(args, *p.Title)
	}
	if p.Model != nil {
		sets = append(sets, `model = ?`)
		args = append(args, *p.Model)
	}
	if p.SystemPrompt != nil {
		sets = append(sets, `system_prompt = ?`)
		args = append(args, *p.SystemPrompt)
	}
	if len(sets) == 0 {
		return d.ConversationForUser(ctx, userID, convID)
	}
	sets = append(sets, `updated_at = ?`)
	args = append(args, time.Now().Unix(), convID, userID)
	// #nosec G202 -- sets holds only hardcoded "col = ?" fragments; the join
	// just wires them together, values bind as parameters.
	q := `UPDATE conversations SET ` + strings.Join(sets, ", ") +
		` WHERE id = ? AND user_id = ? AND deleted_at IS NULL`
	res, err := d.sqlDB.ExecContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrConversationNotFound
	}
	return d.ConversationForUser(ctx, userID, convID)
}

// SoftDeleteConversation stamps deleted_at; returns ErrConversationNotFound
// when there is nothing live to delete.
// SetTitleIfDefault names a conversation that is still unnamed. The guard
// runs inside the UPDATE so a title the user typed (rename) can never be
// overwritten by the concurrent auto-titler. Reports whether it wrote.
func (d *DB) SetTitleIfDefault(ctx context.Context, convID, title string) (bool, error) {
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE conversations SET title = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL AND (title = '' OR title = ?)`,
		title, time.Now().Unix(), convID, DefaultTitle)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Touch bumps a live conversation's updated_at: "activity" for the list
// ordering also includes simply opening a chat, not just new messages
// (AddMessage bumps it too). Ownership rides the WHERE clause so the
// touch can never reach another user's row. Reports whether it wrote.
func (d *DB) TouchConversation(ctx context.Context, userID, convID string) (bool, error) {
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE conversations SET updated_at = ? WHERE id = ? AND user_id = ? AND deleted_at IS NULL`,
		time.Now().Unix(), convID, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (d *DB) SoftDeleteConversation(ctx context.Context, userID, convID string) error {
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE conversations SET deleted_at = ? WHERE id = ? AND user_id = ? AND deleted_at IS NULL`,
		time.Now().Unix(), convID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// ListConversations returns live conversations, most recently updated first.
// cursor is the opaque value from a previous page ("" for the first). The
// returned next cursor is "" when exhausted.
func (d *DB) ListConversations(ctx context.Context, userID, q, cursor string, limit int) (items []Conversation, next string, err error) {
	if limit < 1 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	sb := strings.Builder{}
	sb.WriteString(`SELECT id, user_id, title, model, system_prompt, created_at, updated_at
	                 FROM conversations WHERE user_id = ? AND deleted_at IS NULL`)
	args := []any{userID}
	if q != "" {
		sb.WriteString(` AND title LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(q)+"%")
	}
	if cursor != "" {
		ts, cid, ok := decodeCursor(cursor)
		if !ok {
			return nil, "", errors.New("invalid cursor")
		}
		sb.WriteString(` AND (updated_at < ? OR (updated_at = ? AND id < ?))`)
		args = append(args, ts, ts, cid)
	}
	sb.WriteString(` ORDER BY updated_at DESC, id DESC LIMIT ?`)
	args = append(args, limit+1) // one extra row reveals has-more

	rows, err := d.sqlDB.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			c                Conversation
			created, updated int64
		)
		if err := rows.Scan(&c.ID, &c.UserID, &c.Title, &c.Model, &c.SystemPrompt, &created, &updated); err != nil {
			return nil, "", err
		}
		c.CreatedAt = time.Unix(created, 0)
		c.UpdatedAt = time.Unix(updated, 0)
		items = append(items, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) > limit {
		last := items[limit-1]
		items = items[:limit]
		next = encodeCursor(last.UpdatedAt.Unix(), last.ID)
	}
	return items, next, nil
}

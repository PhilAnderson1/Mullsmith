package store

import (
	"context"
	"errors"
	"time"
)

// CreateInvite records a single-use invite code minted by the admin CLI.
func (d *DB) CreateInvite(ctx context.Context, code string, now time.Time) error {
	_, err := d.sqlDB.ExecContext(ctx,
		`INSERT INTO invites(code, created_by, created_at) VALUES(?, 'cli', ?)`, code, now.Unix())
	return err
}

// InviteAvailable reports whether a code exists and is still unconsumed.
// Read-only pre-flight for registration so a dead invite fails before the
// expensive Argon2 hash (audit finding 4); Register's transaction remains
// the sole authority that consumes a code, so a concurrent race still
// rejects correctly — it just pays for a wasted hash.
func (d *DB) InviteAvailable(ctx context.Context, code string) (bool, error) {
	var usedBy *string
	err := d.sqlDB.QueryRowContext(ctx,
		`SELECT used_by FROM invites WHERE code = ?`, code).Scan(&usedBy)
	if errors.Is(err, ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return usedBy == nil, nil
}

// InviteInfo is the admin-visible state of a code.
type InviteInfo struct {
	Code      string
	CreatedAt time.Time
	UsedBy    string // empty when unused
	UsedAt    time.Time
}

// ListInvites returns all invites, newest first.
func (d *DB) ListInvites(ctx context.Context) ([]InviteInfo, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT code, created_at, COALESCE(used_by, ''), COALESCE(used_at, 0)
		 FROM invites ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InviteInfo
	for rows.Next() {
		var (
			i               InviteInfo
			created, usedAt int64
		)
		if err := rows.Scan(&i.Code, &created, &i.UsedBy, &usedAt); err != nil {
			return nil, err
		}
		i.CreatedAt = time.Unix(created, 0)
		if usedAt != 0 {
			i.UsedAt = time.Unix(usedAt, 0)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

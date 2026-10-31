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

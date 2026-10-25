package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Session is a sessions-table row; IDHash is sha256(cookie token).
type Session struct {
	IDHash     []byte
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Remembered bool
	LastSeenAt time.Time
	UserAgent  string
}

func (d *DB) CreateSession(ctx context.Context, s *Session) error {
	_, err := d.sqlDB.ExecContext(ctx,
		`INSERT INTO sessions(id_hash, user_id, created_at, expires_at, remembered, last_seen_at, user_agent)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		s.IDHash, s.UserID, s.CreatedAt.Unix(), s.ExpiresAt.Unix(),
		s.Remembered, s.LastSeenAt.Unix(), s.UserAgent)
	return err
}

// SessionByHash returns nil, nil for unknown tokens.
func (d *DB) SessionByHash(ctx context.Context, hash []byte) (*Session, error) {
	var (
		s                          Session
		created, expires, lastSeen int64
		remembered                 int
	)
	err := d.sqlDB.QueryRowContext(ctx,
		`SELECT user_id, created_at, expires_at, remembered, last_seen_at, user_agent
		 FROM sessions WHERE id_hash = ?`, hash).
		Scan(&s.UserID, &created, &expires, &remembered, &lastSeen, &s.UserAgent)
	switch {
	case errors.Is(err, ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	s.IDHash = hash
	s.CreatedAt = time.Unix(created, 0)
	s.ExpiresAt = time.Unix(expires, 0)
	s.LastSeenAt = time.Unix(lastSeen, 0)
	s.Remembered = remembered != 0
	return &s, nil
}

// TouchSession extends expiry and last_seen for the sliding renewal (spec §9.1).
func (d *DB) TouchSession(ctx context.Context, hash []byte, expiresAt, lastSeenAt time.Time) error {
	_, err := d.sqlDB.ExecContext(ctx,
		`UPDATE sessions SET expires_at = ?, last_seen_at = ? WHERE id_hash = ?`,
		expiresAt.Unix(), lastSeenAt.Unix(), hash)
	return err
}

func (d *DB) DeleteSession(ctx context.Context, hash []byte) error {
	_, err := d.sqlDB.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, hash)
	return err
}

// DeleteSessionsForUser removes all of a user's sessions except exceptHash
// (nil = all): "sign out everywhere" and password-change revocation.
func (d *DB) DeleteSessionsForUser(ctx context.Context, userID string, exceptHash []byte) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if exceptHash == nil {
		res, err = d.sqlDB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	} else {
		res, err = d.sqlDB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id_hash <> ?`, userID, exceptHash)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteExpired purges expired sessions; returns rows removed.
func (d *DB) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := d.sqlDB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

package store

import (
	"context"
	"errors"
	"time"
)

// File is one row of the files table (spec §4). RelPath is relative to
// scratchpad/<user-id>/ and must already have passed scratch.ParseRel on the
// way in; the DB never holds absolute paths.
type File struct {
	ID             string
	UserID         string
	ConversationID string // "" when no provenance
	Kind           string // upload | artifact
	RelPath        string
	MediaType      string
	Name           string
	Size           int64
	SHA256         []byte
	CreatedAt      time.Time
	ExpiresAt      time.Time // zero for uploads
}

// File kinds (§4).
const (
	FileKindUpload   = "upload"
	FileKindArtifact = "artifact"
)

var ErrFileNotFound = errors.New("file not found")

const fileCols = `id, user_id, COALESCE(conversation_id,''), kind, rel_path, media_type,
	name, size, sha256, created_at, COALESCE(expires_at,0)`

func scanFile(r row) (File, error) {
	var (
		f       File
		created int64
		expires int64
	)
	err := r.Scan(&f.ID, &f.UserID, &f.ConversationID, &f.Kind, &f.RelPath,
		&f.MediaType, &f.Name, &f.Size, &f.SHA256, &created, &expires)
	f.CreatedAt = time.Unix(created, 0)
	if expires > 0 {
		f.ExpiresAt = time.Unix(expires, 0)
	}
	return f, err
}

func (d *DB) CreateFile(ctx context.Context, f *File) error {
	var expires any
	if !f.ExpiresAt.IsZero() {
		expires = f.ExpiresAt.Unix()
	}
	var conv any
	if f.ConversationID != "" {
		conv = f.ConversationID
	}
	_, err := d.sqlDB.ExecContext(ctx,
		`INSERT INTO files(id,user_id,conversation_id,kind,rel_path,media_type,name,size,sha256,created_at,expires_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		f.ID, f.UserID, conv, f.Kind, f.RelPath, f.MediaType, f.Name, f.Size, f.SHA256,
		f.CreatedAt.Unix(), expires)
	return err
}

// GetFileForUser fetches a file scoped to its owner — a different user's id
// is indistinguishable from a nonexistent one (§9.2).
func (d *DB) GetFileForUser(ctx context.Context, userID, fileID string) (File, error) {
	f, err := scanFile(d.sqlDB.QueryRowContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE id = ? AND user_id = ?`, fileID, userID))
	if errors.Is(err, ErrNoRows) {
		return f, ErrFileNotFound
	}
	return f, err
}

// ListFiles returns the user's files newest first; kind "" means both.
func (d *DB) ListFiles(ctx context.Context, userID, kind, cursor string, limit int) (items []File, next string, err error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	q := `SELECT ` + fileCols + ` FROM files WHERE user_id = ?`
	args := []any{userID}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	if cursor != "" {
		ts, fid, ok := decodeCursor(cursor)
		if !ok {
			return nil, "", errors.New("invalid cursor")
		}
		q += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, ts, ts, fid)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := d.sqlDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, "", err
		}
		items = append(items, f)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) > limit {
		last := items[limit-1]
		items = items[:limit]
		next = encodeCursor(last.CreatedAt.Unix(), last.ID)
	}
	return items, next, nil
}

// DeleteFileForUser removes the row and returns it so the caller can unlink
// the physical file (DB first: a stale file is swept, a ghost row is not).
func (d *DB) DeleteFileForUser(ctx context.Context, userID, fileID string) (File, error) {
	f, err := d.GetFileForUser(ctx, userID, fileID)
	if err != nil {
		return f, err
	}
	if _, err := d.sqlDB.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, fileID); err != nil {
		return f, err
	}
	return f, nil
}

// AttachFilesToMessage links uploaded files to a message after verifying
// every id belongs to the user; any foreign id aborts the whole batch.
func (d *DB) AttachFilesToMessage(ctx context.Context, messageID, userID string, fileIDs []string) error {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, fid := range fileIDs {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT user_id FROM files WHERE id = ?`, fid).Scan(&owner)
		if errors.Is(err, ErrNoRows) {
			return ErrFileNotFound
		} else if err != nil {
			return err
		}
		if owner != userID {
			return ErrFileNotFound // never reveal existence (§9.2)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attachments(message_id, file_id) VALUES(?, ?)`, messageID, fid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FilesForMessage returns the files attached to a message (for API JSON).
func (d *DB) FilesForMessage(ctx context.Context, messageID string) ([]File, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE id IN (SELECT file_id FROM attachments WHERE message_id = ?)
		 ORDER BY created_at, id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ExpiredArtifacts returns up to limit artifact rows past their TTL; the
// caller unlinks the files and calls DeleteFilesByID.
func (d *DB) ExpiredArtifacts(ctx context.Context, now time.Time, limit int) ([]File, error) {
	if limit < 1 || limit > 1000 {
		limit = 1000 // bounded pass (§9.11)
	}
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE kind = 'artifact' AND expires_at IS NOT NULL AND expires_at <= ?
		 ORDER BY expires_at LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (d *DB) DeleteFilesByID(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ExistingFileSet returns "userID/relPath" keys for every row — the orphan
// sweep's "known files" set (§8).
func (d *DB) ExistingFileSet(ctx context.Context) (map[string]bool, error) {
	rows, err := d.sqlDB.QueryContext(ctx, `SELECT user_id, rel_path FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var uid, rel string
		if err := rows.Scan(&uid, &rel); err != nil {
			return nil, err
		}
		out[uid+"/"+rel] = true
	}
	return out, rows.Err()
}

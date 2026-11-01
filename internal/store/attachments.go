// Attachments: the join between a message and the uploads it was sent
// with (spec §4). This is the ONLY file-related table left — it holds no
// file metadata, because the disk is the source of truth. A row asserts a
// conversation fact whose pairing exists nowhere else, and stays truthful
// even after the bytes are gone (the UI chip then renders dead and the
// download 404s).
package store

import (
	"context"
	"path"
	"strings"
)

// Attachment is one attachments-table row. Path is scratchpad-relative
// ("uploads/report.pdf"), canonical (scratch.ParseRel segments rejoined)
// and proven to resolve inside the owner's sandbox BEFORE insertion.
// The HTTP upload edge narrows that further to uploads/ (a user message
// may only claim what the user sent); a view_image tool turn may point
// at any sandbox dir, and the row ages with the file's TTL — the chip
// renders dead and replay degrades to manifest-only, the documented
// fate of any vanished attachment.
type Attachment struct {
	Path string
}

// Name is the display name: since uploads live under their real sanitized
// filename, the basename of the path is the name.
func (a Attachment) Name() string { return path.Base(a.Path) }

// attachmentsForMessages loads the attachments of many messages in one
// query, keyed by message id (ordered by path for stable chips).
func (d *DB) attachmentsForMessages(ctx context.Context, messageIDs []string) (map[string][]Attachment, error) {
	if len(messageIDs) == 0 {
		return map[string][]Attachment{}, nil
	}
	placeholders := strings.Repeat("?,", len(messageIDs))
	// #nosec G202 -- the interpolated fragment is "?,?,..." placeholders only;
	// every value is bound as a parameter.
	q := `SELECT message_id, path FROM attachments
	       WHERE message_id IN (` + placeholders[:len(placeholders)-1] + `)
	       ORDER BY message_id, path`
	args := make([]any, len(messageIDs))
	for i, id := range messageIDs {
		args[i] = id
	}
	rows, err := d.sqlDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Attachment{}
	for rows.Next() {
		var msgID, p string
		if err := rows.Scan(&msgID, &p); err != nil {
			return nil, err
		}
		out[msgID] = append(out[msgID], Attachment{Path: p})
	}
	return out, rows.Err()
}

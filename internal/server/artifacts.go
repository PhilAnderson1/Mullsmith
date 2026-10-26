package server

import (
	"context"
	"errors"
	"time"

	"gabkeep/internal/id"
	"gabkeep/internal/scratch"
	"gabkeep/internal/store"
	"gabkeep/internal/tools"
)

// artifactSink implements tools.ArtifactSink over the files table: files the
// model shares via files_share become kind=artifact rows with a TTL
// (spec §8: artifact_ttl from last share). Kept server-side so the tools
// package never imports the store.
type artifactSink struct {
	db  *store.DB
	ttl time.Duration
}

func (a *artifactSink) RegisterArtifact(ctx context.Context, rec tools.ArtifactRecord) (string, error) {
	ttl := a.ttl
	if ttl <= 0 { // config.Load sets 168h; struct-literal configs must not mean "expire instantly"
		ttl = 168 * time.Hour
	}
	// Defence in depth: the tool already stat'ed through the sandbox, but a
	// row must never carry a path the sandbox would reject (§9.7) — and
	// never an uploads/ path at all: artifact rows expire, and a TTL'd
	// pointer at the user's upload made cleanup delete the original file
	// (audit finding 8). With uploads excluded, every on-disk path carries
	// at most one row and row-deletes can unlink without refcounting.
	segs, err := scratch.ParseRel(rec.RelPath)
	if err != nil {
		return "", errors.New("unsafe artifact path")
	}
	if len(segs) > 0 && segs[0] == "uploads" {
		return "", errors.New("artifacts cannot point into uploads/ (user data must not expire)")
	}
	if rec.Size < 0 || (rec.SHA256 == nil && rec.Size > 0) {
		return "", errors.New("invalid artifact record")
	}
	expires := time.Now().Add(ttl)

	// Dedupe: re-sharing the same path refreshes the existing row so the
	// download URL stays stable across rewrites within the TTL window.
	if existing, err := a.db.FindArtifactByPath(ctx, rec.UserID, rec.RelPath); err == nil {
		if err := a.db.UpdateArtifactOnShare(ctx, existing.ID, rec.MediaType, rec.Size, rec.SHA256, expires); err != nil {
			return "", err
		}
		return existing.ID, nil
	} else if !errors.Is(err, store.ErrFileNotFound) {
		return "", err
	}

	f := &store.File{
		ID: id.New(), UserID: rec.UserID, ConversationID: rec.ConversationID,
		Kind: store.FileKindArtifact, RelPath: rec.RelPath, MediaType: rec.MediaType,
		Name: rec.Name, Size: rec.Size, SHA256: rec.SHA256,
		CreatedAt: time.Now(), ExpiresAt: expires,
	}
	if err := a.db.CreateFile(ctx, f); err != nil {
		return "", err
	}
	return f.ID, nil
}

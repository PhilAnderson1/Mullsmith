package server

import (
	"context"
	"testing"
	"time"

	"gabkeep/internal/config"
	"gabkeep/internal/store"
	"gabkeep/internal/tools"
)

func TestArtifactSinkDedupeAndTTL(t *testing.T) {
	db := testStore(t)
	sink := &artifactSink{db: db, ttl: time.Hour}
	ctx := context.Background()

	u := &store.User{Email: "sink@test.io", Name: "S", PasswordHash: "x", Status: "active",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	rec := tools.ArtifactRecord{UserID: u.ID, ConversationID: "", RelPath: "artifacts/r.md",
		MediaType: "text/plain", Name: "r.md", Size: 10, SHA256: make([]byte, 32)}

	id1, err := sink.RegisterArtifact(ctx, rec)
	if err != nil || id1 == "" {
		t.Fatalf("register: %q, %v", id1, err)
	}
	row, err := db.GetFileForUser(ctx, u.ID, id1)
	if err != nil || row.Kind != store.FileKindArtifact || row.ExpiresAt.IsZero() {
		t.Fatalf("row = %+v, %v", row, err)
	}
	if d := time.Until(row.ExpiresAt); d < 55*time.Minute || d > time.Hour {
		t.Errorf("expires_at = %v, want ~1h ahead", row.ExpiresAt)
	}

	// Re-share the same path: same id, refreshed size/hash, URL stays stable.
	rec2 := rec
	rec2.Size = 99
	rec2.SHA256 = make([]byte, 32)
	rec2.SHA256[0] = 1
	id2, err := sink.RegisterArtifact(ctx, rec2)
	if err != nil || id2 != id1 {
		t.Fatalf("dedupe: %q != %q (%v)", id2, id1, err)
	}
	row2, _ := db.GetFileForUser(ctx, u.ID, id1)
	if row2.Size != 99 {
		t.Errorf("size not refreshed: %+v", row2)
	}
	items, _, _ := db.ListFiles(ctx, u.ID, store.FileKindArtifact, "", 10)
	if len(items) != 1 {
		t.Errorf("dedupe left %d rows", len(items))
	}

	// Unsafe paths never become rows (§9.7).
	if _, err := sink.RegisterArtifact(ctx, tools.ArtifactRecord{
		UserID: u.ID, RelPath: "../escape.txt", Name: "x", Size: 1,
		SHA256: make([]byte, 32)}); err == nil {
		t.Error("unsafe path must be refused")
	}
}

func TestArtifactTTLOverridesDefault(t *testing.T) {
	cfg := scratchCfg(t)
	cfg.Scratchpad.ArtifactTTL = config.Duration(2 * time.Hour)
	db := testStore(t)
	sink := &artifactSink{db: db, ttl: time.Duration(cfg.Scratchpad.ArtifactTTL)}
	ctx := context.Background()
	u := &store.User{Email: "ttl@test.io", Name: "S", PasswordHash: "x", Status: "active",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	db.CreateUser(ctx, u)
	id, err := sink.RegisterArtifact(ctx, tools.ArtifactRecord{UserID: u.ID,
		RelPath: "artifacts/a.txt", MediaType: "text/plain", Name: "a.txt",
		Size: 3, SHA256: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	row, _ := db.GetFileForUser(ctx, u.ID, id)
	if d := time.Until(row.ExpiresAt); d < 115*time.Minute || d > 121*time.Minute {
		t.Errorf("TTL from config not honoured: %v until expiry", d)
	}
}

// Audit finding 8: an artifact row carries a TTL; pointing one at the
// user's upload made expiry (or deleting the share card) destroy the
// original bytes. The sink itself now refuses the uploads namespace —
// defense in depth behind the tool's refusal.
func TestArtifactSinkRejectsUploadsNamespace(t *testing.T) {
	db := testStore(t)
	sink := &artifactSink{db: db, ttl: time.Hour}
	ctx := context.Background()

	u := &store.User{Email: "sinkns@test.io", Name: "S", PasswordHash: "x", Status: "active",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	rec := tools.ArtifactRecord{UserID: u.ID, RelPath: "uploads/data.csv",
		MediaType: "text/csv", Name: "data.csv", Size: 5, SHA256: make([]byte, 32)}
	if id, err := sink.RegisterArtifact(ctx, rec); err == nil || id != "" {
		t.Errorf("uploads path registered: id=%q err=%v, want refusal", id, err)
	}
	rec.RelPath = "./uploads/data.csv" // normalizes into the same namespace
	if id, err := sink.RegisterArtifact(ctx, rec); err == nil || id != "" {
		t.Errorf("./uploads path registered: id=%q err=%v, want refusal", id, err)
	}

	rec.RelPath = "work/report.md" // generated files remain registrable
	if id, err := sink.RegisterArtifact(ctx, rec); err != nil || id == "" {
		t.Errorf("work registration: %q %v", id, err)
	}
}

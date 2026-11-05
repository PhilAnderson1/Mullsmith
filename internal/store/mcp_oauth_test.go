package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestMCPOAuthConnectionBelongsToUserAndCascades(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now()
	user := &User{ID: "oauth-user", Email: "oauth@example.test", Name: "OAuth", Status: "active", CreatedAt: now, UpdatedAt: now}
	if err := db.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := db.PutMCPOAuthConnection(ctx, &MCPOAuthConnection{UserID: user.ID, ServerID: "service", TokenCipher: []byte("ciphertext")}); err != nil {
		t.Fatal(err)
	}
	if row, err := db.MCPOAuthConnection(ctx, "another-user", "service"); err != nil || row != nil {
		t.Fatalf("another user's lookup = %v, %v", row, err)
	}
	if deleted, err := db.DeleteUser(ctx, user.ID); err != nil || !deleted {
		t.Fatalf("DeleteUser = %v, %v", deleted, err)
	}
	if row, err := db.MCPOAuthConnection(ctx, user.ID, "service"); err != nil || row != nil {
		t.Fatalf("connection survived user deletion: %v, %v", row, err)
	}
}

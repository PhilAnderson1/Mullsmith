package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConcurrentPendingRegistrationPreservesLosingInvite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := make(chan struct{})
	type result struct {
		invite string
		err    error
	}
	results := make(chan result, 2)
	for _, invite := range []string{"first-invite", "second-invite"} {
		if err := db.CreateInvite(ctx, invite, time.Now()); err != nil {
			t.Fatal(err)
		}
		u := mkUser("")
		u.Status, u.UnconfirmedEmail = StatusUnconfirmed, "pending@example.test"
		go func() { <-start; results <- result{invite, db.Register(ctx, u, invite)} }()
	}
	close(start)
	succeeded, refused := 0, 0
	for i := 0; i < 2; i++ {
		r := <-results
		switch {
		case r.err == nil:
			succeeded++
		case errors.Is(r.err, ErrUserExists):
			refused++
			if available, err := db.InviteAvailable(ctx, r.invite); err != nil || !available {
				t.Fatalf("losing invite consumed: %v %v", available, err)
			}
		default:
			t.Fatalf("registration error: %v", r.err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Fatalf("succeeded=%d refused=%d", succeeded, refused)
	}
	for _, table := range []string{"users", "conversations"} {
		var count int
		query := "SELECT COUNT(*) FROM users"
		if table == "conversations" {
			query = "SELECT COUNT(*) FROM conversations"
		}
		if err := db.sqlDB.QueryRowContext(ctx, query).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count=%d error=%v", table, count, err)
		}
	}
}

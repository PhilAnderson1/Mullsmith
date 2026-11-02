package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestConversationLimitAtomicAndPerUser(t *testing.T) {
	db := openTestDB(t).WithRecordLimits(3, 5)
	ctx := context.Background()
	a, b := mkUser("limit-a@test.io"), mkUser("limit-b@test.io")
	for _, u := range []*User{a, b} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	foreign := mkConv(t, db, b.ID, "foreign")
	results := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- db.CreateConversation(ctx, &Conversation{UserID: a.ID}) }()
	}
	wg.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else if !errors.Is(err, ErrConversationLimit) {
			t.Fatal(err)
		}
	}
	if created != 3 {
		t.Fatalf("created %d, want 3", created)
	}
	convs, _, err := db.ListConversations(ctx, a.ID, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteConversation(ctx, a.ID, convs[0].ID); err != nil {
		t.Fatal(err)
	}
	mkConv(t, db, a.ID, "reclaimed")
	if _, err := db.ConversationForUser(ctx, b.ID, foreign.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMessageLimitAtomicAcrossConversations(t *testing.T) {
	db := openTestDB(t).WithRecordLimits(-1, 5)
	ctx := context.Background()
	u := mkUser("messages@test.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	convs := make([]*Conversation, 12)
	for i := range convs {
		convs[i] = mkConv(t, db, u.ID, "thread")
	}
	results := make(chan error, len(convs))
	var wg sync.WaitGroup
	for _, c := range convs {
		wg.Add(1)
		go func(c *Conversation) {
			defer wg.Done()
			_, _, err := db.BeginSend(ctx, u.ID, c.ID, "hello", []string{"uploads/input.txt"}, 1, "")
			results <- err
		}(c)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrMessageLimit) {
			t.Fatal(err)
		}
	}
	if accepted != 2 {
		t.Fatalf("accepted %d sends, want 2", accepted)
	}
	var rows, links int
	if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if rows != 4 || links != 2 {
		t.Fatalf("rows=%d attachments=%d, want 4/2", rows, links)
	}
	// All roles, including synthetic user turns and tool results, spend capacity.
	synthetic := &Message{ConversationID: convs[0].ID, Role: "user", Synthetic: true}
	if err := db.AddMessageWithFiles(ctx, u.ID, synthetic, []string{"uploads/image.png"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddMessage(ctx, u.ID, &Message{ConversationID: convs[0].ID, Role: "tool"}); !errors.Is(err, ErrMessageLimit) {
		t.Fatalf("tool over limit: %v", err)
	}
	if err := db.DeleteConversation(ctx, u.ID, convs[0].ID); err != nil {
		t.Fatal(err)
	}
	// Deletion always frees at least the synthetic row; no counters to repair.
	if err := db.AddMessage(ctx, u.ID, &Message{ConversationID: convs[1].ID, Role: "tool"}); err != nil {
		t.Fatal(err)
	}
}

func TestMessageLimitRegenerationAndIsolation(t *testing.T) {
	db := openTestDB(t).WithRecordLimits(-1, 2)
	ctx := context.Background()
	a, b := mkUser("regen-a@test.io"), mkUser("regen-b@test.io")
	for _, u := range []*User{a, b} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	c := mkConv(t, db, a.ID, "thread")
	foreign := mkConv(t, db, b.ID, "foreign")
	_, draft, err := db.BeginSend(ctx, a.ID, c.ID, "hello", nil, 1, "original-generation")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddMessage(ctx, b.ID, &Message{ConversationID: foreign.ID, Role: "tool"}); err != nil {
		t.Fatal(err)
	}
	replacement, err := db.BeginTurnRegeneration(ctx, a.ID, c.ID, draft.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	// Regeneration at capacity is a replacement, not another charged row.
	if replacement.ID == draft.ID {
		t.Fatal("draft not replaced")
	}
	if _, _, err := db.BeginSend(ctx, a.ID, c.ID, "over", nil, 1, ""); !errors.Is(err, ErrMessageLimit) {
		t.Fatalf("extra send: %v", err)
	}
	// A lower newly configured cap cannot destroy the retired turn on refusal.
	lower := db.WithRecordLimits(-1, 1)
	if _, err := lower.BeginTurnRegeneration(ctx, a.ID, c.ID, replacement.ID, ""); !errors.Is(err, ErrMessageLimit) {
		t.Fatalf("lowered cap: %v", err)
	}
	if _, err := db.GetMessageForUser(ctx, a.ID, replacement.ID); err != nil {
		t.Fatalf("refused regeneration lost old turn: %v", err)
	}
	// Ownership must fail before quota disclosure.
	if err := db.AddMessage(ctx, b.ID, &Message{ConversationID: c.ID, Role: "user"}); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("foreign message: %v", err)
	}
}

func TestRecordLimitQueriesUseCoveringIndexes(t *testing.T) {
	db := openTestDB(t)
	for name, query := range map[string]string{"conversations": conversationLimitQuery, "messages": messageLimitQuery} {
		t.Run(name, func(t *testing.T) {
			rows, err := db.sqlDB.Query("EXPLAIN QUERY PLAN "+query, "user", 10)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(details, "\n")
			t.Log(plan)
			if !strings.Contains(plan, "SEARCH") || !strings.Contains(plan, "COVERING INDEX idx_conv_user_") {
				t.Fatalf("missing indexed owner search: %s", plan)
			}
			if name == "messages" && !strings.Contains(plan, "SEARCH m USING COVERING INDEX idx_messages_conv") {
				t.Fatalf("message rows not covered: %s", plan)
			}
			// Scanning the bounded subquery's result is expected; base-table scans are not.
			for _, alias := range []string{"c", "m", "conversations", "messages"} {
				if strings.Contains(plan, fmt.Sprintf("SCAN %s ", alias)) {
					t.Fatalf("base table scan: %s", plan)
				}
			}
		})
	}
}

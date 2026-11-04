package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"mullsmith/internal/config"
	"mullsmith/internal/llm"
	"mullsmith/internal/store"
	"mullsmith/internal/tools"
)

func chatFixture(t *testing.T) (*Service, *store.DB, *store.Conversation) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Database.Dir = t.TempDir()
	cfg.Scratchpad.Root = t.TempDir()
	cfg.LLM.Endpoint = "http://127.0.0.1:1/v1"
	cfg.LLM.Models = []config.LLMModel{{ID: "test", Default: true}}
	cfg.AppLimits.ChatMessagesPerHour = -1
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user := &store.User{ID: "user", Status: "active", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	conv := &store.Conversation{UserID: user.ID}
	if err := db.CreateConversation(context.Background(), conv); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, db, logger, tools.BuildFor(cfg, logger, nil, nil, nil)), db, conv
}

func TestToolTimeoutsHonorConfiguration(t *testing.T) {
	s, _, _ := chatFixture(t)
	s.cfg.Tools.WebFetch.Timeout = config.Duration(90 * time.Second)
	s.cfg.Tools.WebDownload.Timeout = config.Duration(2 * time.Minute)
	for name, want := range map[string]time.Duration{"web_fetch": 95 * time.Second, "web_download": 125 * time.Second, "mcp_docs_search": 90 * time.Second, "fs_read": 30 * time.Second} {
		if got := s.execTimeout(name); got != want {
			t.Errorf("%s timeout %v, want %v", name, got, want)
		}
	}
}

func TestUnknownToolRefusalIsDurableAndRegenerable(t *testing.T) {
	s, db, conv := chatFixture(t)
	ctx := context.Background()
	op, err := s.Begin(ctx, "user", Command{ConversationID: conv.ID, Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	if err := s.finishMessage(op.draft.ID, "trying a tool", "complete", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	call := llm.ChatToolCall{ID: "canonical-call", Function: llm.ChatToolCallFunc{Name: `unknown"tool`, Arguments: `{}`}}
	var end ToolFinished
	content, _, err := s.runToolCall(ctx, op.generation, sink{emit: func(e Event) error {
		if event, ok := e.(ToolFinished); ok {
			end = event
		}
		return nil
	}}, "user", conv.ID, op.draft.ID, call, false)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(content)) || end.Status != "error" {
		t.Fatalf("invalid refusal: %s / %+v", content, end)
	}
	hist, err := db.ContextHistory(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 || len(hist[1].Calls) != 1 || hist[2].ToolCallID != call.ID || hist[2].Content != content {
		t.Fatalf("not durable: %+v", hist)
	}
	if _, err := db.BeginTurnRegeneration(ctx, "user", conv.ID, op.draft.ID, "next-generation"); err != nil {
		t.Fatalf("stopped tool turn cannot regenerate: %v", err)
	}
}

func TestOperationReservationAndDisconnectPersistence(t *testing.T) {
	service, db, conv := chatFixture(t)
	ctx := context.Background()
	op, err := service.Begin(ctx, "user", Command{ConversationID: conv.ID, Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	if _, err := service.Begin(ctx, "user", Command{ConversationID: conv.ID, Content: "overlap"}); !errors.Is(err, ErrGenerating) {
		t.Fatalf("overlap: %v", err)
	}
	rows, _, err := db.ListMessages(ctx, "user", conv.ID, "", 100)
	if err != nil || len(rows) != 2 {
		t.Fatalf("accepted rows=%v err=%v", rows, err)
	}
	// A consumer failing on acceptance must finalize the durable draft even
	// though there is no usable transport and no model call should be made.
	op.Run(func(event Event) error {
		if start, ok := event.(RoundStarted); !ok || !start.Initial {
			t.Fatalf("first event=%#v", event)
		}
		return errors.New("consumer disconnected")
	})
	rows, _, err = db.ListMessages(ctx, "user", conv.ID, "", 100)
	if err != nil || len(rows) != 2 || rows[1].Status != "aborted" {
		t.Fatalf("final rows=%v err=%v", rows, err)
	}
	if service.Generations.Count() != 0 {
		t.Fatal("reservation leaked")
	}
	op.Close() // also safe for the adapter's deferred cleanup
}

func TestRejectedOperationReleasesReservationWithoutWrites(t *testing.T) {
	service, db, conv := chatFixture(t)
	invalid := "does-not-exist"
	_, err := service.Begin(context.Background(), "user", Command{ConversationID: conv.ID, Content: "hello", Thinking: &invalid})
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Code != "invalid_thinking_choice" {
		t.Fatalf("refusal=%v", err)
	}
	if service.Generations.Count() != 0 {
		t.Fatal("reservation leaked on refusal")
	}
	rows, _, err := db.ListMessages(context.Background(), "user", conv.ID, "", 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("refused send wrote rows=%v err=%v", rows, err)
	}
}

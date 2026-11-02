package store

import (
	"context"
	"testing"
)

func TestFinishToolCallWithReservedResultIsAtomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := mkUser("reserved-tool@test.io")
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	c := mkConv(t, db, u.ID, "tools")
	assistant := &Message{ConversationID: c.ID, Role: "assistant"}
	if err := db.AddMessage(ctx, u.ID, assistant); err != nil {
		t.Fatal(err)
	}
	call := &ToolCall{MessageID: assistant.ID, LLMCallID: "call-1", Tool: "fs_write", Arguments: `{}`, Status: "running"}
	if err := db.AddToolCall(ctx, call); err != nil {
		t.Fatal(err)
	}
	reserved := &Message{ConversationID: c.ID, Role: "tool", ToolCallID: call.LLMCallID,
		Content: `{"error":"interrupted"}`}
	if err := db.AddMessage(ctx, u.ID, reserved); err != nil {
		t.Fatal(err)
	}

	if err := db.FinishToolCallWithResult(ctx, call.ID, reserved.ID, "ok", `{"written":1}`,
		`{"summary":"wrote"}`, 7, 3); err != nil {
		t.Fatal(err)
	}
	var callStatus, callResult, messageContent string
	var messageTokens int64
	if err := db.sqlDB.QueryRow(`SELECT status, result FROM tool_calls WHERE id = ?`, call.ID).
		Scan(&callStatus, &callResult); err != nil {
		t.Fatal(err)
	}
	if err := db.sqlDB.QueryRow(`SELECT content, token_count FROM messages WHERE id = ?`, reserved.ID).
		Scan(&messageContent, &messageTokens); err != nil {
		t.Fatal(err)
	}
	if callStatus != "ok" || callResult != `{"written":1}` || messageContent != callResult || messageTokens != 3 {
		t.Fatalf("call=%q/%q message=%q/%d", callStatus, callResult, messageContent, messageTokens)
	}

	wrong := &Message{ConversationID: c.ID, Role: "tool", ToolCallID: "another-call", Content: "fallback"}
	if err := db.AddMessage(ctx, u.ID, wrong); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishToolCallWithResult(ctx, call.ID, wrong.ID, "error", "changed", "", 9, 4); err != ErrRowGone {
		t.Fatalf("mismatched result error = %v, want ErrRowGone", err)
	}
	// The failed pair update rolls the tool-call update back too.
	if err := db.sqlDB.QueryRow(`SELECT status, result FROM tool_calls WHERE id = ?`, call.ID).
		Scan(&callStatus, &callResult); err != nil {
		t.Fatal(err)
	}
	if callStatus != "ok" || callResult != `{"written":1}` {
		t.Fatalf("failed atomic update changed call to %q/%q", callStatus, callResult)
	}
}

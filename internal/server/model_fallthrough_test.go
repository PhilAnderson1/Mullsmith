package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"mullsmith/internal/store"
)

// The endpoint moved on (e.g. LM Studio's qwen3.6 chat replaced by an
// other-host deployment offering one model). Conversations created back
// then still store the dead name; it must not resurface in the picker or
// ride the wire — they ride the current default instead. The stored
// column survives untouched, so a config that re-adds the model adopts
// it back.
func TestRemovedModelConversationRidesDefault(t *testing.T) {
	var mu sync.Mutex
	var wireModels []string
	ts := newFakeLLM(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		wireModels = append(wireModels, body.Model)
		mu.Unlock()
		scriptReply("Back on the default.").ServeHTTP(w, r)
	})
	cfg := chatCfg(t, ts.URL) // models: [m1 (default)]
	a := signedInChat(t, cfg)
	owner := ownerOf(t, a)

	conv := &store.Conversation{UserID: owner.ID, Title: "Keeper", Model: "qwen3.6-35b-a3b"}
	if err := a.db.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	// The list (and thus the dropdown) shows the default, not the ghost.
	resp := a.do(http.MethodGet, "/api/v1/conversations", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list: %d %s", resp.StatusCode, dumpBody(resp))
	}
	var page struct {
		Items []convResp `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	var listed *convResp
	for i := range page.Items {
		if page.Items[i].ID == conv.ID {
			listed = &page.Items[i]
		}
	}
	if listed == nil || listed.Model != "m1" {
		t.Fatalf("listed conversation = %+v, want it on m1", listed)
	}

	// And continuing the chat asks the endpoint for the default.
	r := sendChat(t, a, conv.ID, "continue where we left off")
	newSSEReader(t, r).untilDone()
	mu.Lock()
	defer mu.Unlock()
	for _, m := range wireModels {
		if m != "m1" {
			t.Fatalf("wire model = %v, want every request on m1", wireModels)
		}
	}

	// The stored column is untouched: config restores the model later,
	// the conversation is on it again.
	got, err := a.db.ConversationForUser(context.Background(), owner.ID, conv.ID)
	if err != nil {
		t.Fatalf("stored lookup: %v", err)
	}
	if got.Model != "qwen3.6-35b-a3b" {
		t.Errorf("stored model = %q, want the original name kept", got.Model)
	}
}

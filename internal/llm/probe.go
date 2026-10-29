// Package llm holds the OpenAI-compatible client for the inference endpoint
// (spec §6). M0 ships only the reachability probe used by -check-config; the
// streaming client arrives in M3.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"mullsmith/internal/config"
)

// Model is one entry of the GET /models listing. ContextLength is the
// endpoint-reported window (llama.cpp-family servers spell it
// "context_length"); 0 means the server did not report one — the field is
// not part of the OpenAI spec, so absence is normal, not an error.
type Model struct {
	ID            string
	ContextLength int
}

// ProbeModels checks endpoint reachability via GET /models and returns what
// the listing says. The API key is sent as a header only — never in the
// URL — and never included in error messages (spec §9.12).
func ProbeModels(ctx context.Context, cfg *config.Config) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	u := strings.TrimRight(cfg.LLM.Endpoint, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("bad endpoint URL: %w", err)
	}
	if cfg.LLM.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.LLM.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s failed: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned HTTP %d", u, resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"` // llama.cpp-family spelling
			MaxModelLen   int    `json:"max_model_len"`  // vLLM spelling
			Meta          struct {
				NCtx int `json:"n_ctx"` // older llama.cpp meta spelling
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("models response malformed: %w", err)
	}
	models := make([]Model, 0, len(payload.Data))
	for _, m := range payload.Data {
		window := m.ContextLength
		if window == 0 {
			window = m.MaxModelLen
		}
		if window == 0 {
			window = m.Meta.NCtx
		}
		models = append(models, Model{ID: m.ID, ContextLength: window})
	}
	return models, nil
}

// Probe returns just the model ids of ProbeModels.
func Probe(ctx context.Context, cfg *config.Config) ([]string, error) {
	models, err := ProbeModels(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// ContextLengthFor returns the reported window of one model id, and whether
// the listing carried one at all.
func ContextLengthFor(models []Model, want string) (int, bool) {
	for _, m := range models {
		if m.ID == want && m.ContextLength > 0 {
			return m.ContextLength, true
		}
	}
	return 0, false
}

// HasModel reports whether the given model id appears in ids.
func HasModel(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

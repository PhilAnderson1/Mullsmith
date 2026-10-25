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

	"gabkeep/internal/config"
)

// Probe checks endpoint reachability via GET /models and returns the model
// ids it reports. The API key is sent as a header only — never in the URL —
// and never included in error messages (spec §9.12).
func Probe(ctx context.Context, cfg *config.Config) ([]string, error) {
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
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("models response malformed: %w", err)
	}
	ids := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
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

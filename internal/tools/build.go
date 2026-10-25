package tools

import (
	"log/slog"
	"net/http"
	"time"

	"gabkeep/internal/config"
)

// BuildFor assembles the registry from config (§7.1: registration is driven
// by tools.*.enabled). Disabled or misconfigured tools are skipped with a
// log line — the chat still runs, just without that capability (§14:
// search stays disabled until the key exists).
func BuildFor(cfg *config.Config, logger *slog.Logger, searchClient *http.Client) *Registry {
	reg := NewRegistry(func(name string) string {
		if p, ok := cfg.Tools.Agent.Approval[name]; ok {
			return p
		}
		return ApprovalConfirm // unknown key ⇒ fail closed
	})

	if cfg.Tools.WebSearch.Enabled {
		switch cfg.Tools.WebSearch.Provider {
		case "brave":
			switch {
			case cfg.Tools.WebSearch.APIKey == "":
				logger.Warn("web_search disabled: tools.web_search.api_key / GABKEEP_SEARCH_API_KEY not set")
			default:
				reg.Register(NewBraveSearch(cfg.Tools.WebSearch.APIKey,
					cfg.Tools.WebSearch.MaxResults, searchClient))
			}
		case "":
			logger.Warn("web_search enabled but tools.web_search.provider is empty; set \"brave\"")
		default:
			logger.Warn("unsupported search provider; only brave is implemented",
				"provider", cfg.Tools.WebSearch.Provider)
		}
	}

	if cfg.Tools.WebFetch.Enabled {
		timeout := cfg.Tools.WebFetch.Timeout.D()
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		reg.Register(NewWebFetch(NewGuardedClient(timeout), int64(cfg.Tools.WebFetch.MaxBytes)))
	}

	if reg.Enabled() {
		logger.Info("tools enabled", "names", reg.Names())
	}
	return reg
}

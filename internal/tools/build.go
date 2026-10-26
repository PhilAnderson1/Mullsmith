package tools

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gabkeep/internal/config"
	"gabkeep/internal/runner"
)

// BuildFor assembles the registry from config (§7.1: registration is driven
// by tools.*.enabled). Disabled or misconfigured tools are skipped with a
// log line — the chat still runs, just without that capability (§14:
// search stays disabled until the key exists). The fs_* tools and
// files_share ride the scratchpad and are always available (sharing is a
// file copy, nothing to configure); code_run needs a Runner and is
// registered only when code execution is enabled (§9.9 layer 1: opt-in).
// quotaMu is the storage-quota lock shared with the server's upload
// commits (audit2 finding 3); nil serializes nothing (unit tests).
func BuildFor(cfg *config.Config, logger *slog.Logger, searchClient *http.Client,
	code runner.Runner, quotaMu *sync.Mutex,
) *Registry {
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

	// Scratchpad file tools (§7.2): always available, the root exists by
	// construction (config validation + EnsureDirs at startup).
	reg.Register(FsList{})
	reg.Register(FsRead{})
	reg.Register(FsWrite{UserQuota: int64(cfg.Scratchpad.UserQuota), QuotaMu: quotaMu})
	reg.Register(FsDelete{})
	reg.Register(FilesShare{UserQuota: int64(cfg.Scratchpad.UserQuota), QuotaMu: quotaMu})
	if cfg.Scratchpad.CodeExec.Enabled && code != nil {
		reg.Register(CodeRun{
			Runner:       code,
			Runtimes:     cfg.Scratchpad.CodeExec.AllowedRuntimes,
			Timeout:      cfg.Scratchpad.CodeExec.Timeout.D(),
			MaxOutput:    int(cfg.Scratchpad.CodeExec.MaxOutput),
			UserQuota:    int64(cfg.Scratchpad.UserQuota),
			AllowNetwork: cfg.Scratchpad.CodeExec.AllowNetwork,
			QuotaMu:      quotaMu,
		})
	}

	if reg.Enabled() {
		logger.Info("tools enabled", "names", reg.Names())
	}
	return reg
}

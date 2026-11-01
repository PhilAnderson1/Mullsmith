package tools

import (
	"log/slog"
	"net/http"

	"mullsmith/internal/config"
	"mullsmith/internal/managed"
	"mullsmith/internal/runner"
)

// BuildFor assembles the registry from config (§7.1: registration is driven
// by tools.*.enabled). Disabled or misconfigured tools are skipped with a
// log line — the chat still runs, just without that capability (§14:
// search stays disabled until the key exists). The fs_* tools and
// files_share ride the scratchpad and are always available (sharing is a
// file copy, nothing to configure); code_run needs a Runner and is
// registered only when code execution is enabled (§9.9 layer 1: opt-in).
// cfg is prepared by the application constructor. files owns the shared
// managed-write policy, including quota serialization.
func BuildFor(cfg *config.Config, logger *slog.Logger, searchClient *http.Client,
	code runner.Runner, files *managed.Service,
) *Registry {
	if files == nil {
		files = managed.New(int64(cfg.Scratchpad.UserQuota))
	}
	reg := NewRegistry(func(name string) string {
		if p, ok := cfg.Tools.Agent.Approval[name]; ok {
			return p
		}
		if name == ViewImageName {
			// Seeing the user's own picture is no more exposure than the
			// automatic upload embedding llm.vision already performs;
			// an approval prompt per look would only train reflexive
			// clicking. Config can still downgrade to confirm.
			return ApprovalAuto
		}
		return ApprovalConfirm // unknown key ⇒ fail closed
	})

	if cfg.Tools.WebSearch.Enabled {
		switch cfg.Tools.WebSearch.Provider {
		case "brave":
			switch {
			case cfg.Tools.WebSearch.APIKey == "":
				logger.Warn("web_search disabled: tools.web_search.api_key / MULLSMITH_SEARCH_API_KEY not set")
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
		reg.Register(NewWebFetch(NewGuardedClient(timeout), int64(cfg.Tools.WebFetch.MaxBytes)))
	}

	// Scratchpad file tools (§7.2): always available, the root exists by
	// construction (config validation + EnsureDirs at startup).
	reg.Register(FsList{})
	reg.Register(FsRead{})
	reg.Register(FsWrite{Files: files})
	reg.Register(FsDelete{})
	reg.Register(FilesShare{Files: files})
	// Seeing pictures is only a capability when some model can take
	// them (llm.models[].vision): with no entry flagged the tool is not
	// offered at all. Per conversation the offer is filtered further by
	// the model actually running (spec §7.2).
	if cfg.LLM.AnyVision() {
		reg.Register(ViewImage{MaxImageBytes: int64(cfg.LLM.VisionMaxImageBytes)})
	}
	if cfg.Scratchpad.CodeExec.Enabled && code != nil {
		reg.Register(CodeRun{
			Runner:       code,
			Runtimes:     cfg.Scratchpad.CodeExec.AllowedRuntimes,
			Timeout:      cfg.Scratchpad.CodeExec.Timeout.D(),
			MaxOutput:    int(cfg.Scratchpad.CodeExec.MaxOutput),
			Files:        files,
			AllowNetwork: cfg.Scratchpad.CodeExec.AllowNetwork,
		})
	}

	if reg.Enabled() {
		logger.Info("tools enabled", "names", reg.Names())
	}
	return reg
}

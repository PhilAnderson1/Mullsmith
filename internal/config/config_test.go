package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// minimalBase is the required-keys config minus the llm.models entry —
// tests that must spell their own model list (with per-entry thinking
// profiles, say) build on this one.
const minimalBase = `
server:
  listen: "127.0.0.1:8090"
database:
  dir: "./data/db"
scratchpad:
  root: "./data/scratchpad"
llm:
  endpoint: "http://127.0.0.1:8081/v1"
`

// minimalYAML adds the picker list: one entry, its default status left
// to the first-entry fallback so the fallback itself stays under test.
const minimalYAML = minimalBase + `  models:
    - id: "test-model"
`

// modelsYAML spells a full llm.models block for tests built on
// minimalBase: entries is the already-indented list body.
func modelsYAML(entries string) string { return "  models:\n" + entries }

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// repoCatalogueLine points a config at one shipped catalogue file by
// absolute path — a config under test lives in a temp directory, where
// relative paths resolve differently (the subject of
// TestLoadThinkingExtended instead).
func repoCatalogueLine(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "configs", name))
	if err != nil {
		t.Fatal(err)
	}
	switch name {
	case "endpoints.yaml":
		return fmt.Sprintf("  endpoints_file: %q\n", p)
	case "thinking-profiles.yaml":
		return fmt.Sprintf("  profiles_file: %q\n", p)
	}
	t.Fatalf("unknown catalogue %q", name)
	return ""
}

// catalogueTail carries both shipped catalogue files.
func catalogueTail(t *testing.T) string {
	t.Helper()
	return repoCatalogueLine(t, "endpoints.yaml") + repoCatalogueLine(t, "thinking-profiles.yaml")
}

func TestLoadMinimalAndDefaults(t *testing.T) {
	cfg, err := Load(writeCfg(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.Registration != "invite-only" {
		t.Errorf("registration default = %q, want invite-only", cfg.Auth.Registration)
	}
	if cfg.Server.MaxBodyBytes != 1<<20 {
		t.Errorf("max_body_bytes default = %d", cfg.Server.MaxBodyBytes)
	}
	if cfg.Auth.Argon2.Memory != 65536 {
		t.Errorf("argon2 memory default = %d", cfg.Auth.Argon2.Memory)
	}
	// The global open-mode sign-up budget (audit1 M-2): absent means 5.
	if got := cfg.Auth.SignupsPerHour(); got != 5 {
		t.Errorf("max_signups_per_hour default = %d, want 5", got)
	}
	if cfg.Tools.Agent.MaxToolRounds != 20 {
		t.Errorf("max_tool_rounds default = %d, want 20", cfg.Tools.Agent.MaxToolRounds)
	}
	if cfg.Tools.Agent.MaxToolCallsPerTurn != 100 {
		t.Errorf("max_tool_calls_per_turn default = %d, want 100", cfg.Tools.Agent.MaxToolCallsPerTurn)
	}
	if cfg.Tools.Agent.ToolCards != "useful" {
		t.Errorf("tool_cards default = %q, want useful", cfg.Tools.Agent.ToolCards)
	}
	if cfg.Tools.WebDownload.Enabled || cfg.Tools.WebDownload.MaxBytes != 1<<30 || cfg.Tools.WebDownload.Timeout != Duration(time.Minute) {
		t.Errorf("web_download defaults = %+v", cfg.Tools.WebDownload)
	}
	if cfg.Tools.Agent.Approval["code_run"] != "confirm" {
		t.Errorf("code_run approval default = %q, want confirm", cfg.Tools.Agent.Approval["code_run"])
	}
	if cfg.Tools.Agent.Approval["fs_delete"] != "confirm" ||
		cfg.Tools.Agent.Approval["display_image"] != "auto" || cfg.Tools.Agent.Approval["view_image"] != "auto" {
		t.Errorf("file/image approval defaults = %q / %q / %q, want confirm / auto / auto",
			cfg.Tools.Agent.Approval["fs_delete"], cfg.Tools.Agent.Approval["display_image"],
			cfg.Tools.Agent.Approval["view_image"])
	}
	// write_timeout defaults ON (astra2 finding 1): 60s, passed to the
	// http.Server unchanged; the bare -1 stays the explicit off-switch,
	// now alongside hsts_max_age, the file's remaining off-by-default flag.
	if cfg.Server.WriteTimeout != Duration(60*time.Second) {
		t.Errorf("write_timeout default = %v, want 60s", cfg.Server.WriteTimeout.D())
	}
	if cfg.Server.WriteTimeout.HTTPDeadline() != 60*time.Second {
		t.Errorf("write_timeout HTTPDeadline = %v, want 60s", cfg.Server.WriteTimeout.HTTPDeadline())
	}
	if cfg.Server.HSTSMaxAge != -1 {
		t.Errorf("hsts_max_age default = %d, want -1 off-sentinel", cfg.Server.HSTSMaxAge)
	}
	if len(cfg.LLM.Models) != 1 || cfg.LLM.Models[0].ID != "test-model" || !cfg.LLM.Models[0].Default {
		t.Errorf("models default = %+v, want test-model marked default by the first-entry fallback", cfg.LLM.Models)
	}
}

func TestMCPConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(writeCfg(t, minimalYAML+`mcp:
  servers:
    - id: "docs"
      transport: "streamable_http"
      url: "https://example.org/mcp"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.MCP.Servers[0]; got.Name != "docs" || got.Approval != "confirm" {
		t.Fatalf("MCP defaults = %+v", got)
	}
	if got, want := cfg.MCP.StateDir, filepath.Join(filepath.Dir(cfg.Scratchpad.Root), "mcp"); got != want {
		t.Fatalf("MCP state_dir = %q, want %q", got, want)
	}

	t.Setenv("MCP_TEST_TOKEN", "bearer-secret")
	authenticated, err := Load(writeCfg(t, minimalYAML+`mcp:
  servers:
    - id: "private"
      transport: "streamable_http"
      url: "https://example.org/mcp"
      bearer_token: "${MCP_TEST_TOKEN}"
      headers:
        X-API-Key: "header-secret"
`))
	if err != nil {
		t.Fatalf("Load authenticated HTTP: %v", err)
	}
	if got := authenticated.MCP.Servers[0]; got.BearerToken != "bearer-secret" || got.Headers["X-API-Key"] != "header-secret" {
		t.Fatalf("authenticated MCP config = %+v", got)
	}

	stdio, err := Load(writeCfg(t, minimalYAML+`mcp:
  servers:
    - id: "local"
      transport: "stdio"
      command: "/usr/local/bin/local-mcp-server"
      args: ["--stdio"]
      environment:
        EXAMPLE_TOKEN: "value"
`))
	if err != nil {
		t.Fatalf("Load stdio: %v", err)
	}
	if got := stdio.MCP.Servers[0]; got.StateDir != filepath.Join(stdio.MCP.StateDir, "local") {
		t.Fatalf("stdio StateDir = %q, root %q", got.StateDir, stdio.MCP.StateDir)
	}

	tests := []struct {
		name string
		body string
		want string
	}{
		{"duplicate id", `
mcp:
  servers:
    - {id: "docs", transport: "streamable_http", url: "https://one.example/mcp"}
    - {id: "docs", transport: "streamable_http", url: "https://two.example/mcp"}
`, "duplicate id"},
		{"bad id", `
mcp:
  servers:
    - {id: "Bad ID", transport: "streamable_http", url: "https://example.org/mcp"}
`, "use lowercase"},
		{"credentials", `
mcp:
  servers:
    - {id: "docs", transport: "streamable_http", url: "https://user:pass@example.org/mcp"}
`, "embedded credentials"},
		{"approval", `
mcp:
  servers:
    - {id: "docs", transport: "streamable_http", url: "https://example.org/mcp", approval: "sometimes"}
`, "must be auto or confirm"},
		{"relative stdio command", `
mcp:
  servers:
    - {id: "local", transport: "stdio", command: "local-mcp-server"}
`, "absolute executable path"},
		{"stdio url", `
mcp:
  servers:
    - {id: "local", transport: "stdio", command: "/usr/bin/local-mcp-server", url: "https://example.org/mcp"}
`, "not valid for stdio"},
		{"http command", `
mcp:
  servers:
    - {id: "docs", transport: "streamable_http", url: "https://example.org/mcp", command: "/bin/true"}
`, "only valid for stdio"},
		{"reserved stdio environment", `
mcp:
  servers:
    - id: "local"
      transport: "stdio"
      command: "/usr/bin/local-mcp-server"
      environment: {HOME: "/tmp"}
`, "HOME is managed by Mullsmith"},
		{"stdio HTTP credentials", `
mcp:
  servers:
    - id: "local"
      transport: "stdio"
      command: "/usr/bin/local-mcp-server"
      bearer_token: "secret"
`, "only valid for streamable_http"},
		{"bearer whitespace", `
mcp:
  servers:
    - {id: "docs", transport: "streamable_http", url: "https://example.org/mcp", bearer_token: "bad token"}
`, "without whitespace"},
		{"invalid header name", `
mcp:
  servers:
    - id: "docs"
      transport: "streamable_http"
      url: "https://example.org/mcp"
      headers: {"Bad Header": "secret"}
`, "invalid header name"},
		{"unsafe header", `
mcp:
  servers:
    - id: "docs"
      transport: "streamable_http"
      url: "https://example.org/mcp"
      headers: {X-Forwarded-For: "127.0.0.1"}
`, "unsafe to override"},
		{"authorization header", `
mcp:
  servers:
    - id: "docs"
      transport: "streamable_http"
      url: "https://example.org/mcp"
      headers: {authorization: "Basic secret"}
`, "controlled by Mullsmith"},
		{"duplicate normalized header", `
mcp:
  servers:
    - id: "docs"
      transport: "streamable_http"
      url: "https://example.org/mcp"
      headers: {X-API-Key: "one", x-api-key: "two"}
`, "duplicate header"},
		{"header newline", `
mcp:
  servers:
    - id: "docs"
      transport: "streamable_http"
      url: "https://example.org/mcp"
      headers:
        X-API-Key: "line1\nline2"
`, "control characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, minimalYAML+tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestToolCardModeValidation(t *testing.T) {
	_, err := Load(writeCfg(t, minimalYAML+`tools:
  agent:
    tool_cards: "sometimes"
`))
	if err == nil || !strings.Contains(err.Error(), "tools.agent.tool_cards: must be useful or all") {
		t.Fatalf("Load error = %v", err)
	}
}

// TestCapSyntax pins the house syntax for every optional cap, wherever it
// lives: positive → that value; -1 → disables the control (stored verbatim,
// consumers read "off"); absent → the code default; 0 spelled out (which
// would read as "allow nothing" on knobs phrased as maxima) and every other
// negative → startup error naming the syntax.
func TestCapSyntax(t *testing.T) {
	knobs := []struct {
		name string // full key path, exactly as the error must name it
		yaml string // one-key block, %d for the value under test
		read func(*Config) int
		def  int
	}{
		{"app_limits.max_conversations_per_user", "app_limits:\n  max_conversations_per_user: %d\n",
			func(c *Config) int { return c.AppLimits.MaxConversationsPerUser }, DefaultMaxConversationsPerUser},
		{"app_limits.max_messages_per_user", "app_limits:\n  max_messages_per_user: %d\n",
			func(c *Config) int { return c.AppLimits.MaxMessagesPerUser }, DefaultMaxMessagesPerUser},
		{"app_limits.concurrent_streams_per_user", "app_limits:\n  concurrent_streams_per_user: %d\n",
			func(c *Config) int { return c.AppLimits.ConcurrentStreamsPerUser }, DefaultConcurrentStreamsUser},
		{"app_limits.concurrent_streams_total", "app_limits:\n  concurrent_streams_total: %d\n",
			func(c *Config) int { return c.AppLimits.ConcurrentStreamsTotal }, DefaultConcurrentStreamsTotal},
		{"app_limits.requests_per_minute", "app_limits:\n  requests_per_minute: %d\n",
			func(c *Config) int { return c.AppLimits.RequestsPerMinute }, DefaultRequestsPerMinute},
		{"app_limits.chat_messages_per_hour", "app_limits:\n  chat_messages_per_hour: %d\n",
			func(c *Config) int { return c.AppLimits.ChatMessagesPerHour }, DefaultChatMessagesPerHour},
		{"app_limits.hash_concurrency", "app_limits:\n  hash_concurrency: %d\n",
			func(c *Config) int { return c.AppLimits.HashConcurrency }, DefaultHashConcurrency},
		{"server.max_connections", "server:\n  max_connections: %d\n",
			func(c *Config) int { return c.Server.MaxConnections }, DefaultMaxConnections},
		{"server.concurrent_requests", "server:\n  concurrent_requests: %d\n",
			func(c *Config) int { return c.Server.ConcurrentRequests }, DefaultConcurrentRequests},
		{"app_limits.concurrent_downloads_per_user", "app_limits:\n  concurrent_downloads_per_user: %d\n",
			func(c *Config) int { return c.AppLimits.ConcurrentDownloadsPerUser }, DefaultConcurrentDownloadsPerUser},
		{"app_limits.concurrent_uploads_per_user", "app_limits:\n  concurrent_uploads_per_user: %d\n",
			func(c *Config) int { return c.AppLimits.ConcurrentUploadsPerUser }, DefaultConcurrentUploadsPerUser},
		{"app_limits.concurrent_uploads", "app_limits:\n  concurrent_uploads: %d\n",
			func(c *Config) int { return c.AppLimits.ConcurrentUploads }, DefaultConcurrentUploads},
		{"auth.max_signups_per_hour", "auth:\n  max_signups_per_hour: %d\n",
			func(c *Config) int { return c.Auth.MaxSignupsPerHour }, defaultMaxSignupsPerHour},
	}
	for _, k := range knobs {
		t.Run(k.name, func(t *testing.T) {
			load := func(v int) (*Config, error) {
				snippet := fmt.Sprintf(k.yaml, v)
				body := minimalYAML
				if strings.HasPrefix(snippet, "server:\n") {
					body = strings.Replace(body, "server:\n", snippet, 1)
				} else {
					body += snippet
				}
				return Load(writeCfg(t, body))
			}
			// absent → code default
			cfg, err := Load(writeCfg(t, minimalYAML))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := k.read(cfg); got != k.def {
				t.Errorf("absent: %d, want default %d", got, k.def)
			}
			// positive → the value
			if cfg, err := load(7); err != nil || k.read(cfg) != 7 {
				t.Errorf("positive 7: %v, %v", k.read(cfg), err)
			}
			// -1 → off sentinel, stored verbatim
			if cfg, err := load(-1); err != nil || k.read(cfg) != -1 {
				t.Errorf("off sentinel -1: %v, %v", k.read(cfg), err)
			}
			// 0 spelled out → startup error naming the syntax
			_, err = load(0)
			if err == nil || !strings.Contains(err.Error(), k.name) ||
				!strings.Contains(err.Error(), "-1 to disable") {
				t.Errorf("explicit 0: err = %v, want rejection naming %q and the -1 syntax", err, k.name)
			}
			// other negatives → same
			_, err = load(-2)
			if err == nil || !strings.Contains(err.Error(), k.name) {
				t.Errorf("negative -2: err = %v, want rejection naming %q", err, k.name)
			}
		})
	}

	// The sign-up budget's consumer-side read of the sentinel: -1 (off)
	// resolves to 0 so the handler skips the check entirely.
	cfg, err := Load(writeCfg(t, minimalYAML+"auth:\n  max_signups_per_hour: -1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Auth.SignupsPerHour(); got != 0 {
		t.Errorf("SignupsPerHour() with -1 = %d, want 0 (no budget)", got)
	}
}

// The two off-switches outside limits follow the house syntax too (§2.2):
// the flag is the bare -1 on every knob, durations included — "-1s" is
// not a spelling of off, it is a typo. Absent means the default, which
// for both knobs IS off, in the same sentinel spelling.
func TestOffSwitchesOutsideCaps(t *testing.T) {
	at := func(extra string) error {
		body := strings.Replace(minimalYAML, `  listen: "127.0.0.1:8090"`,
			"  listen: \"127.0.0.1:8090\"\n"+extra, 1)
		_, err := Load(writeCfg(t, body))
		return err
	}
	for _, tc := range []struct {
		key  string
		body string
		want string // "" = must load; else a substring the error must name
	}{
		{"write_timeout", "  write_timeout: -1\n", ""},
		{"write_timeout", "  write_timeout: 30s\n", ""},
		{"write_timeout", "  write_timeout: -1s\n", "-1 for no write cap"}, // a duration, not the flag
		{"write_timeout", "  write_timeout: -2s\n", "server.write_timeout"},
		{"write_timeout", "  write_timeout: 0s\n", "write -1"},
		{"write_timeout", "  write_timeout: 0\n", "write -1"}, // numbers reach the same gate
		{"hsts_max_age", "  hsts_max_age: -1\n", ""},
		{"hsts_max_age", "  hsts_max_age: 600\n", ""},
		{"hsts_max_age", "  hsts_max_age: 0\n", "server.hsts_max_age"},
		{"hsts_max_age", "  hsts_max_age: -2\n", "server.hsts_max_age"},
	} {
		err := at(tc.body)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %s rejected: %v", tc.key, tc.body, err)
		case tc.want != "" && err == nil:
			t.Errorf("%s: %s accepted, want startup error naming the syntax", tc.key, tc.body)
		case tc.want != "" && !strings.Contains(err.Error(), tc.want):
			t.Errorf("%s: %s err = %v, want mention of %q", tc.key, tc.body, err, tc.want)
		}
	}
	// And the sentinel's meaning at the one Go boundary that cares:
	// http.Server must receive 0 (no deadline), never a negative.
	cfg, err := Load(writeCfg(t, strings.Replace(minimalYAML, `  listen: "127.0.0.1:8090"`,
		"  listen: \"127.0.0.1:8090\"\n  write_timeout: -1", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Server.WriteTimeout.HTTPDeadline(); d != 0 {
		t.Errorf("HTTPDeadline with -1 = %v, want 0", d)
	}
	// A knob with no off-switch must refuse the flag too.
	if err := at("  read_timeout: -1\n"); err == nil ||
		!strings.Contains(err.Error(), "server.read_timeout") {
		t.Errorf("read_timeout: -1 err = %v, want rejection naming the key", err)
	}
}

func TestRelativePathsResolveAgainstConfigDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "config.yaml")
	if err := os.WriteFile(p, []byte(minimalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(sub, "data", "db")
	if cfg.Database.Dir != want {
		t.Errorf("Database.Dir = %q, want %q", cfg.Database.Dir, want)
	}
	wantSp := filepath.Join(sub, "data", "scratchpad")
	if cfg.Scratchpad.Root != wantSp {
		t.Errorf("Scratchpad.Root = %q, want %q", cfg.Scratchpad.Root, wantSp)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("MULLSMITH_TEST_KEY", "sekret")

	load := func(body string) (*Config, error) { return Load(writeCfg(t, body)) }

	cfg, err := load(minimalYAML + `  # llm section is last; extra indented key extends it
`)
	if err == nil && cfg.LLM.APIKey != "" {
		t.Fatal("api_key unexpectedly set")
	}
	// (the above only checks comments don't break parsing; real cases below)

	// value substituted
	if _, err := load(replaceLastLLM(`  api_key: "${MULLSMITH_TEST_KEY}"`)); err != nil {
		t.Fatalf("substitution: %v", err)
	}
	cfg, err = load(replaceLastLLM(`  api_key: "${MULLSMITH_TEST_KEY}"`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.APIKey != "sekret" {
		t.Errorf("api_key = %q, want sekret", cfg.LLM.APIKey)
	}

	// :- default allowed when unset
	cfg, err = load(replaceLastLLM(`  api_key: "${MULLSMITH_UNSET_VAR:-}"`))
	if err != nil {
		t.Fatalf("default form: %v", err)
	}
	if cfg.LLM.APIKey != "" {
		t.Errorf("api_key = %q, want empty via default", cfg.LLM.APIKey)
	}

	// missing without default → error naming the variable
	_, err = load(replaceLastLLM(`  api_key: "${MULLSMITH_UNSET_VAR}"`))
	if err == nil || !strings.Contains(err.Error(), "MULLSMITH_UNSET_VAR") {
		t.Fatalf("expected missing-var error, got: %v", err)
	}
}

// replaceLastLLM appends lines to the trailing llm block of minimalYAML.
func replaceLastLLM(extra string) string {
	return minimalYAML + extra + "\n"
}

func TestUnknownKeyRejected(t *testing.T) {
	if _, err := Load(writeCfg(t, minimalYAML+"\nserver_extra: 1\n")); err == nil {
		t.Fatal("expected unknown-key error")
	}
	body := `
server:
  listen: "127.0.0.1:8090"
database:
  dir: ./data/db
scratchpad:
  root: ./data/scratchpad
llm:
  endpoint: "http://x/v1"
  models: [{id: m}]
  typo_field: true
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatal("expected nested unknown-key error")
	}
}

// The retired rate_limit: block must fail with a message naming the
// move, never silently win over or override app_limits:. (The caps' own
// syntax lives in TestCapSyntax.)
func TestRetiredRateLimitBlock(t *testing.T) {
	_, err := Load(writeCfg(t, minimalYAML+`
rate_limit:
  requests_per_minute: 300
`))
	if err == nil {
		t.Fatal("retired rate_limit accepted, want error")
	}
	if !strings.Contains(err.Error(), "rate_limit") ||
		!strings.Contains(err.Error(), "app_limits:") {
		t.Errorf("error = %v, want it to point at the app_limits: block", err)
	}
}

// llm.request_timeout is retired — no request path ever read it. An old
// config still spelling it must get a retirement message, not a silent
// no-op (or a raw unknown-key complaint from the strict decoder).
func TestRetiredRequestTimeout(t *testing.T) {
	_, err := Load(writeCfg(t, minimalYAML+"  request_timeout: 5m\n"))
	if err == nil {
		t.Fatal("retired request_timeout accepted, want error")
	}
	if !strings.Contains(err.Error(), "request_timeout") ||
		!strings.Contains(err.Error(), "retired") {
		t.Errorf("error = %v, want a retirement message", err)
	}
}

// TestShippedTemplateMirrorsCodeDefaults keeps configs/config.yaml honest:
// every policy value it spells out must equal the default the program
// applies when that key is absent (measured by Loading the minimal config).
// Only reasoned exceptions may differ — per-deployment values and the rare
// knob whose template value is an opt-in the zero-collapse cannot encode.
// Change a default? The template must follow in the same commit, or the
// path gets an explicit exception with a reason.
func TestShippedTemplateMirrorsCodeDefaults(t *testing.T) {
	tmpl, err := Load(filepath.Join("..", "..", "configs", "config.yaml"))
	if err != nil {
		t.Fatalf("Load template: %v", err)
	}
	mini, err := Load(writeCfg(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load minimal: %v", err)
	}

	exceptions := map[string]string{
		"server.listen":                   "deployment choice — template ships the production port",
		"server.tls.cert_file":            "deployment paths — template stages certbot's cert",
		"server.tls.key_file":             "deployment paths — template stages certbot's cert",
		"server.hsts_max_age":             "-1 (code default) means no HSTS header; the template ships one year as the TLS-era default",
		"database.dir":                    "required; per-machine path",
		"scratchpad.root":                 "required; per-machine path",
		"tools.code_run.allowed_runtimes": "host-specific interpreter paths — code default is the empty list",
		"llm.models":                      "required; the template holds a worked example (entries carry each model's default/thinking_profile)",
		"llm.api_key":                     "secret via ${ENV} expansion — environment-dependent at test time",
		"llm.default_temperature":         "zero is a meaningful LLM value, so zero-collapse cannot carry 0.7; code default is 0",
		"auth.registration":               "template explicitly opens trusted-LAN signup; omitted config fails safe to invite-only",
		"auth.remember_login":             "bool zero-collapse — template opts in; code default keeps the checkbox off",
		"tools.web_search.api_key":        "secret via ${ENV} expansion — environment-dependent at test time",
		"tools.web_fetch.enabled":         "bool zero-collapse — template opts into guarded egress; code default is off",
		"tools.web_download.enabled":      "bool zero-collapse — template opts into guarded downloads; code default is off",
		"email.smtp.password":             "secret via ${ENV} expansion — environment-dependent at test time",
		"email.smtp.from":                 "sender identity is operator- and domain-specific; the template shows the shape",
	}

	tmplLeaf, miniLeaf := map[string]any{}, map[string]any{}
	flattenLeaves("", reflect.ValueOf(*tmpl), tmplLeaf)
	flattenLeaves("", reflect.ValueOf(*mini), miniLeaf)
	if len(tmplLeaf) != len(miniLeaf) {
		t.Fatalf("leaf count mismatch %d vs %d — flattenLeaves out of date?", len(tmplLeaf), len(miniLeaf))
	}
	// Keys the template must NOT carry: retired knobs whose presence (with a
	// value) is itself a startup error — the template can only confuse.
	notInTemplate := map[string]bool{
		"rate_limit.requests_per_minute":    true,
		"rate_limit.chat_messages_per_hour": true,
		// Retired with automatic upload embedding (2026-10-29): view
		// turns carry exactly one picture, so there is no per-message
		// inline budget left to configure (§7.2).
		"llm.vision_max_images": true,
		// Retired with per-model picker entries: the choice list lives on
		// llm.models entries now, and legacyModelKeys rejects this spelling.
		"llm.thinking_profile": true,
		// Retired 2026-10-31: no request path ever read it (streaming has
		// no global client timeout by design; titling bounds itself).
		"llm.request_timeout": true,
	}
	for path, codeDefault := range miniLeaf {
		if _, ok := exceptions[path]; !ok && !leafEqual(codeDefault, tmplLeaf[path]) {
			t.Errorf("%s: template = %v, code default = %v — match them or list the path as a reasoned exception",
				path, tmplLeaf[path], codeDefault)
		}
		// Presence: every live key is spelled out in the template, so the
		// shipped file shows every knob and its default (the operator's
		// doctrine: same options in the file as wired into the code).
		present := rawHasPath(tmpl.rawDoc, path)
		if notInTemplate[path] {
			if present {
				t.Errorf("%s: retired key spelled out in the template — it must be absent", path)
			}
			continue
		}
		if !present {
			t.Errorf("%s: missing from configs/config.yaml — spell out the default or list the path as a reasoned exception", path)
		}
	}
}

// rawHasPath walks a dotted key path through the untyped YAML document.
func rawHasPath(doc map[string]any, path string) bool {
	here := doc
	segs := strings.Split(path, ".")
	for i, seg := range segs {
		v, ok := here[seg]
		if !ok {
			return false
		}
		if i == len(segs)-1 {
			return true
		}
		here, ok = v.(map[string]any)
		if !ok {
			return false
		}
	}
	return false
}

// flattenLeaves maps every yaml-tagged scalar (including slices and maps)
// to its value by dotted path; nested structs recurse. Unexported fields
// and `yaml:"-"` are skipped.
func flattenLeaves(prefix string, v reflect.Value, out map[string]any) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct { // Duration/Size are integer KINDS, not structs
			flattenLeaves(path, fv, out)
			continue
		}
		out[path] = fv.Interface()
	}
}

// leafEqual compares two config leaves; an absent (nil) and an empty
// list/map are the same policy.
func leafEqual(a, b any) bool {
	na, nb := 0, 0
	if s := reflect.ValueOf(a); s.Kind() == reflect.Slice || s.Kind() == reflect.Map {
		na = s.Len()
	}
	if s := reflect.ValueOf(b); s.Kind() == reflect.Slice || s.Kind() == reflect.Map {
		nb = s.Len()
	}
	if na == 0 && nb == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestValidationCases(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			"bad listen",
			`
server:
  listen: "nope"
database: {dir: ./db}
scratchpad: {root: ./sp}
llm: {endpoint: "http://x/v1", models: [{id: m}]}
`,
			"server.listen",
		},
		{
			// write_timeout has the house off-switch (the flag -1);
			// any OTHER negative is still a passed deadline — reject.
			"negative write_timeout",
			`
server:
  listen: "127.0.0.1:8090"
  write_timeout: -2s
database: {dir: ./db}
scratchpad: {root: ./sp}
llm: {endpoint: "http://x/v1", models: [{id: m}]}
`,
			"server.write_timeout",
		},
		{
			"negative shutdown_grace",
			`
server:
  listen: "127.0.0.1:8090"
  shutdown_grace: -15s
database: {dir: ./db}
scratchpad: {root: ./sp}
llm: {endpoint: "http://x/v1", models: [{id: m}]}
`,
			"server.shutdown_grace",
		},
		{
			"no model",
			`
database: {dir: ./db}
scratchpad: {root: ./sp}
llm: {endpoint: "http://x/v1"}
`,
			"llm.model",
		},
		{
			"maxtokens >= context",
			`
database: {dir: ./db}
scratchpad: {root: ./sp}
llm: {endpoint: "http://x/v1", models: [{id: m, context_size: 4096}], max_tokens_response: 8000}
`,
			"max_tokens_response",
		},
		{
			"bad provider",
			minimalYAML + "tools:\n  web_search:\n    provider: google\n",
			"tools.web_search.provider",
		},
		{
			"brave needs key",
			minimalYAML + "tools:\n  web_search:\n    enabled: true\n    provider: brave\n",
			"api_key",
		},
		{
			"bad approval",
			minimalYAML + "tools:\n  agent:\n    approval:\n      web_search: maybe\n",
			"approval",
		},
		{
			"unknown approval tool",
			minimalYAML + "tools:\n  agent:\n    approval:\n      web_serach: auto\n",
			"tools.agent.approval.web_serach: unknown tool",
		},
		{
			"negative tool call cap",
			minimalYAML + "tools:\n  agent:\n    max_tool_calls_per_turn: -1\n",
			"max_tool_calls_per_turn",
		},
		{
			"negative download size",
			minimalYAML + "tools:\n  web_download:\n    max_bytes: -1\n",
			"invalid size",
		},
		{
			"overflowing download size",
			minimalYAML + "tools:\n  web_download:\n    max_bytes: 9223372036854775807\n",
			"tools.web_download.max_bytes",
		},
		{
			"negative download timeout",
			minimalYAML + "tools:\n  web_download:\n    timeout: -1s\n",
			"tools.web_download.timeout",
		},
		{
			"registration enum",
			minimalYAML + "auth:\n  registration: yes-please\n",
			"auth.registration",
		},
		{
			"remember absolute shorter than sliding",
			minimalYAML + "auth:\n  remember_ttl: 720h\n  remember_absolute_ttl: 100h\n",
			"remember_absolute_ttl",
		},
		{
			"runtime binary must be absolute",
			`
database: {dir: ./db}
scratchpad:
  root: ./sp
tools:
  code_run:
    enabled: true
    allowed_runtimes:
      - { id: python3, binary: python3 }
llm: {endpoint: "http://x/v1", models: [{id: m}]}
`,
			"absolute path",
		},
		{
			"bad log level",
			minimalYAML + "logging:\n  level: shout\n",
			"logging.level",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			if err == nil {
				t.Fatalf("expected error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestEmailValidation(t *testing.T) {
	// A disabled (or absent) email section needs nothing else — the
	// minimalYAML in this file has no email: block and must keep loading.
	const base = minimalYAML + `email:
  enabled: true
  base_url: "https://mullsmith.example:3210"
  smtp:
    host: "mail.example"
    authenticated_smtp: true
    username: "mullsmith"
    from: "mullsmith@example"
`
	if _, err := Load(writeCfg(t, base)); err != nil {
		t.Fatalf("valid enabled config rejected: %v", err)
	}
	cases := []struct {
		name    string
		old     string // replaced inside the fixture (count 1)
		nw      string
		wantErr string
	}{
		{"needs host", `host: "mail.example"`, `host: ""`, "email.smtp.host"},
		{"needs from", `from: "mullsmith@example"`, `from: "mullsmith"`, "email.smtp.from"},
		{"needs base_url", `  base_url: "https://mullsmith.example:3210"`, "", "email.base_url"},
		{"base_url path", `https://mullsmith.example:3210"`, `https://x.example/app"`, "email.base_url"},
		{"base_url scheme", `https://mullsmith.example:3210"`, `ftp://x.example"`, "email.base_url"},
		{"auth needs username", `username: "mullsmith"`, `username: ""`, "email.smtp.username"},
		{"auth on port 25", `host: "mail.example"`, "host: \"mail.example\"\n    port: 25", "port 25"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(base, tc.old, tc.nw, 1)
			if body == base && tc.old != "" {
				t.Fatalf("fixture lacks %q", tc.old)
			}
			_, err := Load(writeCfg(t, body))
			if err == nil {
				t.Fatalf("expected error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}

	// Defaults + normalization: port fills to 587, trailing slash in
	// base_url is stripped (link building appends "/#reset?t=…").
	cfg, err := Load(writeCfg(t, base))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Email.SMTP.Port != 587 {
		t.Errorf("smtp port default = %d, want 587", cfg.Email.SMTP.Port)
	}
	cfg, err = Load(writeCfg(t, strings.Replace(base,
		"base_url: \"https://mullsmith.example:3210\"",
		"base_url: \"https://mullsmith.example:3210/\"", 1)))
	if err != nil {
		t.Fatalf("trailing-slash base_url: %v", err)
	}
	if cfg.Email.BaseURL != "https://mullsmith.example:3210" {
		t.Errorf("base_url = %q, want trailing slash stripped", cfg.Email.BaseURL)
	}

	// Unauthenticated port 25 (LAN relay) stays legal.
	body := strings.Replace(base, "authenticated_smtp: true", "authenticated_smtp: false", 1)
	body = strings.Replace(body, `host: "mail.example"`, "host: \"mail.example\"\n    port: 25", 1)
	if _, err := Load(writeCfg(t, body)); err != nil {
		t.Errorf("unauthenticated port 25 rejected: %v", err)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]Size{
		"25MiB":  25 << 20,
		"512KiB": 512 << 10,
		"1GiB":   1 << 30,
		"1024":   1024,
		"1.5MiB": 1.5 * (1 << 20),
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "1 MB", "MiB", "-3", "1MB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q): expected error", bad)
		}
	}
}

func TestTrustedProxiesParsing(t *testing.T) {
	body := `
server:
  listen: "127.0.0.1:8090"
  trusted_proxies: ["192.168.0.0/24", "10.0.0.5"]
database: {dir: ./db}
scratchpad: {root: ./sp}
llm: {endpoint: "http://x/v1", models: [{id: m}]}
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.TrustedNets) != 2 {
		t.Fatalf("TrustedNets = %d, want 2", len(cfg.TrustedNets))
	}
	inside, _ := netip.ParseAddr("192.168.0.7")
	if !cfg.TrustedNets[0].Contains(inside) {
		t.Error("192.168.0.7 should be inside 192.168.0.0/24")
	}
	bare, _ := netip.ParseAddr("10.0.0.5")
	if !cfg.TrustedNets[1].Contains(bare) {
		t.Error("bare IP proxy should contain itself")
	}
}

func TestWarnings(t *testing.T) {
	body := `
server:
  listen: "0.0.0.0:8090"
database: {dir: ./db}
scratchpad: {root: ./sp}
llm: {endpoint: "http://x/v1", models: [{id: m}]}
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	joined := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(joined, "without TLS") {
		t.Errorf("expected no-TLS warning, got: %s", joined)
	}

	// I-4: a default-route proxy list makes every client trusted.
	warns := func(proxies string) string {
		t.Helper()
		c, err := Load(writeCfg(t, strings.Replace(body,
			`  listen: "0.0.0.0:8090"`,
			`  listen: "0.0.0.0:8090"`+"\n"+`  trusted_proxies: ["`+proxies+`"]`, 1)))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return strings.Join(c.Warnings(), "\n")
	}
	if j := warns("0.0.0.0/0"); !strings.Contains(j, "X-Forwarded-For is honored") {
		t.Errorf("0.0.0.0/0 in trusted_proxies: want spoofing warning, got: %s", j)
	}
	if j := warns("::/0"); !strings.Contains(j, "X-Forwarded-For is honored") {
		t.Errorf("::/0 in trusted_proxies: want spoofing warning, got: %s", j)
	}
	if j := warns("192.168.0.0/24"); strings.Contains(j, "X-Forwarded-For is honored") {
		t.Errorf("honest proxy list must not warn, got: %s", j)
	}
}

func TestCheckIOWritable(t *testing.T) {
	cfg, err := Load(writeCfg(t, minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfg.CheckIO() {
		if !c.OK {
			t.Errorf("check %s failed: %s", c.Name, c.Detail)
		}
	}
	if cfg.Server.ReadTimeout.D() != 30*time.Second {
		t.Errorf("read_timeout default = %v", cfg.Server.ReadTimeout.D())
	}
}

func TestCheckIODoesNotCreateStdioStateAsCLIUser(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "mcp-state")
	cfg, err := Load(writeCfg(t, minimalYAML+fmt.Sprintf(`mcp:
  state_dir: %q
  servers:
    - id: "local"
      transport: "stdio"
      command: "/usr/local/bin/local-mcp-server"
`, stateDir)))
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range cfg.CheckIO() {
		if !check.OK {
			t.Fatalf("check %s failed: %s", check.Name, check.Detail)
		}
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("CheckIO created stdio state as the CLI user: %v", err)
	}
}

func TestLoadModelsDefaultAndValidation(t *testing.T) {
	// Unmarked list ⇒ the first entry is the default.
	cfg, err := Load(writeCfg(t, minimalBase+modelsYAML(
		"    - id: \"alpha\"\n    - id: \"beta\"\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.DefaultModelID() != "alpha" || cfg.LLM.ModelNames()[1] != "beta" {
		t.Errorf("default/first = %q / %v", cfg.LLM.DefaultModelID(), cfg.LLM.ModelNames())
	}

	// Marked entry wins over position.
	cfg, err = Load(writeCfg(t, minimalBase+modelsYAML(
		"    - id: \"alpha\"\n    - id: \"beta\"\n      default: true\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.DefaultModelID() != "beta" {
		t.Errorf("default = %q, want the marked beta", cfg.LLM.DefaultModelID())
	}

	// A single entry needs no marking at all.
	cfg, err = Load(writeCfg(t, minimalBase+modelsYAML("    - id: \"solo\"\n")))
	if err != nil || !cfg.LLM.Models[0].Default || cfg.LLM.DefaultModelID() != "solo" {
		t.Errorf("solo entry: err=%v default=%v", err, cfg.LLM.Models)
	}

	for _, tc := range []struct{ name, yaml string }{
		{"no models at all", minimalBase},
		{"two defaults claimed", minimalBase + modelsYAML(
			"    - id: \"a\"\n      default: true\n    - id: \"b\"\n      default: true\n")},
		{"duplicate ids", minimalBase + modelsYAML(
			"    - id: \"a\"\n    - id: \"a\"\n")},
		{"entry without id", minimalBase + modelsYAML("    - default: true\n")},
	} {
		if _, err := Load(writeCfg(t, tc.yaml)); err == nil {
			t.Errorf("%s accepted", tc.name)
		}
	}
}

// vision per entry: OFF unless the entry spells vision: true; models
// outside the picker read the default entry, like every per-model
// property; AnyVision is the registration gate for view_image.
func TestLoadVisionPerModel(t *testing.T) {
	cfg, err := Load(writeCfg(t, minimalBase+modelsYAML(
		"    - id: \"seer\"\n      vision: true\n    - id: \"blind\"\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.LLM.VisionFor("seer") || cfg.LLM.VisionFor("blind") {
		t.Errorf("vision flags = %+v", cfg.LLM.Models)
	}
	if !cfg.LLM.AnyVision() {
		t.Error("AnyVision must be true when one entry sees")
	}
	if cfg.AppLimits.VisionMaxImagesPerConversation != 8 {
		t.Errorf("vision image conversation cap = %d, want default 8", cfg.AppLimits.VisionMaxImagesPerConversation)
	}
	// Outside the picker → the default entry (here "seer" is default).
	if !cfg.LLM.VisionFor("somewhere-else") {
		t.Error("a model outside the picker must read the default entry")
	}
	cfg, err = Load(writeCfg(t, minimalBase+modelsYAML("    - id: \"blind\"\n")))
	if err != nil || cfg.LLM.AnyVision() || cfg.LLM.VisionFor("blind") {
		t.Errorf("no entry sees: err=%v AnyVision=%v", err, cfg.LLM.AnyVision())
	}
	if _, err := Load(writeCfg(t, minimalBase+
		modelsYAML("    - id: \"seer\"\n      vision: true\n")+
		"app_limits:\n  vision_max_images_per_conversation: -1\n")); err == nil {
		t.Error("negative vision_max_images_per_conversation accepted")
	}
}

// context_size per entry: a positive number pins; omitted or -1 (or any
// negative) normalises to the auto sentinel; a spelled 0 is the house
// cap-syntax error.
func TestLoadContextSizePerModel(t *testing.T) {
	cfg, err := Load(writeCfg(t, minimalBase+modelsYAML(
		"    - id: \"pinned\"\n      context_size: 131072\n    - id: \"auto\"\n    - id: \"minus-one\"\n      context_size: -1\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.ContextSizeFor("pinned") != 131072 ||
		cfg.LLM.Models[1].ContextSize != -1 || cfg.LLM.Models[2].ContextSize != -1 {
		t.Errorf("context sizes = %+v", cfg.LLM.Models)
	}
	if !cfg.LLM.AnyContextAuto() {
		t.Error("auto entries must keep discovery alive")
	}
	cfg, err = Load(writeCfg(t, minimalBase+modelsYAML("    - id: \"pinned\"\n      context_size: 131072\n")))
	if err != nil || cfg.LLM.AnyContextAuto() {
		t.Errorf("all-pinned config: err=%v anyAuto=%v", err, cfg.LLM.AnyContextAuto())
	}
	if _, err := Load(writeCfg(t, minimalBase+modelsYAML("    - id: m\n      context_size: 0\n"))); err == nil {
		t.Error("spelled context_size: 0 accepted")
	}
}

// The pre-picker spellings answer with the new shape, not a raw YAML
// complaint: `model:`, an llm-level `thinking_profile:`, and string
// items in `models:` are all migration messages.
func TestLoadLegacyModelKeys(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"llm.model", minimalBase + "  model: \"old-way\"\n", "has moved into llm.models"},
		{"llm.thinking_profile", minimalYAML + "  thinking_profile: qwen3_8\n", "thinking_profile has moved into llm.models"},
		{"llm.max_context_tokens", minimalYAML + "  max_context_tokens: 4096\n", "context_size on each entry"},
		{"llm.vision", minimalYAML + "  vision: true\n", "vision: true on each entry"},
		{"llm.thinking_selector_enable", minimalYAML + "  thinking_selector_enable: true\n", "thinking_selector_enable is retired"},
		{"string models", minimalBase + "  models: [\"old-way\"]\n", "entries are maps"},
	}
	for _, tc := range cases {
		_, err := Load(writeCfg(t, tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to say %q", tc.name, err, tc.want)
		}
	}
}

func TestLoadBlockExtensionsHasNoDefaults(t *testing.T) {
	// Absent key ⇒ block nothing; the configured list is the whole rule
	// (normalized, deduped, order kept). Safety lives in the download
	// disposition, not in a built-in upload blocklist.
	cfg, err := Load(writeCfg(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Scratchpad.BlockExtensions) != 0 {
		t.Errorf("block_extensions default = %v, want empty", cfg.Scratchpad.BlockExtensions)
	}

	cfg, err = Load(writeCfg(t, strings.Replace(minimalYAML,
		"scratchpad:\n", "scratchpad:\n  block_extensions: [exe, .DLL, exe, HTML]\n", 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{".exe", ".dll", ".html"}
	if len(cfg.Scratchpad.BlockExtensions) != len(want) {
		t.Fatalf("block_extensions = %v, want %v", cfg.Scratchpad.BlockExtensions, want)
	}
	for i := range want {
		if cfg.Scratchpad.BlockExtensions[i] != want[i] {
			t.Fatalf("block_extensions = %v, want %v", cfg.Scratchpad.BlockExtensions, want)
		}
	}
}

// --- thinking control fields (thinking-control-specification.md §2) ---------

// The legacy llm block — none of the four new fields — loads unchanged
// and leaves the runtime off: no selector, existing transport.
func TestLoadThinkingOmittedIsLegacy(t *testing.T) {
	cfg, err := Load(writeCfg(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Thinking != nil {
		t.Errorf("Thinking = %+v, want nil for a config without the new fields", cfg.Thinking)
	}
}

// The extended block from §2: every existing value keeps its exact
// meaning, the four new fields are carried, catalogue paths resolve
// against the config file's directory (not the working directory), and
// the selected combination resolves to the documented choice list.
func TestLoadThinkingExtended(t *testing.T) {
	dir := t.TempDir()
	// The files ARE the catalogues now: they define the selected gufo
	// and qwen3_8 entries, alongside an extra ID each that must not
	// disturb the selection.
	if err := os.WriteFile(filepath.Join(dir, "endpoints.yaml"), []byte(`
schema_version: 2
endpoints:
  gufo:
    protocol: openai_chat
    path: /chat/completions
    requests:
      'off':
        chat_template_kwargs: {enable_thinking: false}
      effort:
        chat_template_kwargs: {enable_thinking: true}
        reasoning_effort: $effort
  extra:
    protocol: openai_chat
    path: /c
    requests: {'off': {x: true}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "thinking-profiles.yaml"), []byte(`
schema_version: 2
profiles:
  qwen3_8:
    options:
    - {id: 'off', label: 'Off', mode: 'off'}
    - {id: low, label: Low, mode: effort, effort: low}
    - {id: medium, label: Medium, mode: effort, effort: medium}
    - {id: high, label: High, mode: effort, effort: xhigh}
  operator_p:
    options: []
`), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `
server:
  listen: "127.0.0.1:8090"
database:
  dir: "./data/db"
scratchpad:
  root: "./data/scratchpad"
app_limits:
  vision_max_image_bytes: 1MiB
llm:
  endpoint: "http://192.168.0.2:8080/v1"
  api_key: ""
  default_temperature: 0.7
  max_tokens_response: 16384
  show_reasoning: false
  endpoints_file: ./endpoints.yaml
  profiles_file: ./thinking-profiles.yaml
  endpoint_type: gufo
  models:
    - id: "Qwen3.8 Flash Next"
      default: true
      thinking_profile: qwen3_8
      context_size: 32768
      vision: true
    - id: "quiet-model"
      thinking_profile: operator_p
`
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.DefaultModelID() != "Qwen3.8 Flash Next" || cfg.LLM.ContextSizeFor("Qwen3.8 Flash Next") != 32768 ||
		cfg.LLM.ContextSizeFor("quiet-model") != 0 ||
		cfg.LLM.DefaultTemperature != 0.7 || cfg.LLM.MaxTokensResponse != 16384 ||
		!cfg.LLM.VisionFor("Qwen3.8 Flash Next") || cfg.LLM.VisionFor("quiet-model") {
		t.Errorf("existing llm values changed meaning: %+v", cfg.LLM)
	}
	if cfg.LLM.EndpointType != "gufo" ||
		cfg.LLM.ThinkingProfileFor("Qwen3.8 Flash Next") != "qwen3_8" ||
		cfg.LLM.ThinkingProfileFor("quiet-model") != "operator_p" {
		t.Errorf("new fields = %q / %v", cfg.LLM.EndpointType, cfg.LLM.ModelProfiles())
	}
	if cfg.AppLimits.VisionMaxImageBytes != 1<<20 {
		t.Errorf("vision image byte cap = %d, want 1 MiB", cfg.AppLimits.VisionMaxImageBytes)
	}
	if want := filepath.Join(dir, "endpoints.yaml"); cfg.LLM.EndpointsFile != want {
		t.Errorf("endpoints_file = %q, want resolved %q", cfg.LLM.EndpointsFile, want)
	}
	if cfg.Thinking == nil || !cfg.Thinking.SelectorVisible() {
		t.Fatal("runtime not resolved")
	}
	var ids []string
	for _, c := range cfg.Thinking.Choices("Qwen3.8 Flash Next") {
		ids = append(ids, c.ID)
	}
	want := []string{"default", "off", "low", "medium", "high"}
	if len(ids) != len(want) {
		t.Fatalf("choices = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("choices = %v, want %v", ids, want)
		}
	}
	// The second entry's empty profile offers only the built-in Default:
	// the dropdown follows the model, per entry.
	if q := cfg.Thinking.Choices("quiet-model"); len(q) != 1 || q[0].ID != "default" {
		t.Errorf("quiet-model choices = %v, want just default", q)
	}
}

// An explicitly selected but unknown ID, or a broken catalogue file, is
// a startup error — config.Load fails, the server never boots half-on.
func TestLoadThinkingFailures(t *testing.T) {
	cases := []struct{ name, epType, profile string }{
		{"unknown endpoint_type", "nope", "qwen3_8"},
		{"unknown thinking_profile", "gufo", "nope"},
		{"profile without type", "", "qwen3_8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra := ""
			if tc.epType != "" {
				extra = "  endpoint_type: " + tc.epType + "\n"
			}
			body := minimalBase + catalogueTail(t) + extra +
				modelsYAML("    - id: \"test-model\"\n      thinking_profile: "+tc.profile+"\n")
			if _, err := Load(writeCfg(t, body)); err == nil {
				t.Errorf("config accepted: %s / %s", tc.epType, tc.profile)
			}
		})
	}
	t.Run("unreadable file", func(t *testing.T) {
		if _, err := Load(writeCfg(t, minimalYAML+"  endpoints_file: ./gone.yaml\n  endpoint_type: gufo\n")); err == nil {
			t.Error("missing endpoints_file accepted")
		}
	})
	// Nothing is embedded: a selection without the file its ID lives in
	// is a startup error, not a silent fallback.
	t.Run("selection without its catalogue file", func(t *testing.T) {
		if _, err := Load(writeCfg(t, minimalYAML+"  endpoint_type: gufo\n")); err == nil {
			t.Error("endpoint_type without endpoints_file accepted")
		}
		// Endpoint catalogue present, profile named without its file.
		if _, err := Load(writeCfg(t, minimalBase+repoCatalogueLine(t, "endpoints.yaml")+
			"  endpoint_type: gufo\n"+
			modelsYAML("    - id: \"test-model\"\n      thinking_profile: qwen3_8\n"))); err == nil {
			t.Error("thinking_profile without profiles_file accepted")
		}
		// "none" is built in, so the profile file stays optional then.
		if _, err := Load(writeCfg(t, minimalYAML+repoCatalogueLine(t, "endpoints.yaml")+
			"  endpoint_type: gufo\n")); err != nil {
			t.Errorf("endpoint type without a profile must load (built-in none): %v", err)
		}
	})
}

// No switch: the dropdown derives from the entries. A profile spelled on
// an entry shows it; an endpoint type alone still builds the runtime
// (its transport) with nothing to choose; a profile without an endpoint
// type is the error it always was.
func TestLoadThinkingSelectorDerived(t *testing.T) {
	t.Run("a profiled entry shows the selector", func(t *testing.T) {
		cfg, err := Load(writeCfg(t, minimalBase+catalogueTail(t)+
			"  endpoint_type: gufo\n"+
			modelsYAML("    - id: \"test-model\"\n      thinking_profile: qwen3_8\n")))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Thinking == nil || !cfg.Thinking.SelectorVisible() {
			t.Error("selector must appear")
		}
	})
	t.Run("endpoint type alone loads, nothing to choose", func(t *testing.T) {
		cfg, err := Load(writeCfg(t, minimalBase+catalogueTail(t)+
			"  endpoint_type: gufo\n"+
			modelsYAML("    - id: \"test-model\"\n")))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Thinking != nil && cfg.Thinking.SelectorVisible() {
			t.Error("no profile anywhere: the selector must stay hidden")
		}
	})
	t.Run("a profile without an endpoint type fails", func(t *testing.T) {
		if _, err := Load(writeCfg(t, minimalBase+repoCatalogueLine(t, "endpoints.yaml")+
			modelsYAML("    - id: \"test-model\"\n      thinking_profile: qwen3_8\n"))); err == nil {
			t.Error("thinking_profile without endpoint_type accepted")
		}
	})
}

func TestPrepareUsesSameDefaultsAndRejectsRuntimeFailure(t *testing.T) {
	loaded, err := Load(writeCfg(t, minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	direct := Defaults()
	direct.Server.Listen = loaded.Server.Listen
	direct.Database.Dir = t.TempDir()
	direct.Scratchpad.Root = t.TempDir()
	direct.LLM.Endpoint = loaded.LLM.Endpoint
	direct.LLM.Models = loaded.LLM.Models
	if err := direct.Prepare(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(direct.Tools.CodeRun, loaded.Tools.CodeRun) {
		t.Fatalf("constructor defaults differ: %+v / %+v", direct.Tools.CodeRun, loaded.Tools.CodeRun)
	}
	if direct.Tools.CodeRun.MaxOutput != 256<<10 {
		t.Fatalf("output default=%d", direct.Tools.CodeRun.MaxOutput)
	}
	if direct.Tools.CodeRun.MaxMemory != 1<<30 {
		t.Fatalf("memory default=%d", direct.Tools.CodeRun.MaxMemory)
	}
	runtime := direct.Thinking
	if err := direct.Prepare(); err != nil || direct.Thinking != runtime {
		t.Fatalf("preparation rebuilt the runtime: %v", err)
	}
	bad := *direct
	bad.prepared = false
	bad.Thinking = nil
	bad.LLM.ProfilesFile = filepath.Join(t.TempDir(), "missing-profiles.yaml")
	if err := bad.Prepare(); err == nil {
		t.Fatal("runtime loading failure was silently accepted")
	}
	if bad.prepared {
		t.Fatal("failed configuration marked prepared")
	}
}

func TestServerAndApplicationLimitSections(t *testing.T) {
	body := strings.Replace(minimalYAML, "server:\n", `server:
  max_connections: 17
  concurrent_requests: 9
  max_body_bytes: 2MiB
`, 1) + `app_limits:
  max_message_chars: 1234
  concurrent_streams_total: 3
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.MaxConnections != 17 || cfg.Server.ConcurrentRequests != 9 || cfg.Server.MaxBodyBytes != 2<<20 || cfg.AppLimits.MaxMessageChars != 1234 || cfg.AppLimits.ConcurrentStreamsTotal != 3 {
		t.Fatalf("settings did not reach their owning sections: server=%+v app_limits=%+v", cfg.Server, cfg.AppLimits)
	}
	for _, tc := range []struct{ name, section, key string }{
		{"unknown section", "limits", "max_connections"},
		{"transport under app", "app_limits", "max_connections"},
		{"body cap under app", "app_limits", "max_body_bytes"},
		{"app under server", "server", "max_message_chars"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snippet := fmt.Sprintf("%s:\n  %s: 10\n", tc.section, tc.key)
			body := minimalYAML + snippet
			if tc.section == "server" {
				body = strings.Replace(minimalYAML, "server:\n", snippet, 1)
			}
			if _, err := Load(writeCfg(t, body)); err == nil {
				t.Fatal("accepted a setting in the wrong section")
			}
		})
	}
}

func TestScratchpadEntryLimitConfiguration(t *testing.T) {
	cfg, err := Load(writeCfg(t, minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scratchpad.MaxEntries != 1000 {
		t.Fatalf("default entries=%d", cfg.Scratchpad.MaxEntries)
	}
	for _, value := range []int{3, 10, 1000, -1, 1, 2} {
		body := strings.Replace(minimalYAML, "scratchpad:\n", fmt.Sprintf("scratchpad:\n  max_entries: %d\n", value), 1)
		cfg, err := Load(writeCfg(t, body))
		if value < 3 {
			if err == nil || !strings.Contains(err.Error(), "scratchpad.max_entries") {
				t.Fatalf("invalid %d accepted: %v", value, err)
			}
		} else if err != nil || cfg.Scratchpad.MaxEntries != value {
			t.Fatalf("entries %d: cfg=%v err=%v", value, cfg, err)
		}
	}
}

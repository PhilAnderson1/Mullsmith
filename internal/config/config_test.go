package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalYAML = `
server:
  listen: "127.0.0.1:8090"
database:
  dir: "./data/db"
scratchpad:
  root: "./data/scratchpad"
llm:
  endpoint: "http://127.0.0.1:8081/v1"
  model: "test-model"
`

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMinimalAndDefaults(t *testing.T) {
	cfg, err := Load(writeCfg(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.Registration != "invite-only" {
		t.Errorf("registration default = %q, want invite-only", cfg.Auth.Registration)
	}
	if cfg.Limits.MaxBodyBytes != 1<<20 {
		t.Errorf("max_body_bytes default = %d", cfg.Limits.MaxBodyBytes)
	}
	if cfg.Auth.Argon2.Memory != 65536 {
		t.Errorf("argon2 memory default = %d", cfg.Auth.Argon2.Memory)
	}
	if cfg.Tools.Agent.Approval["code_run"] != "confirm" {
		t.Errorf("code_run approval default = %q, want confirm", cfg.Tools.Agent.Approval["code_run"])
	}
	if cfg.Server.WriteTimeout.D() != 0 {
		t.Errorf("write_timeout default = %v, want 0 (unlimited, per spec example)", cfg.Server.WriteTimeout.D())
	}
	if cfg.Server.PublicURL != "http://127.0.0.1:8090" {
		t.Errorf("public_url derived = %q", cfg.Server.PublicURL)
	}
	if len(cfg.LLM.Models) != 1 || cfg.LLM.Models[0] != "test-model" {
		t.Errorf("models default = %v", cfg.LLM.Models)
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
	t.Setenv("GABKEEP_TEST_KEY", "sekret")

	load := func(body string) (*Config, error) { return Load(writeCfg(t, body)) }

	cfg, err := load(minimalYAML + `  # llm section is last; extra indented key extends it
`)
	if err == nil && cfg.LLM.APIKey != "" {
		t.Fatal("api_key unexpectedly set")
	}
	// (the above only checks comments don't break parsing; real cases below)

	// value substituted
	if _, err := load(replaceLastLLM(`  api_key: "${GABKEEP_TEST_KEY}"`)); err != nil {
		t.Fatalf("substitution: %v", err)
	}
	cfg, err = load(replaceLastLLM(`  api_key: "${GABKEEP_TEST_KEY}"`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.APIKey != "sekret" {
		t.Errorf("api_key = %q, want sekret", cfg.LLM.APIKey)
	}

	// :- default allowed when unset
	cfg, err = load(replaceLastLLM(`  api_key: "${GABKEEP_UNSET_VAR:-}"`))
	if err != nil {
		t.Fatalf("default form: %v", err)
	}
	if cfg.LLM.APIKey != "" {
		t.Errorf("api_key = %q, want empty via default", cfg.LLM.APIKey)
	}

	// missing without default → error naming the variable
	_, err = load(replaceLastLLM(`  api_key: "${GABKEEP_UNSET_VAR}"`))
	if err == nil || !strings.Contains(err.Error(), "GABKEEP_UNSET_VAR") {
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
  model: m
  typo_field: true
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatal("expected nested unknown-key error")
	}
}

// TestOptionalLimitKnobs pins the three-state semantics of the caps:
// unset → hardcoded default, explicit 0 → disabled, negative → rejected.
func TestOptionalLimitKnobs(t *testing.T) {
	load := func(t *testing.T, extra string) *Config {
		t.Helper()
		cfg, err := Load(writeCfg(t, minimalYAML+"\n"+extra))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return cfg
	}

	t.Run("unset takes defaults", func(t *testing.T) {
		cfg := load(t, "")
		if got := Deref(cfg.Limits.RequestsPerMinute); got != DefaultRequestsPerMinute {
			t.Errorf("requests_per_minute = %d, want %d", got, DefaultRequestsPerMinute)
		}
		if got := Deref(cfg.Limits.ChatMessagesPerHour); got != DefaultChatMessagesPerHour {
			t.Errorf("chat_messages_per_hour = %d, want %d", got, DefaultChatMessagesPerHour)
		}
		if got := Deref(cfg.Limits.ConcurrentStreamsPerUser); got != DefaultConcurrentStreamsUser {
			t.Errorf("concurrent_streams_per_user = %d, want %d", got, DefaultConcurrentStreamsUser)
		}
		if got := Deref(cfg.Limits.ConcurrentStreamsTotal); got != DefaultConcurrentStreamsTotal {
			t.Errorf("concurrent_streams_total = %d, want %d", got, DefaultConcurrentStreamsTotal)
		}
		if got := Deref(cfg.Limits.HashConcurrency); got != DefaultHashConcurrency {
			t.Errorf("hash_concurrency = %d, want %d", got, DefaultHashConcurrency)
		}
	})

	t.Run("explicit zero disables", func(t *testing.T) {
		cfg := load(t, `
limits:
  concurrent_streams_per_user: 0
  concurrent_streams_total: 0
  requests_per_minute: 0
  chat_messages_per_hour: 0
  hash_concurrency: 0
`)
		for name, p := range map[string]*int{
			"concurrent_streams_per_user": cfg.Limits.ConcurrentStreamsPerUser,
			"concurrent_streams_total":    cfg.Limits.ConcurrentStreamsTotal,
			"requests_per_minute":         cfg.Limits.RequestsPerMinute,
			"chat_messages_per_hour":      cfg.Limits.ChatMessagesPerHour,
			"hash_concurrency":            cfg.Limits.HashConcurrency,
		} {
			if p == nil || *p != 0 {
				t.Errorf("%s = %v, want explicit 0 (disabled)", name, p)
			}
		}
	})

	negatives := []struct{ key, wantErr string }{
		{"limits:\n  concurrent_streams_per_user: -1\n", "limits.concurrent_streams_per_user"},
		{"limits:\n  concurrent_streams_total: -3\n", "limits.concurrent_streams_total"},
		{"limits:\n  requests_per_minute: -1\n", "limits.requests_per_minute"},
		{"limits:\n  chat_messages_per_hour: -1\n", "limits.chat_messages_per_hour"},
		{"limits:\n  hash_concurrency: -2\n", "limits.hash_concurrency"},
	}
	for _, tc := range negatives {
		t.Run("negative rejected: "+tc.key, func(t *testing.T) {
			_, err := Load(writeCfg(t, minimalYAML+"\n"+tc.key))
			if err == nil {
				t.Fatal("negative limit accepted, want error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) ||
				!strings.Contains(err.Error(), "0 disables") {
				t.Errorf("error = %v, want mention of %q and \"0 disables\"", err, tc.wantErr)
			}
		})
	}

	// The retired rate_limit: block must fail with a message naming the
	// move, never silently win over or override limits:.
	t.Run("retired rate_limit names the move", func(t *testing.T) {
		_, err := Load(writeCfg(t, minimalYAML+`
rate_limit:
  requests_per_minute: 300
`))
		if err == nil {
			t.Fatal("retired rate_limit accepted, want error")
		}
		if !strings.Contains(err.Error(), "rate_limit") ||
			!strings.Contains(err.Error(), "limits:") {
			t.Errorf("error = %v, want it to point at the limits: block", err)
		}
	})
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
llm: {endpoint: "http://x/v1", model: m}
`,
			"server.listen",
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
llm: {endpoint: "http://x/v1", model: m, max_context_tokens: 4096, max_tokens_response: 8000}
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
  code_execution:
    enabled: true
    run_as_user: nobody
    allowed_runtimes:
      - { id: python3, binary: python3 }
llm: {endpoint: "http://x/v1", model: m}
`,
			"absolute path",
		},
		{
			"retired mode key rejected",
			`
database: {dir: ./db}
scratchpad:
  root: ./sp
  code_execution:
    enabled: true
    mode: "isolated"
    allowed_runtimes:
      - { id: python3, binary: /usr/bin/python3 }
llm: {endpoint: "http://x/v1", model: m}
`,
			"removed",
		},
		{
			"retired run_as_user key rejected",
			`
database: {dir: ./db}
scratchpad:
  root: ./sp
  code_execution:
    enabled: true
    run_as_user: nobody
    allowed_runtimes:
      - { id: python3, binary: /usr/bin/python3 }
llm: {endpoint: "http://x/v1", model: m}
`,
			"run_as_user",
		},
		{
			"retired container_image key rejected",
			`
database: {dir: ./db}
scratchpad:
  root: ./sp
  code_execution:
    enabled: true
    container_image: python:3
    allowed_runtimes:
      - { id: python3, binary: /usr/bin/python3 }
llm: {endpoint: "http://x/v1", model: m}
`,
			"container_image",
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
llm: {endpoint: "http://x/v1", model: m}
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
llm: {endpoint: "http://x/v1", model: m}
auth:
  cookie_secure: true
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	joined := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(joined, "without TLS") {
		t.Errorf("expected no-TLS warning, got: %s", joined)
	}
	if !strings.Contains(joined, "Secure cookies") {
		t.Errorf("expected cookie_secure warning, got: %s", joined)
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

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
	// The global open-mode sign-up budget (audit1 M-2): absent means 5.
	if got := cfg.Auth.SignupsPerHour(); got != 5 {
		t.Errorf("max_signups_per_hour default = %d, want 5", got)
	}
	if cfg.Tools.Agent.Approval["code_run"] != "confirm" {
		t.Errorf("code_run approval default = %q, want confirm", cfg.Tools.Agent.Approval["code_run"])
	}
	if cfg.Server.WriteTimeout.D() != 0 {
		t.Errorf("write_timeout default = %v, want 0 (unlimited, per spec example)", cfg.Server.WriteTimeout.D())
	}
	if len(cfg.LLM.Models) != 1 || cfg.LLM.Models[0] != "test-model" {
		t.Errorf("models default = %v", cfg.LLM.Models)
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
		{"limits.concurrent_streams_per_user", "limits:\n  concurrent_streams_per_user: %d\n",
			func(c *Config) int { return c.Limits.ConcurrentStreamsPerUser }, DefaultConcurrentStreamsUser},
		{"limits.concurrent_streams_total", "limits:\n  concurrent_streams_total: %d\n",
			func(c *Config) int { return c.Limits.ConcurrentStreamsTotal }, DefaultConcurrentStreamsTotal},
		{"limits.requests_per_minute", "limits:\n  requests_per_minute: %d\n",
			func(c *Config) int { return c.Limits.RequestsPerMinute }, DefaultRequestsPerMinute},
		{"limits.chat_messages_per_hour", "limits:\n  chat_messages_per_hour: %d\n",
			func(c *Config) int { return c.Limits.ChatMessagesPerHour }, DefaultChatMessagesPerHour},
		{"limits.hash_concurrency", "limits:\n  hash_concurrency: %d\n",
			func(c *Config) int { return c.Limits.HashConcurrency }, DefaultHashConcurrency},
		{"limits.concurrent_uploads", "limits:\n  concurrent_uploads: %d\n",
			func(c *Config) int { return c.Limits.ConcurrentUploads }, DefaultConcurrentUploads},
		{"auth.max_signups_per_hour", "auth:\n  max_signups_per_hour: %d\n",
			func(c *Config) int { return c.Auth.MaxSignupsPerHour }, defaultMaxSignupsPerHour},
	}
	for _, k := range knobs {
		t.Run(k.name, func(t *testing.T) {
			load := func(v int) (*Config, error) {
				return Load(writeCfg(t, minimalYAML+fmt.Sprintf(k.yaml, v)))
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

// The retired rate_limit: block must fail with a message naming the
// move, never silently win over or override limits:. (The caps' own
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
		!strings.Contains(err.Error(), "limits:") {
		t.Errorf("error = %v, want it to point at the limits: block", err)
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
		"server.listen":                              "deployment choice — template ships the production port",
		"server.tls.cert_file":                       "deployment paths — template stages certbot's cert",
		"server.tls.key_file":                        "deployment paths — template stages certbot's cert",
		"server.hsts_max_age":                        "0 (code default) means no HSTS header; the template ships one year as the TLS-era default",
		"database.dir":                               "required; per-machine path",
		"scratchpad.root":                            "required; per-machine path",
		"scratchpad.code_execution.allowed_runtimes": "host-specific interpreter paths — code default is the empty list",
		"llm.model":                                  "required; the template holds a placeholder",
		"llm.models":                                 "derived from llm.model (the default model is always pickable)",
		"llm.api_key":                                "secret via ${ENV} expansion — environment-dependent at test time",
		"llm.default_temperature":                    "zero is a meaningful LLM value, so zero-collapse cannot carry 0.7; code default is 0",
		"auth.remember_login":                        "bool zero-collapse — template opts in; code default keeps the checkbox off",
		"tools.web_search.api_key":                   "secret via ${ENV} expansion — environment-dependent at test time",
		"tools.web_fetch.enabled":                    "bool zero-collapse — template opts into guarded egress; code default is off",
		"email.smtp.password":                        "secret via ${ENV} expansion — environment-dependent at test time",
		"email.smtp.from":                            "sender identity is operator- and domain-specific; the template shows the shape",
	}

	tmplLeaf, miniLeaf := map[string]any{}, map[string]any{}
	flattenLeaves("", reflect.ValueOf(*tmpl), tmplLeaf)
	flattenLeaves("", reflect.ValueOf(*mini), miniLeaf)
	if len(tmplLeaf) != len(miniLeaf) {
		t.Fatalf("leaf count mismatch %d vs %d — flattenLeaves out of date?", len(tmplLeaf), len(miniLeaf))
	}
	for path, codeDefault := range miniLeaf {
		if _, ok := exceptions[path]; ok {
			continue
		}
		if !leafEqual(codeDefault, tmplLeaf[path]) {
			t.Errorf("%s: template = %v, code default = %v — match them or list the path as a reasoned exception",
				path, tmplLeaf[path], codeDefault)
		}
	}
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

func TestEmailValidation(t *testing.T) {
	// A disabled (or absent) email section needs nothing else — the
	// minimalYAML in this file has no email: block and must keep loading.
	const base = minimalYAML + `email:
  enabled: true
  base_url: "https://gabkeep.example:3210"
  smtp:
    host: "mail.example"
    authenticated_smtp: true
    username: "gabkeep"
    from: "gabkeep@example"
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
		{"needs from", `from: "gabkeep@example"`, `from: "gabkeep"`, "email.smtp.from"},
		{"needs base_url", `  base_url: "https://gabkeep.example:3210"`, "", "email.base_url"},
		{"base_url path", `https://gabkeep.example:3210"`, `https://x.example/app"`, "email.base_url"},
		{"base_url scheme", `https://gabkeep.example:3210"`, `ftp://x.example"`, "email.base_url"},
		{"auth needs username", `username: "gabkeep"`, `username: ""`, "email.smtp.username"},
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
		"base_url: \"https://gabkeep.example:3210\"",
		"base_url: \"https://gabkeep.example:3210/\"", 1)))
	if err != nil {
		t.Fatalf("trailing-slash base_url: %v", err)
	}
	if cfg.Email.BaseURL != "https://gabkeep.example:3210" {
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
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	joined := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(joined, "without TLS") {
		t.Errorf("expected no-TLS warning, got: %s", joined)
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

func TestLoadModelsAlwaysContainsDefault(t *testing.T) {
	// The picker shows the model a conversation actually runs on, and the
	// same list validates per-conversation choices — so the default model
	// must be in it even when the admin curated an explicit list.
	cfg, err := Load(writeCfg(t, minimalYAML+"  models: [\"alpha\", \"beta\"]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"alpha", "beta", "test-model"}
	if len(cfg.LLM.Models) != len(want) {
		t.Fatalf("models = %v, want %v", cfg.LLM.Models, want)
	}
	for i := range want {
		if cfg.LLM.Models[i] != want[i] {
			t.Fatalf("models = %v, want %v", cfg.LLM.Models, want)
		}
	}

	// Already present ⇒ not duplicated.
	cfg, err = Load(writeCfg(t, minimalYAML+"  models: [\"test-model\", \"alpha\"]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.LLM.Models) != 2 || cfg.LLM.Models[1] != "alpha" {
		t.Errorf("models = %v, want no duplicate of test-model", cfg.LLM.Models)
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

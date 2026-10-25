// Package config loads, environment-expands, and validates Gabkeep's YAML
// configuration file (spec §2.2). Unknown keys are a hard error so typos are
// caught at startup rather than silently ignored.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a YAML string parsed by time.ParseDuration, e.g. "720h", "30s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("must be a duration string like \"30s\" or \"720h\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// D returns the wrapped time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Size is a YAML string such as "25MiB", "512KiB" or a bare byte count.
// Only 1024-based units are accepted (KiB, MiB, GiB, TiB).
type Size int64

var sizeRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*(B|[KMGT]iB)?$`)

func (s *Size) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("must be a size like \"25MiB\" or a byte count")
	}
	v, err := ParseSize(raw)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// ParseSize parses "25MiB"-style strings; a bare integer means bytes.
func ParseSize(raw string) (Size, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, fmt.Errorf("invalid size %q (want e.g. 25MiB, 512KiB, or a byte count)", raw)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", raw)
	}
	var mult float64 = 1
	switch m[2] {
	case "", "B":
		mult = 1
	case "KiB":
		mult = 1 << 10
	case "MiB":
		mult = 1 << 20
	case "GiB":
		mult = 1 << 30
	case "TiB":
		mult = 1 << 40
	}
	return Size(n * mult), nil
}

// --- schema ---------------------------------------------------------------

type Config struct {
	Server     Server     `yaml:"server"`
	Limits     Limits     `yaml:"limits"`
	Database   Database   `yaml:"database"`
	Scratchpad Scratchpad `yaml:"scratchpad"`
	LLM        LLM        `yaml:"llm"`
	Tools      Tools      `yaml:"tools"`
	Auth       Auth       `yaml:"auth"`
	RateLimit  RateLimit  `yaml:"rate_limit"`
	Logging    Logging    `yaml:"logging"`

	// Path is the config file path; relative dirs resolve against its folder.
	Path string `yaml:"-"`
	// TrustedNets is the parsed form of Server.TrustedProxies (spec §9.10).
	TrustedNets []netip.Prefix `yaml:"-"`
}

type Server struct {
	Listen         string   `yaml:"listen"`
	PublicURL      string   `yaml:"public_url"`
	TLS            TLS      `yaml:"tls"`
	ReadTimeout    Duration `yaml:"read_timeout"`
	WriteTimeout   Duration `yaml:"write_timeout"`
	ShutdownGrace  Duration `yaml:"shutdown_grace"`
	TrustedProxies []string `yaml:"trusted_proxies"`
	HSTSMaxAge     int64    `yaml:"hsts_max_age"`
}

type TLS struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type Limits struct {
	MaxBodyBytes             Size `yaml:"max_body_bytes"`
	MaxMessageChars          int  `yaml:"max_message_chars"`
	MaxAttachments           int  `yaml:"max_attachments"`
	ConcurrentStreamsPerUser int  `yaml:"concurrent_streams_per_user"`
}

type Database struct {
	Dir string `yaml:"dir"`
}

// defaultBlockExtensions: §9.4 active-content types; scratchpad.block_extensions
// in config appends to this list.
var defaultBlockExtensions = []string{
	".html", ".htm", ".svg", ".xhtml", ".xml", ".js", ".exe", ".sh", ".bat",
	".cmd", ".com", ".scr", ".jar", ".vbs", ".ps1", ".apk", ".dmg",
}

type Scratchpad struct {
	Root        string   `yaml:"root"`
	MaxFileSize Size     `yaml:"max_file_size"`
	UserQuota   Size     `yaml:"user_quota"`
	ArtifactTTL Duration `yaml:"artifact_ttl"`
	// BlockExtensions rejects uploads whose (final) extension matches —
	// active-content types that must never sit in a user's uploads dir
	// (§9.4). Config values extend the built-in default list.
	BlockExtensions []string      `yaml:"block_extensions"`
	CodeExec        CodeExecution `yaml:"code_execution"`
}

type CodeExecution struct {
	Enabled         bool      `yaml:"enabled"`
	Mode            string    `yaml:"mode"` // isolated | container | raw
	ContainerImage  string    `yaml:"container_image"`
	RunAsUser       string    `yaml:"run_as_user"`
	AllowedRuntimes []Runtime `yaml:"allowed_runtimes"`
	Timeout         Duration  `yaml:"timeout"`
	MaxOutput       Size      `yaml:"max_output"`
	MaxMemory       Size      `yaml:"max_memory"`
	AllowNetwork    bool      `yaml:"allow_network"`
}

type Runtime struct {
	ID     string `yaml:"id"`
	Binary string `yaml:"binary"`
}

type LLM struct {
	Endpoint           string   `yaml:"endpoint"`
	APIKey             string   `yaml:"api_key"`
	Model              string   `yaml:"model"`
	Models             []string `yaml:"models"`
	MaxContextTokens   int      `yaml:"max_context_tokens"`
	DefaultTemperature float64  `yaml:"default_temperature"`
	RequestTimeout     Duration `yaml:"request_timeout"`
	MaxTokensResponse  int      `yaml:"max_tokens_response"`
	ShowReasoning      bool     `yaml:"show_reasoning"`
}

type Tools struct {
	WebSearch WebSearch `yaml:"web_search"`
	WebFetch  WebFetch  `yaml:"web_fetch"`
	Agent     Agent     `yaml:"agent"`
}

type WebSearch struct {
	Enabled    bool   `yaml:"enabled"`
	Provider   string `yaml:"provider"` // brave | searxng | duckduckgo
	APIKey     string `yaml:"api_key"`
	Endpoint   string `yaml:"endpoint"` // for searxng instances
	MaxResults int    `yaml:"max_results"`
}

type WebFetch struct {
	Enabled  bool     `yaml:"enabled"`
	MaxBytes Size     `yaml:"max_bytes"`
	Timeout  Duration `yaml:"timeout"`
}

type Agent struct {
	MaxToolRounds int               `yaml:"max_tool_rounds"`
	Approval      map[string]string `yaml:"approval"` // tool → auto | confirm
}

type Auth struct {
	Registration        string   `yaml:"registration"` // open | invite-only | closed
	RememberLogin       bool     `yaml:"remember_login"`
	SessionTTL          Duration `yaml:"session_ttl"`
	RememberTTL         Duration `yaml:"remember_ttl"`
	RememberAbsoluteTTL Duration `yaml:"remember_absolute_ttl"`
	CookieSecure        bool     `yaml:"cookie_secure"`
	MaxFailedLogins     int      `yaml:"max_failed_logins"`
	Argon2              Argon2   `yaml:"argon2"`
}

type Argon2 struct {
	Time    int `yaml:"time"`
	Memory  int `yaml:"memory"` // KiB
	Threads int `yaml:"threads"`
}

type RateLimit struct {
	RequestsPerMinute   int `yaml:"requests_per_minute"`
	ChatMessagesPerHour int `yaml:"chat_messages_per_hour"`
}

type Logging struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json
	File   string `yaml:"file"`   // empty = stderr
}

// --- loading --------------------------------------------------------------

// envRe matches ${NAME} and ${NAME:-default}.
var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv substitutes ${VAR} (unset → error) and ${VAR:-fallback}.
func expandEnv(in string) (string, error) {
	var missing []string
	out := envRe.ReplaceAllStringFunc(in, func(m string) string {
		g := envRe.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(g[1]); ok {
			return v
		}
		if strings.Contains(m, ":-") {
			return g[2]
		}
		missing = append(missing, g[1])
		return m
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable(s) not set: %s (use ${VAR:-} to allow unset)", strings.Join(missing, ", "))
	}
	return out, nil
}

// Load reads, expands, decodes (strict), defaults and validates the config.
// Relative paths are resolved against the config file's directory.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded, err := expandEnv(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.Path = abs
	c.setDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c.resolvePaths()
	return &c, nil
}

func (c *Config) resolvePaths() {
	base := filepath.Dir(c.Path)
	resolve := func(p string) string {
		if p == "" {
			return p
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		return filepath.Clean(p)
	}
	c.Database.Dir = resolve(c.Database.Dir)
	c.Scratchpad.Root = resolve(c.Scratchpad.Root)
	if c.Logging.File != "" {
		c.Logging.File = resolve(c.Logging.File)
	}
}

// --- defaults -------------------------------------------------------------

func (c *Config) setDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:8080"
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = Duration(30 * time.Second)
	}
	if c.Server.ShutdownGrace == 0 {
		c.Server.ShutdownGrace = Duration(15 * time.Second)
	}
	if c.Limits.MaxBodyBytes == 0 {
		c.Limits.MaxBodyBytes = 1 << 20
	}
	if c.Limits.MaxMessageChars == 0 {
		c.Limits.MaxMessageChars = 32000
	}
	if c.Limits.MaxAttachments == 0 {
		c.Limits.MaxAttachments = 8
	}
	if c.Limits.ConcurrentStreamsPerUser == 0 {
		c.Limits.ConcurrentStreamsPerUser = 2
	}
	if c.Scratchpad.MaxFileSize == 0 {
		c.Scratchpad.MaxFileSize = 25 << 20
	}
	if c.Scratchpad.UserQuota == 0 {
		c.Scratchpad.UserQuota = 1 << 30
	}
	if c.Scratchpad.ArtifactTTL == 0 {
		c.Scratchpad.ArtifactTTL = Duration(168 * time.Hour)
	}
	// §9.4: config entries EXTEND the built-in active-content blocklist.
	{
		seen := map[string]bool{}
		merged := make([]string, 0, len(defaultBlockExtensions)+len(c.Scratchpad.BlockExtensions))
		for _, e := range append(append([]string{}, defaultBlockExtensions...),
			c.Scratchpad.BlockExtensions...) {
			e = strings.ToLower(strings.TrimSpace(e))
			if e == "" {
				continue
			}
			if !strings.HasPrefix(e, ".") {
				e = "." + e
			}
			if !seen[e] {
				seen[e] = true
				merged = append(merged, e)
			}
		}
		c.Scratchpad.BlockExtensions = merged
	}
	if c.Scratchpad.CodeExec.Mode == "" {
		c.Scratchpad.CodeExec.Mode = "isolated"
	}
	if c.Scratchpad.CodeExec.Timeout == 0 {
		c.Scratchpad.CodeExec.Timeout = Duration(30 * time.Second)
	}
	if c.Scratchpad.CodeExec.MaxOutput == 0 {
		c.Scratchpad.CodeExec.MaxOutput = 32 << 10
	}
	if c.Scratchpad.CodeExec.MaxMemory == 0 {
		c.Scratchpad.CodeExec.MaxMemory = 512 << 20
	}
	if c.LLM.MaxContextTokens == 0 {
		c.LLM.MaxContextTokens = 32768
	}
	if c.LLM.MaxTokensResponse == 0 {
		c.LLM.MaxTokensResponse = 4096
	}
	if len(c.LLM.Models) == 0 && c.LLM.Model != "" {
		c.LLM.Models = []string{c.LLM.Model}
	}
	if c.LLM.RequestTimeout == 0 {
		c.LLM.RequestTimeout = Duration(5 * time.Minute)
	}
	if c.Tools.WebSearch.Provider == "" {
		c.Tools.WebSearch.Provider = "brave"
	}
	if c.Tools.WebSearch.MaxResults == 0 {
		c.Tools.WebSearch.MaxResults = 8
	}
	if c.Tools.WebFetch.MaxBytes == 0 {
		c.Tools.WebFetch.MaxBytes = 512 << 10
	}
	if c.Tools.WebFetch.Timeout == 0 {
		c.Tools.WebFetch.Timeout = Duration(15 * time.Second)
	}
	if c.Tools.Agent.MaxToolRounds == 0 {
		c.Tools.Agent.MaxToolRounds = 8
	}
	if c.Tools.Agent.Approval == nil {
		c.Tools.Agent.Approval = map[string]string{}
	}
	for tool, def := range map[string]string{
		"web_search": "auto", "web_fetch": "auto", "fs_list": "auto", "fs_read": "auto",
		"fs_write": "auto", "files_share": "auto", "code_run": "confirm",
	} {
		if _, ok := c.Tools.Agent.Approval[tool]; !ok {
			c.Tools.Agent.Approval[tool] = def
		}
	}
	if c.Auth.Registration == "" {
		c.Auth.Registration = "invite-only"
	}
	if c.Auth.SessionTTL == 0 {
		c.Auth.SessionTTL = Duration(24 * time.Hour)
	}
	if c.Auth.RememberTTL == 0 {
		c.Auth.RememberTTL = Duration(720 * time.Hour)
	}
	if c.Auth.RememberAbsoluteTTL == 0 {
		c.Auth.RememberAbsoluteTTL = Duration(2160 * time.Hour)
	}
	if c.Auth.MaxFailedLogins == 0 {
		c.Auth.MaxFailedLogins = 10
	}
	if c.Auth.Argon2 == (Argon2{}) {
		c.Auth.Argon2 = Argon2{Time: 1, Memory: 65536, Threads: 4}
	}
	if c.RateLimit.RequestsPerMinute == 0 {
		c.RateLimit.RequestsPerMinute = 120
	}
	if c.RateLimit.ChatMessagesPerHour == 0 {
		c.RateLimit.ChatMessagesPerHour = 60
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
	}
}

// --- validation -----------------------------------------------------------

type failList struct{ errs []error }

func (f *failList) bad(format string, args ...any) {
	f.errs = append(f.errs, fmt.Errorf(format, args...))
}

// Validate checks every field for coherence; it is pure (no filesystem or
// network access — see CheckIO for that).
func (c *Config) Validate() error {
	var f failList

	host, port, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		f.bad("server.listen: %v", err)
	} else {
		_ = host
		if p, e := strconv.Atoi(port); e != nil || p < 0 || p > 65535 {
			f.bad("server.listen: invalid port %q", port)
		}
	}

	derivedPublic := c.Server.PublicURL == ""
	if c.Server.PublicURL != "" {
		u, err := url.Parse(c.Server.PublicURL)
		switch {
		case err != nil:
			f.bad("server.public_url: %v", err)
		case u.Scheme != "http" && u.Scheme != "https":
			f.bad("server.public_url: scheme must be http or https")
		case u.Host == "":
			f.bad("server.public_url: host required")
		case u.Path != "" && u.Path != "/":
			f.bad("server.public_url: base paths are not supported (scheme+host only)")
		default:
			c.Server.PublicURL = strings.TrimSuffix(u.String(), "/")
		}
	}
	if derivedPublic {
		scheme := "http"
		if c.Server.TLS.Enabled {
			scheme = "https"
		}
		c.Server.PublicURL = scheme + "://" + c.Server.Listen
	}

	if c.Server.TLS.Enabled {
		if c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "" {
			f.bad("server.tls: cert_file and key_file are required when enabled")
		}
	}
	if c.Server.HSTSMaxAge < 0 {
		f.bad("server.hsts_max_age: must be >= 0")
	}
	c.TrustedNets = nil
	for _, p := range c.Server.TrustedProxies {
		prefix, err := parseProxy(p)
		if err != nil {
			f.bad("server.trusted_proxies: %v", err)
			continue
		}
		c.TrustedNets = append(c.TrustedNets, prefix)
	}

	if c.Limits.MaxBodyBytes <= 0 || c.Limits.MaxMessageChars <= 0 ||
		c.Limits.MaxAttachments <= 0 || c.Limits.ConcurrentStreamsPerUser <= 0 {
		f.bad("limits: all values must be > 0")
	}

	if c.Database.Dir == "" {
		f.bad("database.dir: required")
	}
	if c.Scratchpad.Root == "" {
		f.bad("scratchpad.root: required")
	}
	if c.Scratchpad.MaxFileSize <= 0 || c.Scratchpad.UserQuota <= 0 {
		f.bad("scratchpad: max_file_size and user_quota must be > 0")
	}
	switch c.Scratchpad.CodeExec.Mode {
	case "isolated", "container", "raw":
	default:
		f.bad("scratchpad.code_execution.mode: must be isolated, container or raw")
	}
	if c.Scratchpad.CodeExec.Enabled {
		if len(c.Scratchpad.CodeExec.AllowedRuntimes) == 0 {
			f.bad("scratchpad.code_execution.allowed_runtimes: required when code execution is enabled")
		}
		seen := map[string]bool{}
		for _, r := range c.Scratchpad.CodeExec.AllowedRuntimes {
			switch {
			case r.ID == "":
				f.bad("scratchpad.code_execution.allowed_runtimes: id required")
			case seen[r.ID]:
				f.bad("scratchpad.code_execution.allowed_runtimes: duplicate id %q", r.ID)
			case r.Binary == "" || !filepath.IsAbs(r.Binary):
				f.bad("scratchpad.code_execution.allowed_runtimes[%s]: binary must be an absolute path", r.ID)
			}
			seen[r.ID] = true
		}
		if c.Scratchpad.CodeExec.Mode == "container" && c.Scratchpad.CodeExec.ContainerImage == "" {
			f.bad("scratchpad.code_execution.container_image: required in container mode")
		}
		if c.Scratchpad.CodeExec.Mode == "isolated" && c.Scratchpad.CodeExec.RunAsUser == "" {
			f.bad("scratchpad.code_execution.run_as_user: required in isolated mode")
		}
	}

	c.LLM.Endpoint = strings.TrimRight(c.LLM.Endpoint, "/")
	u, err := url.Parse(c.LLM.Endpoint)
	switch {
	case c.LLM.Endpoint == "":
		f.bad("llm.endpoint: required")
	case err != nil:
		f.bad("llm.endpoint: %v", err)
	case u.Scheme != "http" && u.Scheme != "https":
		f.bad("llm.endpoint: scheme must be http or https")
	case u.Host == "":
		f.bad("llm.endpoint: host required")
	}
	if c.LLM.Model == "" {
		f.bad("llm.model: required")
	}
	if c.LLM.MaxContextTokens < 4096 {
		f.bad("llm.max_context_tokens: must be >= 4096")
	}
	if c.LLM.MaxTokensResponse <= 0 || c.LLM.MaxTokensResponse >= c.LLM.MaxContextTokens {
		f.bad("llm.max_tokens_response: must be > 0 and < max_context_tokens")
	}
	if c.LLM.DefaultTemperature < 0 || c.LLM.DefaultTemperature > 2 {
		f.bad("llm.default_temperature: must be within 0..2")
	}

	switch c.Tools.WebSearch.Provider {
	case "brave", "searxng", "duckduckgo":
	default:
		f.bad("tools.web_search.provider: must be brave, searxng or duckduckgo")
	}
	if c.Tools.WebSearch.Enabled {
		switch c.Tools.WebSearch.Provider {
		case "brave":
			if c.Tools.WebSearch.APIKey == "" {
				f.bad("tools.web_search.api_key: required for the brave provider (try ${GABKEEP_SEARCH_API_KEY:-})")
			}
		case "searxng":
			if c.Tools.WebSearch.Endpoint == "" {
				f.bad("tools.web_search.endpoint: required for the searxng provider")
			}
		}
	}
	if c.Tools.WebFetch.MaxBytes <= 0 {
		f.bad("tools.web_fetch.max_bytes: must be > 0")
	}
	for tool, mode := range c.Tools.Agent.Approval {
		if mode != "auto" && mode != "confirm" {
			f.bad("tools.agent.approval.%s: must be \"auto\" or \"confirm\"", tool)
		}
	}

	switch c.Auth.Registration {
	case "open", "invite-only", "closed":
	default:
		f.bad("auth.registration: must be open, invite-only or closed")
	}
	if c.Auth.SessionTTL <= 0 || c.Auth.RememberTTL <= 0 || c.Auth.RememberAbsoluteTTL <= 0 {
		f.bad("auth: session TTLs must be > 0")
	}
	if c.Auth.RememberAbsoluteTTL < c.Auth.RememberTTL {
		f.bad("auth.remember_absolute_ttl: must be >= remember_ttl")
	}
	if c.Auth.Argon2.Time < 1 || c.Auth.Argon2.Memory < 1024 || c.Auth.Argon2.Threads < 1 {
		f.bad("auth.argon2: need time >= 1, memory >= 1024 KiB, threads >= 1")
	}

	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		f.bad("logging.level: must be debug, info, warn or error")
	}
	switch c.Logging.Format {
	case "text", "json":
	default:
		f.bad("logging.format: must be text or json")
	}

	return errors.Join(f.errs...)
}

// Warnings returns non-fatal advisories (spec §9.10 startup warnings).
func (c *Config) Warnings() []string {
	var w []string
	if !c.Server.TLS.Enabled && !isLoopbackHost(strings.Split(c.Server.Listen, ":")[0]) {
		w = append(w, "server is bound to a non-loopback address without TLS (spec §9.10) — accept this only on a trusted network")
	}
	if c.Server.TLS.Enabled && !strings.HasPrefix(c.Server.PublicURL, "https://") {
		w = append(w, "TLS is enabled but server.public_url is not https://")
	}
	if !strings.HasPrefix(c.Server.PublicURL, "https://") && c.Auth.CookieSecure {
		w = append(w, "auth.cookie_secure is true but public_url is plain http — Secure cookies cannot be delivered over http")
	}
	if c.Server.HSTSMaxAge > 0 && !c.Server.TLS.Enabled {
		w = append(w, "server.hsts_max_age > 0 but TLS is disabled — HSTS headers will never be sent")
	}
	if c.Scratchpad.CodeExec.Enabled {
		switch c.Scratchpad.CodeExec.Mode {
		case "raw":
			w = append(w, "code_execution.mode=raw: sandboxed scripts run as the gabkeep OS user — trusted single-user boxes only (spec §9.9)")
		case "isolated":
			w = append(w, "code_execution.mode=isolated shares the host network; consider container mode for internet-facing installs (spec §9.9)")
		}
		if c.Tools.Agent.Approval["code_run"] == "auto" && c.Auth.Registration == "open" {
			w = append(w, "code execution enabled with auto-approval AND open registration — prompt-injection risk (spec §9.13)")
		}
	}
	if c.Auth.Registration == "open" {
		w = append(w, "registration is open while email addresses are unverified in v1 — invite-only is the recommended public default (spec §9.12)")
	}
	return w
}

// --- runtime checks (used at startup and by -check-config) -----------------

// Check is one line of the -check-config report.
type Check struct {
	Name   string
	Detail string
	OK     bool
}

// EnsureDirs creates the database and scratchpad roots.
func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.Database.Dir, c.Scratchpad.Root} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

// CheckIO validates writable roots and TLS files; no network access.
func (c *Config) CheckIO() []Check {
	var out []Check
	for _, d := range []struct{ name, path string }{
		{"database.dir", c.Database.Dir},
		{"scratchpad.root", c.Scratchpad.Root},
	} {
		ch := Check{Name: d.name, Detail: d.path}
		if err := os.MkdirAll(d.path, 0o700); err != nil {
			ch.Detail = err.Error()
		} else if err := dirWritable(d.path); err != nil {
			ch.Detail = err.Error()
		} else {
			ch.OK = true
		}
		out = append(out, ch)
	}
	if c.Server.TLS.Enabled {
		for _, f := range []struct{ name, path string }{
			{"tls.cert_file", c.Server.TLS.CertFile},
			{"tls.key_file", c.Server.TLS.KeyFile},
		} {
			ch := Check{Name: f.name, Detail: f.path}
			if _, err := os.Stat(f.path); err != nil {
				ch.Detail = err.Error()
			} else {
				ch.OK = true
			}
			out = append(out, ch)
		}
	}
	return out
}

func dirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".gabkeep-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.WriteString("x"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// --- helpers ---------------------------------------------------------------

func parseProxy(p string) (netip.Prefix, error) {
	if pre, err := netip.ParsePrefix(p); err == nil {
		return pre.Masked(), nil
	}
	addr, err := netip.ParseAddr(p)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid IP or CIDR %q", p)
	}
	if addr.Is4() {
		return netip.PrefixFrom(addr, 32), nil
	}
	return netip.PrefixFrom(addr.Unmap(), 128), nil
}

func isLoopbackHost(h string) bool {
	if h == "" || h == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(h)
	return err == nil && addr.IsLoopback()
}

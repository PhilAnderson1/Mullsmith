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
	RateLimit  RateLimit  `yaml:"rate_limit"` // retired; see Validate
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
	MaxBodyBytes    Size `yaml:"max_body_bytes"`
	MaxMessageChars int  `yaml:"max_message_chars"`
	MaxAttachments  int  `yaml:"max_attachments"`
	// The four caps below are optional pointers so an explicit 0 (disable)
	// is distinguishable from "unset" (take the hardcoded default):
	// unset → default, 0 → unlimited/disabled, negative → config error
	// (§9.11). requests_per_minute and chat_messages_per_hour moved here
	// from the retired rate_limit block.
	ConcurrentStreamsPerUser *int `yaml:"concurrent_streams_per_user"`
	// ConcurrentStreamsTotal caps generations running at once across ALL
	// users; the global backstop behind the per-user cap (§9.11). One
	// shared LLM endpoint saturates long before a handful of users each
	// hit their personal cap.
	ConcurrentStreamsTotal *int `yaml:"concurrent_streams_total"`
	// Per-user API token bucket over non-chat endpoints (§9.11).
	RequestsPerMinute *int `yaml:"requests_per_minute"`
	// Per-user hourly chat-message bucket (§9.11).
	ChatMessagesPerHour *int `yaml:"chat_messages_per_hour"`
	// HashConcurrency bounds concurrent Argon2 computations across ALL
	// users — the global admission gate behind the per-IP login/register
	// buckets (audit finding 4). Default 1: the parameters are heavy
	// (64 MiB, 4 threads) and hashing is sub-second, so serialising costs
	// honest users nothing while a flood can never exceed one hash's worth
	// of memory and CPU. 0 disables the gate (queue stays bounded).
	HashConcurrency *int `yaml:"hash_concurrency"`
	// ConcurrentUploads bounds simultaneous in-flight uploads across ALL
	// users — temp files plus the atomic quota commit together (audit
	// finding 5). Default 8: a browser tops out around 6 parallel uploads
	// per host, so honest sessions never touch it, while the per-minute
	// request bucket alone would let a script wave unbounded
	// max_file_size temporaries through. 0 disables (unbounded).
	ConcurrentUploads *int `yaml:"concurrent_uploads"`
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
	// WorkTTL ages the model's scratch desk: files under work/ whose mtime
	// exceeds it are swept (§8). Default 24h. Like artifact_ttl it does NOT
	// accept 0 as "never" — a knob that disables expiry of generated bytes
	// would let the desk fill the disk silently.
	WorkTTL Duration `yaml:"work_ttl"`
	// BlockExtensions rejects uploads whose (final) extension matches —
	// active-content types that must never sit in a user's uploads dir
	// (§9.4). Config values extend the built-in default list.
	BlockExtensions []string      `yaml:"block_extensions"`
	CodeExec        CodeExecution `yaml:"code_execution"`
}

type CodeExecution struct {
	Enabled bool `yaml:"enabled"`
	// Code execution always runs inside a bubblewrap jail (§9.9): there is
	// exactly one mode and it is not configurable. The three fields below
	// are retired — they exist only so Validate can tell operators running
	// old configs what changed instead of failing on an unknown key.
	Mode            string    `yaml:"mode"`            // retired
	ContainerImage  string    `yaml:"container_image"` // retired
	RunAsUser       string    `yaml:"run_as_user"`     // retired
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
	// Vision sends image attachments to the endpoint inline (OpenAI
	// image_url content parts) instead of only naming them in the
	// manifest. OFF by default: a text-only endpoint must never receive a
	// content-part array it cannot parse.
	Vision              bool `yaml:"vision"`
	VisionMaxImageBytes Size `yaml:"vision_max_image_bytes"` // per-picture cap (default 4 MiB)
	VisionMaxImages     int  `yaml:"vision_max_images"`      // inline pictures per message (default 4)
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
	CookieSecure        *bool    `yaml:"cookie_secure"`
	MaxFailedLogins     int      `yaml:"max_failed_logins"`
	Argon2              Argon2   `yaml:"argon2"`
}

// SecureCookies reports whether cookies must carry the Secure flag. It
// defaults to true: sessions are Secure unless a deployment explicitly opts
// out (plain-http dev box), never because a field was forgotten (§9.1).
func (a *Auth) SecureCookies() bool {
	return a.CookieSecure == nil || *a.CookieSecure
}

type Argon2 struct {
	Time    int `yaml:"time"`
	Memory  int `yaml:"memory"` // KiB
	Threads int `yaml:"threads"`
}

// RateLimit is retired: its two knobs moved into limits (§9.11). The type
// exists only so Validate can name the move to operators of old configs
// instead of yaml's generic unknown-key error.
type RateLimit struct {
	RequestsPerMinute   *int `yaml:"requests_per_minute"` // retired
	ChatMessagesPerHour *int `yaml:"chat_messages_per_hour"`
}

// Shipped defaults for the optional caps under limits. Sample and
// production configs spell these same values out; 0 on any of them turns
// that single control off (operator's explicit choice, §9.11).
const (
	DefaultRequestsPerMinute      = 120
	DefaultChatMessagesPerHour    = 120
	DefaultConcurrentStreamsUser  = 2
	DefaultConcurrentStreamsTotal = 8
	DefaultHashConcurrency        = 1
	DefaultConcurrentUploads      = 8
)

// Ptr returns &v — for assigning the optional (pointer) knobs in configs
// built as struct literals, e.g. tests.
func Ptr(v int) *int { return &v }

// Deref reads an optional limit knob; nil behaves like 0. After config.Load
// every knob is non-nil; struct-literal configs (tests) may leave them nil,
// which then reads as "disabled" — matching the zero-value behaviour these
// tests were written against.
func Deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
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
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the operator-supplied -config argument
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
	if c.Limits.ConcurrentStreamsPerUser == nil {
		c.Limits.ConcurrentStreamsPerUser = Ptr(DefaultConcurrentStreamsUser)
	}
	if c.Limits.ConcurrentStreamsTotal == nil {
		c.Limits.ConcurrentStreamsTotal = Ptr(DefaultConcurrentStreamsTotal)
	}
	// Unset keys fall back to defaults; an explicit 0 stays 0 and disables
	// that control (validation rejects negatives). Shipped configs spell the
	// defaults out so the effective policy is visible at a glance.
	if c.Limits.RequestsPerMinute == nil {
		c.Limits.RequestsPerMinute = Ptr(DefaultRequestsPerMinute)
	}
	if c.Limits.ChatMessagesPerHour == nil {
		c.Limits.ChatMessagesPerHour = Ptr(DefaultChatMessagesPerHour)
	}
	if c.Limits.HashConcurrency == nil {
		c.Limits.HashConcurrency = Ptr(DefaultHashConcurrency)
	}
	if c.Limits.ConcurrentUploads == nil {
		c.Limits.ConcurrentUploads = Ptr(DefaultConcurrentUploads)
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
	if c.Scratchpad.WorkTTL == 0 {
		c.Scratchpad.WorkTTL = Duration(24 * time.Hour)
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
	if c.LLM.VisionMaxImageBytes == 0 {
		c.LLM.VisionMaxImageBytes = 4 << 20
	}
	if c.LLM.VisionMaxImages == 0 {
		c.LLM.VisionMaxImages = 4
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

	if c.Limits.MaxBodyBytes <= 0 || c.Limits.MaxMessageChars <= 0 || c.Limits.MaxAttachments <= 0 {
		f.bad("limits: max_body_bytes, max_message_chars and max_attachments must be > 0")
	}
	// Optional caps: 0 disables, anything negative is a typo worth failing on.
	for _, k := range []struct {
		name string
		v    *int
	}{
		{"limits.concurrent_streams_per_user", c.Limits.ConcurrentStreamsPerUser},
		{"limits.concurrent_streams_total", c.Limits.ConcurrentStreamsTotal},
		{"limits.requests_per_minute", c.Limits.RequestsPerMinute},
		{"limits.chat_messages_per_hour", c.Limits.ChatMessagesPerHour},
		{"limits.hash_concurrency", c.Limits.HashConcurrency},
		{"limits.concurrent_uploads", c.Limits.ConcurrentUploads},
	} {
		if k.v != nil && *k.v < 0 {
			f.bad("%s: must be >= 0 (0 disables the limit)", k.name)
		}
	}
	// rate_limit: retired — the two knobs moved into limits:. Name the
	// move instead of silently ignoring values an old config still carries.
	if c.RateLimit.RequestsPerMinute != nil || c.RateLimit.ChatMessagesPerHour != nil {
		f.bad("rate_limit: retired — move requests_per_minute and chat_messages_per_hour into the limits: block")
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
	// One sandbox, always: bubblewrap (§9.9). The old mode knobs are
	// retired; say so plainly rather than through an unknown-key error.
	ce := &c.Scratchpad.CodeExec
	if ce.Mode != "" {
		f.bad("scratchpad.code_execution.mode: removed — code execution now always runs in a bubblewrap jail; delete this key")
	}
	if ce.ContainerImage != "" {
		f.bad("scratchpad.code_execution.container_image: removed with container mode — delete this key")
	}
	if ce.RunAsUser != "" {
		f.bad("scratchpad.code_execution.run_as_user: removed — the jail runs in its own user namespace; delete this key")
	}
	if ce.Enabled {
		if len(ce.AllowedRuntimes) == 0 {
			f.bad("scratchpad.code_execution.allowed_runtimes: required when code execution is enabled")
		}
		seen := map[string]bool{}
		for _, r := range ce.AllowedRuntimes {
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
	if c.LLM.VisionMaxImageBytes < 0 {
		f.bad("llm.vision_max_image_bytes: must be >= 0")
	}
	if c.LLM.VisionMaxImages < 0 {
		f.bad("llm.vision_max_images: must be >= 0")
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
	// Upper bounds: typo-sized parameters are an OOM/hang waiting to happen,
	// and auth.HashPassword widens these ints to uint32/uint8.
	if c.Auth.Argon2.Time > 4096 || c.Auth.Argon2.Memory > 1048576 || c.Auth.Argon2.Threads > 64 {
		f.bad("auth.argon2: parameters out of sane range (max time 4096, memory 1048576 KiB, threads 64)")
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
	if !strings.HasPrefix(c.Server.PublicURL, "https://") && c.Auth.SecureCookies() {
		w = append(w, "cookies are Secure but public_url is plain http — Secure cookies cannot be delivered over http; set auth.cookie_secure: false deliberately (spec §9.1)")
	}
	if c.Server.HSTSMaxAge > 0 && !c.Server.TLS.Enabled {
		w = append(w, "server.hsts_max_age > 0 but TLS is disabled — HSTS headers will never be sent")
	}
	if c.Scratchpad.CodeExec.Enabled {
		if c.Scratchpad.CodeExec.AllowNetwork {
			w = append(w, "code_execution.allow_network=true: sandboxed scripts may reach the network from inside the jail — keep off unless required (spec §9.6/§9.9)")
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
	defer func() { _ = os.Remove(name) }()
	if _, err := f.WriteString("x"); err != nil {
		_ = f.Close()
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

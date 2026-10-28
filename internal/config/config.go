// Package config loads, environment-expands, and validates Gabkeep's YAML
// configuration file (spec §2.2). Unknown keys are a hard error so typos are
// caught at startup rather than silently ignored.
package config

import (
	"context"
	"crypto/tls"
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
	Email      Email      `yaml:"email"`
	RateLimit  RateLimit  `yaml:"rate_limit"` // retired; see Validate
	Logging    Logging    `yaml:"logging"`

	// Path is the config file path; relative dirs resolve against its folder.
	Path string `yaml:"-"`
	// TrustedNets is the parsed form of Server.TrustedProxies (spec §9.10).
	TrustedNets []netip.Prefix `yaml:"-"`
	// rawDoc is the decoded document as written, kept for Validate: after
	// setDefaults an absent cap and a spelled-out 0 are the same int, and
	// only the raw keys can tell them apart (cap syntax, §9.11).
	// Unexported — invisible to yaml in both directions.
	rawDoc map[string]any
}

type Server struct {
	Listen         string   `yaml:"listen"`
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
	// The caps below share the house syntax for optional limits (§9.11):
	// positive → that value; -1 → disables the control; absent → the code
	// default; 0 and any other negative → startup error. Plain ints — the
	// sentinel frees absent (Go zero) to mean "take the default".
	// requests_per_minute and chat_messages_per_hour moved here from the
	// retired rate_limit block.
	ConcurrentStreamsPerUser int `yaml:"concurrent_streams_per_user"`
	// ConcurrentStreamsTotal caps generations running at once across ALL
	// users; the global backstop behind the per-user cap (§9.11). One
	// shared LLM endpoint saturates long before a handful of users each
	// hit their personal cap.
	ConcurrentStreamsTotal int `yaml:"concurrent_streams_total"`
	// Per-user API token bucket over non-chat endpoints (§9.11).
	RequestsPerMinute int `yaml:"requests_per_minute"`
	// Per-user hourly chat-message bucket (§9.11).
	ChatMessagesPerHour int `yaml:"chat_messages_per_hour"`
	// HashConcurrency bounds concurrent Argon2 computations across ALL
	// users — the global admission gate behind the per-IP login/register
	// buckets (audit finding 4). Default 1: the parameters are heavy
	// (64 MiB, 4 threads) and hashing is sub-second, so serialising costs
	// honest users nothing while a flood can never exceed one hash's worth
	// of memory and CPU. -1 disables the gate (queue stays bounded).
	HashConcurrency int `yaml:"hash_concurrency"`
	// ConcurrentUploads bounds simultaneous in-flight uploads across ALL
	// users — temp files plus the atomic quota commit together (audit
	// finding 5). Default 8: a browser tops out around 6 parallel uploads
	// per host, so honest sessions never touch it, while the per-minute
	// request bucket alone would let a script wave unbounded
	// max_file_size temporaries through. -1 disables (unbounded).
	ConcurrentUploads int `yaml:"concurrent_uploads"`
}

type Database struct {
	Dir string `yaml:"dir"`
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
	// BlockExtensions rejects uploads whose (final) extension matches.
	// There is NO built-in list: absent or empty means every file type may
	// be uploaded. Safety does not rest on this gate — the serve layer
	// always sends Content-Disposition: attachment (§9.4), so an uploaded
	// .html can never execute in the app's origin no matter what it is.
	// Use this only to keep file types you never want on the disk at all.
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
	MaxFailedLogins     int      `yaml:"max_failed_logins"`
	// MaxSignupsPerHour is the GLOBAL account-creation budget, enforced only
	// in registration: open (audit1 M-2 — per-IP buckets are worthless to a
	// proxy pool, and with email on each flood request is a mail sent from
	// the operator's SMTP host). House cap syntax: positive → that value,
	// -1 disables (LAN operators), absent → 5/hour, 0/other negatives are a
	// startup error. Invite-only needs no budget: the codes are it.
	MaxSignupsPerHour int    `yaml:"max_signups_per_hour"`
	Argon2            Argon2 `yaml:"argon2"`
}

// defaultMaxSignupsPerHour is the global creation budget applied when
// max_signups_per_hour is absent from the config file.
const defaultMaxSignupsPerHour = 5

// SignupsPerHour resolves the raw cap the way the handler needs it: absent
// (0) means the default, -1 ("off") reads as 0 = no budget, positive is
// itself. Struct-literal configs (tests) behave like Load'ed ones.
func (a Auth) SignupsPerHour() int {
	switch {
	case a.MaxSignupsPerHour == 0:
		return defaultMaxSignupsPerHour
	case a.MaxSignupsPerHour < 0:
		return 0
	default:
		return a.MaxSignupsPerHour
	}
}

type Argon2 struct {
	Time    int `yaml:"time"`
	Memory  int `yaml:"memory"` // KiB
	Threads int `yaml:"threads"`
}

// Email is the master switch ("email_features") for password reset links and
// email confirmation of sign-ups/addresses. Disabled by default: no mail is
// sent and no flow asks anyone to confirm anything. Flipping it on later
// never touches existing accounts — only sign-ups registered *while* it is
// enabled enter the confirm-before-login flow.
type Email struct {
	Enabled bool `yaml:"enabled"`
	// BaseURL is the absolute origin reset/confirm links are built from,
	// e.g. https://host.example:3210. Required when enabled: links must
	// come from operator config, never from a (spoofable) Host header.
	BaseURL string `yaml:"base_url"`
	SMTP    SMTP   `yaml:"smtp"`
}

type SMTP struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"` // default 587; 465 means implicit TLS
	// AuthenticatedSMTP: true → AUTH LOGIN after the connection is TLS
	// (STARTTLS is required before sending credentials, fail-closed);
	// false → unauthenticated send, the LAN-relay case.
	AuthenticatedSMTP bool   `yaml:"authenticated_smtp"`
	Username          string `yaml:"username"`
	Password          string `yaml:"password"` // ${ENV} expansion recommended
	From              string `yaml:"from"`     // envelope sender + From header
}

// Addr is host:port for dialing.
func (s SMTP) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// RateLimit is retired: its two knobs moved into limits (§9.11). The type
// exists only so Validate can name the move to operators of old configs
// instead of yaml's generic unknown-key error.
type RateLimit struct {
	RequestsPerMinute   *int `yaml:"requests_per_minute"` // retired
	ChatMessagesPerHour *int `yaml:"chat_messages_per_hour"`
}

// Shipped defaults for the optional caps (limits.* and
// auth.max_signups_per_hour). The sample config spells these same values
// out; -1 on any of them turns that single control off, absent takes the
// default, 0 and other negatives are a startup error (§9.11).
const (
	DefaultRequestsPerMinute      = 120
	DefaultChatMessagesPerHour    = 120
	DefaultConcurrentStreamsUser  = 2
	DefaultConcurrentStreamsTotal = 8
	DefaultHashConcurrency        = 1
	DefaultConcurrentUploads      = 8
)

// capKnobs lists every optional cap for validation: full key path plus a
// reader for the raw (post-decode) value. -1 is the sentinel that disables
// the control; everything else negative — including 0, which would read as
// "allow nothing" on knobs phrased as maxima — is refused at startup.
var capKnobs = []struct {
	name string
	read func(*Config) int
}{
	{"limits.concurrent_streams_per_user", func(c *Config) int { return c.Limits.ConcurrentStreamsPerUser }},
	{"limits.concurrent_streams_total", func(c *Config) int { return c.Limits.ConcurrentStreamsTotal }},
	{"limits.requests_per_minute", func(c *Config) int { return c.Limits.RequestsPerMinute }},
	{"limits.chat_messages_per_hour", func(c *Config) int { return c.Limits.ChatMessagesPerHour }},
	{"limits.hash_concurrency", func(c *Config) int { return c.Limits.HashConcurrency }},
	{"limits.concurrent_uploads", func(c *Config) int { return c.Limits.ConcurrentUploads }},
	{"auth.max_signups_per_hour", func(c *Config) int { return c.Auth.MaxSignupsPerHour }},
}

// capNumber reads an integer out of the untyped YAML document (yaml.v3
// hands back int for values that fit).
func capNumber(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
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
// Whole-line YAML comments (first non-space char '#') pass through
// verbatim: docs commonly SHOW the syntax (the shipped template's header
// mentions ${ENV_VAR}), and a comment must never cost an install its
// startup. Values — including trailing comments after a value — still
// expand, so a typo'd secret reference fails loudly (§9.12).
func expandEnv(in string) (string, error) {
	var missing []string
	expand := func(m string) string {
		g := envRe.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(g[1]); ok {
			return v
		}
		if strings.Contains(m, ":-") {
			return g[2]
		}
		missing = append(missing, g[1])
		return m
	}
	lines := strings.Split(in, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lines[i] = envRe.ReplaceAllStringFunc(line, expand)
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable(s) not set: %s (use ${VAR:-} to allow unset)", strings.Join(missing, ", "))
	}
	return strings.Join(lines, "\n"), nil
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
	// Second, untyped pass for Validate's cap-syntax check: it sees which
	// keys were actually spelled out before defaults touched anything.
	var rawDoc map[string]any
	if err := yaml.Unmarshal([]byte(expanded), &rawDoc); err == nil {
		c.rawDoc = rawDoc
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
	// Optional caps (house syntax): absent (0) takes the code default; -1
	// is the off sentinel and passes through; validation rejects 0 spelled
	// out plus every other negative. Shipped configs spell the defaults out
	// so the effective policy is visible at a glance.
	if c.Limits.ConcurrentStreamsPerUser == 0 {
		c.Limits.ConcurrentStreamsPerUser = DefaultConcurrentStreamsUser
	}
	if c.Limits.ConcurrentStreamsTotal == 0 {
		c.Limits.ConcurrentStreamsTotal = DefaultConcurrentStreamsTotal
	}
	if c.Limits.RequestsPerMinute == 0 {
		c.Limits.RequestsPerMinute = DefaultRequestsPerMinute
	}
	if c.Limits.ChatMessagesPerHour == 0 {
		c.Limits.ChatMessagesPerHour = DefaultChatMessagesPerHour
	}
	if c.Limits.HashConcurrency == 0 {
		c.Limits.HashConcurrency = DefaultHashConcurrency
	}
	if c.Limits.ConcurrentUploads == 0 {
		c.Limits.ConcurrentUploads = DefaultConcurrentUploads
	}
	if c.Auth.MaxSignupsPerHour == 0 {
		c.Auth.MaxSignupsPerHour = defaultMaxSignupsPerHour
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
	// §9.4: the configured list is the whole rule — normalize to lowercase
	// dotted form and dedupe; nothing is merged in from a built-in default.
	{
		seen := map[string]bool{}
		merged := make([]string, 0, len(c.Scratchpad.BlockExtensions))
		for _, e := range c.Scratchpad.BlockExtensions {
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
		c.Scratchpad.CodeExec.MaxMemory = 256 << 20 // matches configs/config.yaml
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
	// The default model is always pickable: a conversation with no explicit
	// choice RUNS on it, and the picker shows the model actually in use —
	// it must be able to name (and re-select) it. Validation reads the same
	// list, so allowing it here keeps picker and API honest with each other.
	if c.LLM.Model != "" {
		found := false
		for _, m := range c.LLM.Models {
			if m == c.LLM.Model {
				found = true
				break
			}
		}
		if !found {
			c.LLM.Models = append(c.LLM.Models, c.LLM.Model)
		}
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
	if c.Email.SMTP.Port == 0 {
		c.Email.SMTP.Port = 587
	}
	// Links are BaseURL + "/#reset?t=…": a trailing slash here would
	// produce "//#" in every mail, so normalize once at load time.
	c.Email.BaseURL = strings.TrimRight(c.Email.BaseURL, "/")
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
	// Cap syntax (§9.11): positive limits the control, -1 disables it,
	// absent took the default in setDefaults — 0 spelled out (which would
	// read as "allow nothing" on knobs phrased as maxima) and every other
	// negative are startup errors naming the syntax. The typed pass cannot
	// see an explicit 0 (defaults already collapsed it), hence rawDoc.
	for _, k := range capKnobs {
		if v := k.read(c); v < -1 {
			f.bad("%s: must be a positive limit, or -1 to disable the control", k.name)
			continue
		}
		section, key, _ := strings.Cut(k.name, ".")
		if sec, ok := c.rawDoc[section].(map[string]any); ok {
			if raw, ok := sec[key]; ok {
				if n, isNum := capNumber(raw); isNum && n == 0 {
					f.bad("%s: must be a positive limit, or -1 to disable the control", k.name)
				}
			}
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

	// email: only coherent-when-enabled — a disabled section needs nothing
	// (the default), so upgrading an old config costs zero keys.
	if c.Email.Enabled {
		if c.Email.SMTP.Host == "" {
			f.bad("email.smtp.host: required when email.enabled is true")
		}
		if c.Email.SMTP.Port < 1 || c.Email.SMTP.Port > 65535 {
			f.bad("email.smtp.port: must be 1..65535")
		}
		if !strings.Contains(c.Email.SMTP.From, "@") {
			f.bad("email.smtp.from: must be an email address when email.enabled is true")
		}
		if c.Email.SMTP.AuthenticatedSMTP {
			if c.Email.SMTP.Username == "" {
				f.bad("email.smtp.username: required when authenticated_smtp is true")
			}
			if c.Email.SMTP.Port == 25 {
				f.bad("email.smtp: authenticated_smtp on port 25 is refused — credentials must not cross a cleartext connection (use 587 with STARTTLS or 465)")
			}
		}
		if c.Email.BaseURL == "" {
			f.bad("email.base_url: required when email.enabled is true (links must come from operator config, never from the request's Host header)")
		} else if err := validOrigin(c.Email.BaseURL); err != nil {
			f.bad("email.base_url: %v", err)
		}
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
	if c.Auth.Registration == "open" && !c.Email.Enabled {
		w = append(w, "registration is open while email addresses are unverified — enable email.enabled or use invite-only (spec §9.12)")
	}
	if c.Email.Enabled && !strings.HasPrefix(c.Email.BaseURL, "https://") {
		w = append(w, "email.base_url is not https — password-reset and confirmation tokens would cross the network in the clear")
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

// ProbeSMTP dials the configured SMTP server for -check-config. It checks
// reachability (and a working TLS handshake on the implicit-TLS port 465)
// only: no message is sent, no credentials are ever transmitted or logged —
// a dial cannot prove the username/password work anyway. Callers should
// treat failure as a warning, never a deploy blocker.
func (c *Config) ProbeSMTP(ctx context.Context) error {
	if !c.Email.Enabled {
		return nil
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.Email.SMTP.Addr())
	if err != nil {
		return err
	}
	defer conn.Close()
	if c.Email.SMTP.Port == 465 {
		tconn := tls.Client(conn, &tls.Config{ServerName: c.Email.SMTP.Host, MinVersion: tls.VersionTLS12})
		dl, ok := ctx.Deadline()
		if !ok {
			dl = time.Now().Add(10 * time.Second)
		}
		_ = tconn.SetDeadline(dl)
		if err := tconn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("TLS handshake: %w", err)
		}
	}
	return nil
}

// validOrigin accepts http(s)://host[:port] with nothing below the authority:
// mail links are built by string-append onto this value, so a path, query or
// fragment here is always a typo.
func validOrigin(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return err
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("scheme must be http or https")
	case u.Host == "":
		return fmt.Errorf("host required")
	case u.Path != "":
		return fmt.Errorf("must not contain a path (links are appended to the origin)")
	case u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("must not contain a query or fragment")
	}
	return nil
}

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

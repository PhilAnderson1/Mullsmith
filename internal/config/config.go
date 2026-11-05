// Package config loads, environment-expands, and validates Mullsmith's YAML
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
	"mullsmith/internal/thinking"
)

// Duration is a YAML string parsed by time.ParseDuration, e.g. "720h", "30s".
type Duration time.Duration

// offDuration is the house off-sentinel (§2.2): the bare YAML number -1,
// a flag rather than a duration. It is not a legal duration in its own
// right; consumers treat any negative as "off", and validation lets only
// this one survive on off-switch knobs (-1s, -2… are typos).
const offDuration = Duration(-1)

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	// The off-sentinel is the only bare number a duration accepts (§2.2).
	if value.Tag == "!!int" && value.Value == "-1" {
		*d = offDuration
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf(`must be a duration string like "30s" or "720h" — the only bare number is -1, which turns supported controls off`)
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

// HTTPDeadline reads a duration the way http.Server does: the house
// off-sentinel (negative; validation leaves only the bare -1 standing)
// means Go's 0, "no deadline".
func (d Duration) HTTPDeadline() time.Duration {
	if d < 0 {
		return 0
	}
	return d.D()
}

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
	AppLimits  AppLimits  `yaml:"app_limits"`
	Database   Database   `yaml:"database"`
	Scratchpad Scratchpad `yaml:"scratchpad"`
	LLM        LLM        `yaml:"llm"`
	Tools      Tools      `yaml:"tools"`
	MCP        MCP        `yaml:"mcp"`
	Auth       Auth       `yaml:"auth"`
	Email      Email      `yaml:"email"`
	RateLimit  RateLimit  `yaml:"rate_limit"` // retired; see Validate
	Logging    Logging    `yaml:"logging"`

	// Path is the config file path; relative dirs resolve against its folder.
	Path string `yaml:"-"`
	// Thinking is the resolved thinking-control runtime (catalogues
	// parsed, selection cross-checked) or nil when the feature is not
	// configured — legacy behaviour untouched. Built by Validate.
	Thinking *thinking.Runtime `yaml:"-"`
	// TrustedNets is the parsed form of Server.TrustedProxies (spec §9.10).
	TrustedNets []netip.Prefix `yaml:"-"`
	// rawDoc is the decoded document as written, kept for Validate: after
	// setDefaults an absent cap and a spelled-out 0 are the same int, and
	// only the raw keys can tell them apart (cap syntax, §9.11).
	// Unexported — invisible to yaml in both directions.
	prepared bool
	rawDoc   map[string]any
}

type Server struct {
	Listen       string `yaml:"listen"`
	TLS          TLS    `yaml:"tls"`
	MaxBodyBytes Size   `yaml:"max_body_bytes"`
	// Admission caps take positive values, or -1 to disable; omission uses defaults.
	MaxConnections     int `yaml:"max_connections"`
	ConcurrentRequests int `yaml:"concurrent_requests"`
	// Read and shutdown durations are positive; write_timeout also accepts -1.
	ReadTimeout    Duration `yaml:"read_timeout"`
	WriteTimeout   Duration `yaml:"write_timeout"` // default 60s; -1 = no write cap (streams self-manage)
	ShutdownGrace  Duration `yaml:"shutdown_grace"`
	TrustedProxies []string `yaml:"trusted_proxies"`
	HSTSMaxAge     int64    `yaml:"hsts_max_age"` // seconds; -1 = header off (the default)
}

type TLS struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type AppLimits struct {
	MaxMessageChars int `yaml:"max_message_chars"`
	MaxAttachments  int `yaml:"max_attachments"`
	// VisionMaxImageBytes is the per-picture cap for view_image across
	// all vision models (default 4 MiB).
	VisionMaxImageBytes Size `yaml:"vision_max_image_bytes"`
	// VisionMaxImagesPerConversation bounds the active image context carried
	// by one conversation. view_image turns persist into later requests, so
	// the cap applies both while a tool loop grows and when history is rebuilt
	// (default 8; deleted or expired image files no longer consume a slot).
	VisionMaxImagesPerConversation int `yaml:"vision_max_images_per_conversation"`
	// The caps below share the house syntax for optional limits (§9.11):
	// positive → that value; -1 → disables the control; absent → the code
	// default; 0 and any other negative → startup error. Plain ints — the
	// sentinel frees absent (Go zero) to mean "take the default".
	// requests_per_minute and chat_messages_per_hour moved here from the
	// retired rate_limit block.
	MaxConversationsPerUser  int `yaml:"max_conversations_per_user"`
	MaxMessagesPerUser       int `yaml:"max_messages_per_user"`
	ConcurrentStreamsPerUser int `yaml:"concurrent_streams_per_user"`
	// ConcurrentStreamsTotal caps generations running at once across ALL
	// users; the global backstop behind the per-user cap (§9.11). One
	// shared LLM endpoint saturates long before a handful of users each
	// hit their personal cap.
	ConcurrentStreamsTotal int `yaml:"concurrent_streams_total"`
	// Per-user API attempt bucket, including chat sends and regeneration (§9.11).
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
	// ConcurrentUploadsPerUser prevents one account monopolizing upload capacity.
	ConcurrentUploadsPerUser int `yaml:"concurrent_uploads_per_user"`
	// ConcurrentDownloadsPerUser bounds active file transfers for each account.
	ConcurrentDownloadsPerUser int `yaml:"concurrent_downloads_per_user"`
}

type Database struct {
	Dir string `yaml:"dir"`
}

type Scratchpad struct {
	Root        string `yaml:"root"`
	MaxFileSize Size   `yaml:"max_file_size"`
	UserQuota   Size   `yaml:"user_quota"`
	// MaxEntries is also the quota scan ceiling; layout directories count.
	MaxEntries  int      `yaml:"max_entries"`
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
	BlockExtensions []string `yaml:"block_extensions"`
}

type CodeExecution struct {
	Enabled         bool      `yaml:"enabled"`
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

// LLMModel is one entry of the llm.models picker (§2.2): the model ID
// sent to the endpoint, whether conversations with no explicit choice run
// on it, which thinking-profile catalogue entry names its dropdown, and
// its context window. Every fact about a model lives ON its entry —
// nothing is implied by position except the fallback default — so the
// list has one honest shape with no shorthand form.
type LLMModel struct {
	ID              string `yaml:"id"`
	Default         bool   `yaml:"default"`
	ThinkingProfile string `yaml:"thinking_profile"`
	// ContextSize is the window in tokens: a positive number pins it;
	// omitted (or -1, normalised by setDefaults) discovers it from the
	// endpoint's /models listing and falls back to unlimited — meter by
	// count, no send gate — when the endpoint says nothing (spec §6.1).
	ContextSize int `yaml:"context_size"`
	// Vision says this model can be shown pictures: the view_image tool
	// is offered on its conversations and the turns it creates carry
	// OpenAI image_url content parts (§7.2). OFF unless the entry says
	// otherwise: a text-only endpoint must never receive a content-part
	// array it cannot parse.
	Vision bool `yaml:"vision"`
}

type LLM struct {
	Endpoint string `yaml:"endpoint"`
	APIKey   string `yaml:"api_key"`
	// Models is the one list: the picker, and where each model's
	// behaviour lives. Exactly one entry may claim default: true; none
	// claiming it means the first entry (so a one-model deployment needs
	// no marking at all — Validate still requires at least one entry).
	Models             []LLMModel `yaml:"models"`
	DefaultTemperature float64    `yaml:"default_temperature"`
	// RequestTimeout is retired: no request path ever read it (streaming
	// deliberately carries no global client timeout, and the one
	// non-streaming quest bounds itself). It exists only so Validate can
	// tell operators running old configs what changed instead of failing
	// on an unknown key.
	RequestTimeout    *Duration `yaml:"request_timeout"` // retired
	MaxTokensResponse int       `yaml:"max_tokens_response"`
	ShowReasoning     bool      `yaml:"show_reasoning"`
	// Upstream progress budgets (C8) — PROGRESS budgets, never total
	// timeouts: a long reasoning run that keeps sending data is legitimate
	// and must never be cut short. upstream_header_timeout caps the wait
	// for the endpoint's response headers; upstream_stall_timeout caps the
	// SILENCE between bytes once the stream has started. Any byte counts as
	// transport progress, SSE comment heartbeats included — an endpoint
	// that keeps the pipe talking is alive as far as the transport is
	// concerned, whether it has model progress to show is its business.
	// House off-syntax (§2.2): absent → default, -1 → off, 0 or any other
	// negative → startup error.
	UpstreamHeaderTimeout Duration `yaml:"upstream_header_timeout"`
	UpstreamStallTimeout  Duration `yaml:"upstream_stall_timeout"`

	// Thinking control (thinking-control-specification.md §2). All three
	// are optional: with none spelled, the existing transport is kept
	// and no thinking selector appears. The choice LIST lives on the
	// model entries (llm.models[].thinking_profile): the dropdown
	// describes a model, and picker models may differ. There is no
	// selector switch: the dropdown appears exactly when at least one
	// entry names a real profile (§6.3). endpoint_type stays here — it
	// is a property of the URL, one request convention per deployment.
	// The two files are the sole source of the catalogues — nothing is
	// built in beyond "none" — so endpoint_type requires endpoints_file
	// and a named profile requires profiles_file (relative paths resolve
	// against this config file's directory). There is deliberately no
	// default_thinking here on purpose: a conversation starts at the
	// built-in Default and the user's choice is conversation state, not
	// another global LLM setting.
	EndpointsFile string `yaml:"endpoints_file"`
	ProfilesFile  string `yaml:"profiles_file"`
	EndpointType  string `yaml:"endpoint_type"`
}

// DefaultModelID is the entry conversations with no explicit choice run
// on: the one marked default (setDefaults guarantees one exists whenever
// the list does), else the first. "" only with no models at all, which
// Validate refuses.
func (l *LLM) DefaultModelID() string {
	for _, m := range l.Models {
		if m.Default {
			return m.ID
		}
	}
	if len(l.Models) > 0 {
		return l.Models[0].ID
	}
	return ""
}

// ModelNames is the picker list, config order.
func (l *LLM) ModelNames() []string {
	names := make([]string, 0, len(l.Models))
	for _, m := range l.Models {
		names = append(names, m.ID)
	}
	return names
}

// ThinkingProfileFor names the profile entry in force for a model: its
// own, or the default entry's for anything not on the list (a
// conversation still running a model the picker has since dropped).
func (l *LLM) ThinkingProfileFor(model string) string {
	for _, m := range l.Models {
		if m.ID == model {
			return m.ThinkingProfile
		}
	}
	for _, m := range l.Models {
		if m.Default {
			return m.ThinkingProfile
		}
	}
	if len(l.Models) > 0 {
		return l.Models[0].ThinkingProfile
	}
	return ""
}

type Tools struct {
	WebSearch   WebSearch     `yaml:"web_search"`
	WebFetch    WebFetch      `yaml:"web_fetch"`
	WebDownload WebDownload   `yaml:"web_download"`
	CodeRun     CodeExecution `yaml:"code_run"`
	Agent       Agent         `yaml:"agent"`
}

type WebSearch struct {
	Enabled    bool   `yaml:"enabled"`
	Provider   string `yaml:"provider"` // brave — the only implemented provider
	APIKey     string `yaml:"api_key"`
	MaxResults int    `yaml:"max_results"`
}

type WebFetch struct {
	Enabled  bool     `yaml:"enabled"`
	MaxBytes Size     `yaml:"max_bytes"`
	Timeout  Duration `yaml:"timeout"`
}

type WebDownload struct {
	Enabled  bool     `yaml:"enabled"`
	MaxBytes Size     `yaml:"max_bytes"`
	Timeout  Duration `yaml:"timeout"`
}

type Agent struct {
	MaxToolRounds       int               `yaml:"max_tool_rounds"`
	MaxToolCallsPerTurn int               `yaml:"max_tool_calls_per_turn"`
	ToolCards           string            `yaml:"tool_cards"` // useful | all
	Approval            map[string]string `yaml:"approval"`   // tool → auto | confirm
}

// MCP configures administrator-managed Model Context Protocol servers.
// Remote authentication and user-managed servers require a separate security
// model; stdio commands are deliberately administrator-only configuration.
type MCP struct {
	StateDir string      `yaml:"state_dir"`
	Servers  []MCPServer `yaml:"servers"`
}

type MCPServer struct {
	ID          string            `yaml:"id"`
	Name        string            `yaml:"name"`
	Transport   string            `yaml:"transport"`
	URL         string            `yaml:"url"`
	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args"`
	Environment map[string]string `yaml:"environment"`
	Approval    string            `yaml:"approval"`
	StateDir    string            `yaml:"-"`
}

// validApprovalTool keeps approval policy fail-fast: a misspelled tool name
// must not be accepted and then silently fall back to its default policy.
func validApprovalTool(name string) bool {
	switch name {
	case "web_search", "web_fetch", "web_download",
		"fs_list", "fs_read", "fs_write", "fs_delete", "files_share",
		"display_image", "view_image", "code_run":
		return true
	default:
		return false
	}
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

// Shipped defaults for the optional caps (server.*, app_limits.*, and
// auth.max_signups_per_hour). The sample config spells these same values
// out; -1 on any of them turns that single control off, absent takes the
// default, 0 and other negatives are a startup error (§9.11).
const (
	DefaultMaxConversationsPerUser    = 1000
	DefaultMaxMessagesPerUser         = 10000
	DefaultRequestsPerMinute          = 120
	DefaultChatMessagesPerHour        = 120
	DefaultConcurrentStreamsUser      = 2
	DefaultConcurrentStreamsTotal     = 8
	DefaultHashConcurrency            = 1
	DefaultConcurrentUploads          = 8
	DefaultConcurrentUploadsPerUser   = 2
	DefaultConcurrentDownloadsPerUser = 2
	DefaultMaxConnections             = 128
	DefaultConcurrentRequests         = 32
)

// capKnobs lists every optional cap for validation: full key path plus a
// reader for the raw (post-decode) value. -1 is the sentinel that disables
// the control; everything else negative — including 0, which would read as
// "allow nothing" on knobs phrased as maxima — is refused at startup.
var capKnobs = []struct {
	name string
	read func(*Config) int
}{
	{"app_limits.max_conversations_per_user", func(c *Config) int { return c.AppLimits.MaxConversationsPerUser }},
	{"app_limits.max_messages_per_user", func(c *Config) int { return c.AppLimits.MaxMessagesPerUser }},
	{"app_limits.concurrent_streams_per_user", func(c *Config) int { return c.AppLimits.ConcurrentStreamsPerUser }},
	{"app_limits.concurrent_streams_total", func(c *Config) int { return c.AppLimits.ConcurrentStreamsTotal }},
	{"app_limits.requests_per_minute", func(c *Config) int { return c.AppLimits.RequestsPerMinute }},
	{"app_limits.chat_messages_per_hour", func(c *Config) int { return c.AppLimits.ChatMessagesPerHour }},
	{"app_limits.hash_concurrency", func(c *Config) int { return c.AppLimits.HashConcurrency }},
	{"app_limits.concurrent_downloads_per_user", func(c *Config) int { return c.AppLimits.ConcurrentDownloadsPerUser }},
	{"app_limits.concurrent_uploads_per_user", func(c *Config) int { return c.AppLimits.ConcurrentUploadsPerUser }},
	{"app_limits.concurrent_uploads", func(c *Config) int { return c.AppLimits.ConcurrentUploads }},
	{"server.max_connections", func(c *Config) int { return c.Server.MaxConnections }},
	{"server.concurrent_requests", func(c *Config) int { return c.Server.ConcurrentRequests }},
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

// rawList reads a section's list-of-mappings from the as-written document
// (context_size's cap-syntax check runs here: a decoded 0 cannot tell
// "omitted" from "spelled"). Missing or malformed → nil.
func (c *Config) rawList(section, key string) []map[string]any {
	sec, ok := c.rawDoc[section].(map[string]any)
	if !ok {
		return nil
	}
	items, ok := sec[key].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// spelledZero reports whether the config file spells section.key out as
// the number 0 — invisible to typed reads once setDefaults has folded an
// absent key into its default (§2.2: 0 is never a legal spelling on an
// off-switch knob).
func (c *Config) spelledZero(section, key string) bool {
	sec, ok := c.rawDoc[section].(map[string]any)
	if !ok {
		return false
	}
	raw, ok := sec[key]
	if !ok {
		return false
	}
	n, isNum := capNumber(raw)
	return isNum && n == 0
}

// spelledZeroDur is spelledZero for Duration knobs, whose zero can arrive
// spelled either way: a duration string ("0s", "0h" — and yaml.v3 hands
// bare numbers to the string decode too, where ParseDuration accepts a
// unitless "0"). Any zero folds into the default typed read, the same
// trap the caps rawDoc walk exists for.
func (c *Config) spelledZeroDur(section, key string) bool {
	sec, ok := c.rawDoc[section].(map[string]any)
	if !ok {
		return false
	}
	raw, ok := sec[key]
	if !ok {
		return false
	}
	if s, isStr := raw.(string); isStr {
		d, err := time.ParseDuration(s)
		return err == nil && d == 0
	}
	n, isNum := capNumber(raw)
	return isNum && n == 0
}

type Logging struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json
	File   string `yaml:"file"`   // empty = stderr
}

// --- loading --------------------------------------------------------------

// envRe matches ${NAME} and ${NAME:-default}.
var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var reservedMCPEnv = map[string]bool{
	"HOME": true, "LANG": true, "PATH": true, "TMPDIR": true,
	"XDG_CACHE_HOME": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
}

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
	// First, an untyped pass: it sees the file as written (so retired
	// keys can answer with a migration message before the strict decoder
	// below fails on them with a raw YAML complaint), and it feeds
	// Validate's cap-syntax check.
	var c Config
	var rawDoc map[string]any
	if err := yaml.Unmarshal([]byte(expanded), &rawDoc); err == nil {
		c.rawDoc = rawDoc
		if err := legacyModelKeys(rawDoc); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
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
	if err := c.Prepare(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Defaults supplies the same pure defaults used by file loading. Callers
// may override fields before Prepare validates and resolves the runtime.
func Defaults() *Config { c := &Config{}; c.setDefaults(); return c }

// Prepare is the single application construction path. Once prepared,
// configurations are immutable; constructors do not repair them silently.
func (c *Config) Prepare() error {
	if c.prepared {
		return nil
	}
	c.setDefaults()
	if c.Path != "" {
		c.LLM.EndpointsFile = resolveAgainst(filepath.Dir(c.Path), c.LLM.EndpointsFile)
		c.LLM.ProfilesFile = resolveAgainst(filepath.Dir(c.Path), c.LLM.ProfilesFile)
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Path != "" {
		c.resolvePaths()
	}
	for i := range c.MCP.Servers {
		if c.MCP.Servers[i].Transport == "stdio" {
			c.MCP.Servers[i].StateDir = filepath.Join(c.MCP.StateDir, c.MCP.Servers[i].ID)
		}
	}
	rt, err := thinking.Build(thinking.Settings{
		EndpointURL:  c.LLM.Endpoint,
		EndpointType: c.LLM.EndpointType,
		// The choice list is per model now: the default entry's profile
		// is the deployment-level one (and the fallback for models that
		// left the picker), and every entry's own name rides along.
		ProfileID:     c.LLM.ThinkingProfileFor(c.LLM.DefaultModelID()),
		ModelProfiles: c.LLM.ModelProfiles(),
		MaxOutput:     c.LLM.MaxTokensResponse,
		EndpointsFile: c.LLM.EndpointsFile,
		ProfilesFile:  c.LLM.ProfilesFile,
	})
	if err != nil {
		return err
	}
	c.Thinking = rt
	c.prepared = true
	return nil
}

// WithDefaults normalizes standalone runner options using the same policy
// as the complete application configuration.
func (ce CodeExecution) WithDefaults() CodeExecution {
	if ce.Timeout == 0 {
		ce.Timeout = Duration(30 * time.Second)
	}
	if ce.MaxOutput == 0 {
		ce.MaxOutput = 256 << 10
	}
	if ce.MaxMemory == 0 {
		ce.MaxMemory = 1 << 30
	}
	return ce
}

// legacyModelKeys answers the pre-picker era of model config with the
// exact new spelling instead of a raw YAML complaint from the strict
// decoder: `model:`, an llm-level `thinking_profile:`, `vision:` and
// `max_context_tokens:` are retired keys (they live on the entries
// now), `thinking_selector_enable:` is retired outright, and string
// items under `models:` predate the entry maps.
func legacyModelKeys(doc map[string]any) error {
	llmSec, ok := doc["llm"].(map[string]any)
	if !ok {
		return nil
	}
	if _, ok := llmSec["model"]; ok {
		return errors.New(`llm.model has moved into llm.models — write, e.g., ` +
			`models: [{id: "<model>", default: true}] (default may be omitted when only one model is listed)`)
	}
	if _, ok := llmSec["thinking_profile"]; ok {
		return errors.New(`llm.thinking_profile has moved into llm.models — spell thinking_profile on each entry: ` +
			`- {id: "<model>", thinking_profile: <profile-id>}`)
	}
	if _, ok := llmSec["max_context_tokens"]; ok {
		return errors.New(`llm.max_context_tokens has moved into llm.models — spell context_size on each entry ` +
			`(omit it or write -1 to auto-detect from the endpoint): - {id: "<model>", context_size: 131072}`)
	}
	if _, ok := llmSec["vision"]; ok {
		return errors.New(`llm.vision has moved into llm.models — spell vision: true on each entry ` +
			`that can see pictures: - {id: "<model>", vision: true} (an entry without it stays text-only)`)
	}
	if _, ok := llmSec["thinking_selector_enable"]; ok {
		return errors.New(`llm.thinking_selector_enable is retired — the dropdown appears by itself when at ` +
			`least one llm.models entry spells a real thinking_profile (none, or no profile, stays hidden)`)
	}
	if items, ok := llmSec["models"].([]any); ok {
		for _, it := range items {
			if _, isMap := it.(map[string]any); !isMap {
				return errors.New(`llm.models entries are maps now — spell each one, e.g., ` +
					`- {id: "<model>", default: true, thinking_profile: <profile-id>}`)
			}
		}
	}
	return nil
}

// ModelProfiles projects the picker entries onto their profile ids for
// the thinking runtime; an entry spelling no profile contributes ""
// (= built-in "none" there).
func (l *LLM) ModelProfiles() map[string]string {
	out := make(map[string]string, len(l.Models))
	for _, m := range l.Models {
		out[m.ID] = m.ThinkingProfile
	}
	return out
}

// entry looks up a model's picker entry: the entry naming the model,
// else the default entry, else the first — the fallback every per-model
// property shares (models outside the picker behave like the default).
func (l *LLM) entry(model string) *LLMModel {
	for i := range l.Models {
		if l.Models[i].ID == model {
			return &l.Models[i]
		}
	}
	for i := range l.Models {
		if l.Models[i].Default {
			return &l.Models[i]
		}
	}
	if len(l.Models) > 0 {
		return &l.Models[0]
	}
	return nil
}

// ContextSizeFor pins the window in force for a model: its entry's
// positive number, or 0 meaning auto (discover, unlimited if unknown).
// Models outside the picker read the default entry — the same fallback
// as the thinking profile.
func (l *LLM) ContextSizeFor(model string) int {
	m := l.entry(model)
	if m != nil && m.ContextSize > 0 {
		return m.ContextSize
	}
	return 0
}

// VisionFor reports whether the model may be shown pictures: its entry's
// vision flag (models outside the picker read the default entry — the
// same fallback as every other per-model property, spec §7.2).
func (l *LLM) VisionFor(model string) bool {
	m := l.entry(model)
	return m != nil && m.Vision
}

// AnyVision reports whether at least one entry can see pictures — the
// one condition under which view_image is registered at all.
func (l *LLM) AnyVision() bool {
	for _, m := range l.Models {
		if m.Vision {
			return true
		}
	}
	return false
}

// AnyContextAuto reports whether at least one entry leaves its window to
// discovery — the probe's only reason to run.
func (l *LLM) AnyContextAuto() bool {
	for _, m := range l.Models {
		if m.ContextSize <= 0 {
			return true
		}
	}
	return false
}

// resolveAgainst anchors one configured path at base and cleans it;
// "" passes through (unset).
func resolveAgainst(base, p string) string {
	if p == "" {
		return p
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

func (c *Config) resolvePaths() {
	base := filepath.Dir(c.Path)
	resolve := func(p string) string { return resolveAgainst(base, p) }
	c.Database.Dir = resolve(c.Database.Dir)
	c.Scratchpad.Root = resolve(c.Scratchpad.Root)
	c.MCP.StateDir = resolve(c.MCP.StateDir)
	if c.Logging.File != "" {
		c.Logging.File = resolve(c.Logging.File)
	}
}

// --- defaults -------------------------------------------------------------

func (c *Config) setDefaults() {
	if c.MCP.StateDir == "" && c.Scratchpad.Root != "" {
		c.MCP.StateDir = filepath.Join(filepath.Dir(c.Scratchpad.Root), "mcp")
	}
	for i := range c.MCP.Servers {
		if c.MCP.Servers[i].Name == "" {
			c.MCP.Servers[i].Name = c.MCP.Servers[i].ID
		}
		if c.MCP.Servers[i].Approval == "" {
			c.MCP.Servers[i].Approval = "confirm"
		}
	}
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:8080"
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = Duration(30 * time.Second)
	}
	// write_timeout defaults ON (astra2 finding 1): without a write cap a
	// client that stops reading can block a response write in the kernel
	// forever, pinning handler, goroutine and connection — the read and
	// idle timeouts do not bound it. Go arms this once per request before
	// the handler runs (net/http), so the surfaces with legitimately long
	// writes manage their own deadlines: SSE lifts it per stream and arms
	// per frame (sse.go), downloads roll a per-chunk deadline
	// (api_files.go). -1 remains the operator off-switch (§2.2), mapped
	// to Go's 0 (no deadline) at the http.Server.
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = Duration(60 * time.Second)
	}
	// HSTS absent means off, likewise spelled with the house -1 (§2.2).
	if c.Server.HSTSMaxAge == 0 {
		c.Server.HSTSMaxAge = -1
	}
	if c.Server.ShutdownGrace == 0 {
		c.Server.ShutdownGrace = Duration(15 * time.Second)
	}
	if c.Server.MaxConnections == 0 {
		c.Server.MaxConnections = DefaultMaxConnections
	}
	if c.Server.ConcurrentRequests == 0 {
		c.Server.ConcurrentRequests = DefaultConcurrentRequests
	}
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = 1 << 20
	}
	if c.AppLimits.MaxMessageChars == 0 {
		c.AppLimits.MaxMessageChars = 32000
	}
	if c.AppLimits.MaxAttachments == 0 {
		c.AppLimits.MaxAttachments = 8
	}
	if c.AppLimits.VisionMaxImageBytes == 0 {
		c.AppLimits.VisionMaxImageBytes = 4 << 20
	}
	if c.AppLimits.VisionMaxImagesPerConversation == 0 {
		c.AppLimits.VisionMaxImagesPerConversation = 8
	}
	// Optional caps (house syntax): absent (0) takes the code default; -1
	// is the off sentinel and passes through; validation rejects 0 spelled
	// out plus every other negative. Shipped configs spell the defaults out
	// so the effective policy is visible at a glance.
	if c.AppLimits.MaxConversationsPerUser == 0 {
		c.AppLimits.MaxConversationsPerUser = DefaultMaxConversationsPerUser
	}
	if c.AppLimits.MaxMessagesPerUser == 0 {
		c.AppLimits.MaxMessagesPerUser = DefaultMaxMessagesPerUser
	}
	if c.AppLimits.ConcurrentStreamsPerUser == 0 {
		c.AppLimits.ConcurrentStreamsPerUser = DefaultConcurrentStreamsUser
	}
	if c.AppLimits.ConcurrentStreamsTotal == 0 {
		c.AppLimits.ConcurrentStreamsTotal = DefaultConcurrentStreamsTotal
	}
	if c.AppLimits.RequestsPerMinute == 0 {
		c.AppLimits.RequestsPerMinute = DefaultRequestsPerMinute
	}
	if c.AppLimits.ChatMessagesPerHour == 0 {
		c.AppLimits.ChatMessagesPerHour = DefaultChatMessagesPerHour
	}
	if c.AppLimits.HashConcurrency == 0 {
		c.AppLimits.HashConcurrency = DefaultHashConcurrency
	}
	if c.AppLimits.ConcurrentDownloadsPerUser == 0 {
		c.AppLimits.ConcurrentDownloadsPerUser = DefaultConcurrentDownloadsPerUser
	}
	if c.AppLimits.ConcurrentUploadsPerUser == 0 {
		c.AppLimits.ConcurrentUploadsPerUser = DefaultConcurrentUploadsPerUser
	}
	if c.AppLimits.ConcurrentUploads == 0 {
		c.AppLimits.ConcurrentUploads = DefaultConcurrentUploads
	}
	if c.Auth.MaxSignupsPerHour == 0 {
		c.Auth.MaxSignupsPerHour = defaultMaxSignupsPerHour
	}
	if c.Scratchpad.MaxFileSize == 0 {
		c.Scratchpad.MaxFileSize = 25 << 20
	}
	if c.Scratchpad.MaxEntries == 0 {
		c.Scratchpad.MaxEntries = 1000
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
	c.Tools.CodeRun = c.Tools.CodeRun.WithDefaults()
	// Any entry leaving context_size unset (or spelled -1) discovers its
	// window from the endpoint (§2.2); a positive number pins it. The
	// internal sentinel for "auto" is -1.
	for i := range c.LLM.Models {
		if c.LLM.Models[i].ContextSize <= 0 {
			c.LLM.Models[i].ContextSize = -1
		}
	}
	if c.LLM.MaxTokensResponse == 0 {
		c.LLM.MaxTokensResponse = 4096
	}
	// None of the entries claims default ⇒ the first one is it: the
	// fallback that lets a one-model list skip the marking entirely
	// (Validate caps the claims at one, so after this there is exactly
	// one whenever the list has any entries).
	if len(c.LLM.Models) > 0 {
		marked := false
		for _, m := range c.LLM.Models {
			if m.Default {
				marked = true
				break
			}
		}
		if !marked {
			c.LLM.Models[0].Default = true
		}
	}
	// Upstream progress budgets (C8): absent takes the shipped default;
	// the off sentinel (-1) survives to mean "no cap" for the client.
	if c.LLM.UpstreamHeaderTimeout == 0 {
		c.LLM.UpstreamHeaderTimeout = Duration(90 * time.Second)
	}
	if c.LLM.UpstreamStallTimeout == 0 {
		c.LLM.UpstreamStallTimeout = Duration(120 * time.Second)
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
	if c.Tools.WebDownload.MaxBytes == 0 {
		c.Tools.WebDownload.MaxBytes = 1 << 30
	}
	if c.Tools.WebDownload.Timeout == 0 {
		c.Tools.WebDownload.Timeout = Duration(60 * time.Second)
	}
	if c.Tools.Agent.MaxToolRounds == 0 {
		c.Tools.Agent.MaxToolRounds = 20
	}
	if c.Tools.Agent.MaxToolCallsPerTurn == 0 {
		c.Tools.Agent.MaxToolCallsPerTurn = 100
	}
	if c.Tools.Agent.ToolCards == "" {
		c.Tools.Agent.ToolCards = "useful"
	}
	if c.Tools.Agent.Approval == nil {
		c.Tools.Agent.Approval = map[string]string{}
	}
	for tool, def := range map[string]string{
		"web_search": "auto", "web_fetch": "auto", "web_download": "auto", "fs_list": "auto", "fs_read": "auto",
		"fs_write": "auto", "fs_delete": "confirm", "files_share": "auto", "display_image": "auto",
		"view_image": "auto", "code_run": "confirm",
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
	if len(c.MCP.Servers) > 8 {
		f.bad("mcp.servers: at most 8 servers may be configured")
	}
	mcpIDs := map[string]bool{}
	for i, server := range c.MCP.Servers {
		prefix := fmt.Sprintf("mcp.servers[%d]", i)
		if server.ID == "" || len(server.ID) > 24 {
			f.bad("%s.id: required and must be at most 24 characters", prefix)
		} else {
			valid := true
			for j, r := range server.ID {
				if !((r >= 'a' && r <= 'z') || (j > 0 && r >= '0' && r <= '9') || (j > 0 && r == '_')) {
					valid = false
				}
			}
			if !valid {
				f.bad("%s.id: use lowercase letters, numbers and underscores, starting with a letter", prefix)
			}
			if mcpIDs[server.ID] {
				f.bad("mcp.servers: duplicate id %q", server.ID)
			}
			mcpIDs[server.ID] = true
		}
		if server.Transport != "streamable_http" && server.Transport != "stdio" {
			f.bad("%s.transport: must be streamable_http or stdio", prefix)
		}
		if strings.TrimSpace(server.Name) == "" || len([]rune(server.Name)) > 80 {
			f.bad("%s.name: required and must be at most 80 characters", prefix)
		}
		switch server.Transport {
		case "streamable_http":
			u, err := url.Parse(server.URL)
			switch {
			case server.URL == "":
				f.bad("%s.url: required for streamable_http", prefix)
			case err != nil:
				f.bad("%s.url: %v", prefix, err)
			case u.Scheme != "http" && u.Scheme != "https":
				f.bad("%s.url: scheme must be http or https", prefix)
			case u.Host == "":
				f.bad("%s.url: host required", prefix)
			case u.User != nil:
				f.bad("%s.url: embedded credentials are not supported", prefix)
			case u.Fragment != "":
				f.bad("%s.url: fragments are not supported", prefix)
			}
			if server.Command != "" || len(server.Args) != 0 || len(server.Environment) != 0 {
				f.bad("%s: command, args and environment are only valid for stdio", prefix)
			}
		case "stdio":
			if server.URL != "" {
				f.bad("%s.url: not valid for stdio", prefix)
			}
			if server.Command == "" || !filepath.IsAbs(server.Command) {
				f.bad("%s.command: an absolute executable path is required for stdio", prefix)
			} else if strings.IndexByte(server.Command, 0) >= 0 || len(server.Command) > 4096 {
				f.bad("%s.command: must not contain NUL and must be at most 4096 bytes", prefix)
			}
			if len(server.Args) > 64 {
				f.bad("%s.args: at most 64 arguments may be configured", prefix)
			}
			for j, arg := range server.Args {
				if strings.IndexByte(arg, 0) >= 0 || len(arg) > 8192 {
					f.bad("%s.args[%d]: must not contain NUL and must be at most 8192 bytes", prefix, j)
				}
			}
			if len(server.Environment) > 32 {
				f.bad("%s.environment: at most 32 variables may be configured", prefix)
			}
			for key, value := range server.Environment {
				if !envNameRe.MatchString(key) {
					f.bad("%s.environment: invalid variable name %q", prefix, key)
				}
				if reservedMCPEnv[key] {
					f.bad("%s.environment: %s is managed by Mullsmith", prefix, key)
				}
				if strings.IndexByte(value, 0) >= 0 || len(value) > 65536 {
					f.bad("%s.environment.%s: must not contain NUL and must be at most 65536 bytes", prefix, key)
				}
			}
		}
		if server.Approval != "auto" && server.Approval != "confirm" {
			f.bad("%s.approval: must be auto or confirm", prefix)
		}
	}

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
	// hsts_max_age is one of the file's two real off-switches outside
	// limits, so it follows the house syntax (§2.2): positive seconds,
	// -1 off, 0 or any other negative a startup error. (0 would be the
	// old spelling; setDefaults has already folded an absent key to -1,
	// hence the rawDoc check.)
	if h := c.Server.HSTSMaxAge; h < 0 && h != -1 {
		f.bad("server.hsts_max_age: must be a positive number of seconds, or -1 to disable")
	}
	if c.spelledZero("server", "hsts_max_age") {
		f.bad("server.hsts_max_age: 0 is not the off spelling — write -1 to disable")
	}
	// Plain durations take no sentinel (§2.2): a negative is always a
	// typo whose runtime effect is a deadline that has already passed —
	// every request or run dying instantly. (write_timeout and
	// hsts_max_age, the file's real off-switches, are handled beside this
	// table with the house -1 syntax.)
	for _, k := range []struct {
		name string
		v    Duration
	}{
		{"server.read_timeout", c.Server.ReadTimeout},
		{"server.shutdown_grace", c.Server.ShutdownGrace},
		{"scratchpad.artifact_ttl", c.Scratchpad.ArtifactTTL},
		{"scratchpad.work_ttl", c.Scratchpad.WorkTTL},
		{"tools.code_run.timeout", c.Tools.CodeRun.Timeout},
		{"tools.web_fetch.timeout", c.Tools.WebFetch.Timeout},
		{"tools.web_download.timeout", c.Tools.WebDownload.Timeout},
	} {
		if k.v < 0 {
			f.bad("%s: must be >= 0 (durations take no off-sentinel)", k.name)
		}
	}
	// write_timeout: the off-switch whose ON side is the default
	// (astra2 finding 1) — it follows the house syntax (§2.2), the flag is
	// the bare -1, never a duration spelling like -1s.
	if wt := c.Server.WriteTimeout; wt < 0 && wt != offDuration {
		f.bad("server.write_timeout: must be a positive duration like 30s, or -1 for no write cap")
	}
	if c.spelledZeroDur("server", "write_timeout") {
		f.bad("server.write_timeout: 0 is not the off spelling — write -1 for no write cap")
	}
	// The two upstream progress budgets (C8) are off-switches too and
	// follow the same house syntax: a positive duration, or the bare -1
	// for no cap; 0 spelled out and every other negative are errors.
	for _, k := range []struct {
		key string
		v   Duration
	}{
		{"upstream_header_timeout", c.LLM.UpstreamHeaderTimeout},
		{"upstream_stall_timeout", c.LLM.UpstreamStallTimeout},
	} {
		if k.v < 0 && k.v != offDuration {
			f.bad("llm.%s: must be a positive duration like 30s, or -1 for no cap", k.key)
		}
		if c.spelledZeroDur("llm", k.key) {
			f.bad("llm.%s: 0 is not the off spelling — write -1 for no cap", k.key)
		}
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

	if c.Server.MaxBodyBytes <= 0 {
		f.bad("server.max_body_bytes: must be > 0")
	}
	if c.AppLimits.MaxMessageChars <= 0 || c.AppLimits.MaxAttachments <= 0 {
		f.bad("app_limits: max_message_chars and max_attachments must be > 0")
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
		if c.spelledZero(section, key) {
			f.bad("%s: must be a positive limit, or -1 to disable the control", k.name)
		}
	}
	// rate_limit: retired — the two knobs moved into app_limits:. Name the
	// move instead of silently ignoring values an old config still carries.
	if c.RateLimit.RequestsPerMinute != nil || c.RateLimit.ChatMessagesPerHour != nil {
		f.bad("rate_limit: retired — move requests_per_minute and chat_messages_per_hour into the app_limits: block")
	}
	// llm.request_timeout: retired — nothing ever read it (streaming
	// deliberately has no global client timeout; the titling quest bounds
	// itself). Say so rather than let the strict decoder echo a raw key.
	if c.LLM.RequestTimeout != nil {
		f.bad("llm.request_timeout: retired — no request ever used this knob; delete this key")
	}

	if c.Database.Dir == "" {
		f.bad("database.dir: required")
	}
	if c.Scratchpad.Root == "" {
		f.bad("scratchpad.root: required")
	}
	if c.MCP.StateDir == "" {
		f.bad("mcp.state_dir: required")
	}
	if c.Scratchpad.MaxEntries < 3 || c.Scratchpad.MaxEntries == int(^uint(0)>>1) {
		f.bad("scratchpad.max_entries: must be at least 3 and below the maximum integer (layout directories count)")
	}
	if c.Scratchpad.MaxFileSize <= 0 || c.Scratchpad.UserQuota <= 0 {
		f.bad("scratchpad: max_file_size and user_quota must be > 0")
	}
	// One sandbox, always: bubblewrap (§9.9).
	ce := &c.Tools.CodeRun
	if ce.Enabled {
		if len(ce.AllowedRuntimes) == 0 {
			f.bad("tools.code_run.allowed_runtimes: required when code execution is enabled")
		}
		seen := map[string]bool{}
		for _, r := range ce.AllowedRuntimes {
			switch {
			case r.ID == "":
				f.bad("tools.code_run.allowed_runtimes: id required")
			case seen[r.ID]:
				f.bad("tools.code_run.allowed_runtimes: duplicate id %q", r.ID)
			case r.Binary == "" || !filepath.IsAbs(r.Binary):
				f.bad("tools.code_run.allowed_runtimes[%s]: binary must be an absolute path", r.ID)
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
	// llm.models: the one list. At least one entry; ids required and
	// unique; at most one default claim (the none-claimed case folded
	// onto the first entry by setDefaults).
	if len(c.LLM.Models) == 0 {
		f.bad(`llm.models: required — spell at least one entry, e.g., - {id: "<model>", default: true}`)
	}
	seenModel := map[string]bool{}
	defaults := 0
	for i, m := range c.LLM.Models {
		if strings.TrimSpace(m.ID) == "" {
			f.bad("llm.models[%d].id: required", i)
		} else if seenModel[m.ID] {
			f.bad("llm.models: duplicate entry %q", m.ID)
		} else {
			seenModel[m.ID] = true
		}
		if m.Default {
			defaults++
		}
	}
	if defaults > 1 {
		f.bad("llm.models: at most one entry may set default: true")
	}
	// llm.models[].context_size: a positive number pins the window; -1
	// (or omitted) auto-detects from the endpoint's /models listing. A
	// spelled 0 folds into that default like on every cap knob, so the
	// house syntax is enforced from the raw document instead.
	for i, v := range c.rawList("llm", "models") {
		raw, ok := v["context_size"]
		if !ok {
			continue
		}
		n, isNum := capNumber(raw)
		if !isNum {
			f.bad("llm.models[%d].context_size: must be a number of tokens, -1 (auto), or omitted", i)
		} else if n == 0 {
			f.bad("llm.models[%d].context_size: 0 is not allowed — omit it or write -1 to auto-detect, or a positive number", i)
		}
	}
	// A pinned window must still leave room for the reply itself.
	for _, m := range c.LLM.Models {
		if m.ContextSize > 0 && c.LLM.MaxTokensResponse >= m.ContextSize {
			f.bad("llm.max_tokens_response: must be smaller than the pinned context_size of %q (%d)", m.ID, m.ContextSize)
		}
	}
	if c.LLM.MaxTokensResponse <= 0 {
		f.bad("llm.max_tokens_response: must be > 0")
	}
	if c.LLM.DefaultTemperature < 0 || c.LLM.DefaultTemperature > 2 {
		f.bad("llm.default_temperature: must be within 0..2")
	}
	if c.AppLimits.VisionMaxImageBytes < 0 {
		f.bad("app_limits.vision_max_image_bytes: must be >= 0")
	}
	if c.AppLimits.VisionMaxImagesPerConversation < 1 {
		f.bad("app_limits.vision_max_images_per_conversation: must be > 0")
	}

	switch c.Tools.WebSearch.Provider {
	case "brave":
	default:
		f.bad("tools.web_search.provider: only \"brave\" is implemented")
	}
	if c.Tools.WebSearch.Enabled && c.Tools.WebSearch.APIKey == "" {
		f.bad("tools.web_search.api_key: required for the brave provider (try ${MULLSMITH_SEARCH_API_KEY:-})")
	}
	if c.Tools.WebFetch.MaxBytes <= 0 {
		f.bad("tools.web_fetch.max_bytes: must be > 0")
	}
	if c.Tools.WebDownload.MaxBytes <= 0 || c.Tools.WebDownload.MaxBytes == Size(1<<63-1) {
		f.bad("tools.web_download.max_bytes: must be > 0 and less than 9223372036854775807")
	}
	if c.Tools.Agent.MaxToolCallsPerTurn <= 0 {
		f.bad("tools.agent.max_tool_calls_per_turn: must be > 0")
	}
	if c.Tools.Agent.ToolCards != "useful" && c.Tools.Agent.ToolCards != "all" {
		f.bad("tools.agent.tool_cards: must be useful or all")
	}
	for tool, mode := range c.Tools.Agent.Approval {
		if !validApprovalTool(tool) {
			f.bad("tools.agent.approval.%s: unknown tool", tool)
			continue
		}
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
	for _, server := range c.MCP.Servers {
		if server.Transport != "streamable_http" {
			continue
		}
		u, err := url.Parse(server.URL)
		if err == nil && u.Scheme == "http" && !privateOrLoopbackHost(u.Hostname()) {
			w = append(w, fmt.Sprintf("mcp server %q uses plain HTTP to a non-private host — tool queries and results can be read or changed in transit", server.ID))
		}
	}
	// I-4: a default-route entry makes every client a "trusted proxy", so
	// X-Forwarded-For decides which per-IP bucket (login/register floods,
	// the whole anonymous story) gets throttled — attacker-controlled.
	// Warning, not refusal: someone may genuinely sit behind all-zeros.
	for _, p := range c.TrustedNets {
		if p.Bits() == 0 {
			w = append(w, fmt.Sprintf("server.trusted_proxies contains %s — ANY client's X-Forwarded-For is honored, so source-IP buckets can be spoofed at will; list only your real proxies' addresses (spec §9.10)", p))
		}
	}
	if c.Tools.CodeRun.Enabled {
		if c.Tools.CodeRun.AllowNetwork {
			w = append(w, "tools.code_run.allow_network=true: sandboxed scripts may reach the network from inside the jail — keep off unless required (spec §9.6/§9.9)")
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

func privateOrLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && (ip.IsLoopback() || ip.IsPrivate())
}

// --- runtime checks (used at startup and by -check-config) -----------------

// Check is one line of the -check-config report.
type Check struct {
	Name   string
	Detail string
	OK     bool
}

func (c *Config) hasStdioMCP() bool {
	for _, server := range c.MCP.Servers {
		if server.Transport == "stdio" {
			return true
		}
	}
	return false
}

// EnsureDirs creates the database, scratchpad and configured MCP state roots.
func (c *Config) EnsureDirs() error {
	dirs := []string{c.Database.Dir, c.Scratchpad.Root}
	if c.hasStdioMCP() {
		dirs = append(dirs, c.MCP.StateDir)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

// CheckIO validates application-owned writable roots and TLS files; no network
// access. MCP stdio state is intentionally not created here: -check-config is
// commonly run as root while the service runs unprivileged, and creating a new
// state tree here would give it the wrong owner. The service creates that tree
// through EnsureDirs, and MCP connectivity checks use disposable state.
func (c *Config) CheckIO() []Check {
	var out []Check
	dirs := []struct{ name, path string }{
		{"database.dir", c.Database.Dir},
		{"scratchpad.root", c.Scratchpad.Root},
	}
	for _, d := range dirs {
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
	f, err := os.CreateTemp(dir, ".mullsmith-check-*")
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

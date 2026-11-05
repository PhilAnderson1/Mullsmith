package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"mullsmith/internal/config"
)

const (
	mcpStartupTimeout        = 10 * time.Second
	mcpResponseHeaderTimeout = 60 * time.Second
	mcpMaxEventBytes         = 1 << 20
	mcpMaxTools              = 100
	mcpMaxSchemaBytes        = 256 << 10
	mcpMaxSchemaDepth        = 32
)

type mcpToolCaller interface {
	CallTool(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error)
}

// mcpTool adapts a remotely advertised MCP tool to Mullsmith's ordinary tool
// path. It therefore inherits approval, audit, cancellation, result clamping,
// persistence and live tool-card behaviour without a parallel execution path.
type mcpTool struct {
	name        string
	remoteName  string
	serverName  string
	description string
	parameters  map[string]any
	caller      mcpToolCaller
	logger      *slog.Logger
	secrets     []string
}

func (t *mcpTool) Name() string               { return t.name }
func (t *mcpTool) Description() string        { return t.description }
func (t *mcpTool) Parameters() map[string]any { return t.parameters }

func (t *mcpTool) Execute(ctx context.Context, inv Invocation) (Result, error) {
	args := map[string]any{}
	if strings.TrimSpace(inv.Arguments) != "" {
		if err := json.Unmarshal([]byte(inv.Arguments), &args); err != nil {
			return Result{}, fmt.Errorf("invalid tool arguments")
		}
	}
	result, err := t.caller.CallTool(ctx, &mcpsdk.CallToolParams{Name: t.remoteName, Arguments: args})
	if err != nil {
		if t.logger != nil {
			t.logger.Warn("MCP tool call failed", "server", t.serverName, "tool", t.remoteName,
				"error", redactMCPError(err, t.secrets))
		}
		return Result{}, fmt.Errorf("MCP tool call failed")
	}
	b, err := json.Marshal(result)
	if err != nil {
		return Result{}, fmt.Errorf("MCP tool returned an invalid result")
	}
	summary := fmt.Sprintf("Called %s: %s", t.serverName, t.remoteName)
	if result.IsError {
		summary = fmt.Sprintf("%s reported an error: %s", t.serverName, t.remoteName)
	}
	return Result{Content: string(b), Summary: summary, Failed: result.IsError}, nil
}

func registerMCP(cfg *config.Config, logger *slog.Logger, reg *Registry, policies map[string]string) {
	type discovery struct {
		session *mcpsdk.ClientSession
		tools   []*mcpsdk.Tool
		close   func() error
		err     error
	}
	discovered := make([]discovery, len(cfg.MCP.Servers))
	var wg sync.WaitGroup
	for i, server := range cfg.MCP.Servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), mcpStartupTimeout)
			defer cancel()
			discovered[i].session, discovered[i].tools, discovered[i].close, discovered[i].err = connectMCP(ctx, logger, server)
		}()
	}
	wg.Wait()

	// Registration remains in config order even though discovery is parallel,
	// keeping model tool specifications stable between restarts.
	for i, server := range cfg.MCP.Servers {
		found := discovered[i]
		if found.err != nil {
			logger.Warn("MCP server unavailable; tools skipped", "server", server.ID,
				"error", redactMCPError(found.err, mcpSecretValues(server)))
			continue
		}
		session, remoteTools, closeSession := found.session, found.tools, found.close
		var caller mcpToolCaller = session
		if server.Transport == "stdio" {
			restarting := newRestartingMCPClient(server, logger, session, closeSession)
			caller = restarting
			closeSession = restarting.Close
		}

		adapted := make([]*mcpTool, 0, len(remoteTools))
		secrets := mcpSecretValues(server)
		sort.Slice(remoteTools, func(i, j int) bool { return remoteTools[i].Name < remoteTools[j].Name })
		seen := map[string]bool{}
		valid := true
		for _, remote := range remoteTools {
			name := mcpToolName(server.ID, remote.Name)
			if name == "" || len(name) > 64 || seen[name] {
				logger.Warn("MCP server advertised unusable tool names; server skipped", "server", server.ID)
				valid = false
				break
			}
			if _, collision := reg.Get(name); collision {
				logger.Warn("MCP tool name collides with a registered tool; server skipped", "server", server.ID, "tool", name)
				valid = false
				break
			}
			params, err := mcpParameters(remote.InputSchema)
			if err != nil {
				logger.Warn("MCP server advertised an invalid input schema; server skipped", "server", server.ID, "tool", remote.Name)
				valid = false
				break
			}
			desc := strings.TrimSpace(remote.Description)
			if desc == "" {
				desc = "Tool supplied by the configured MCP server."
			}
			desc = fmt.Sprintf("[%s MCP server] %s", server.Name, Elide(desc, 4096))
			adapted = append(adapted, &mcpTool{name: name, remoteName: remote.Name,
				serverName: server.Name, description: desc, parameters: params, caller: caller,
				logger: logger, secrets: secrets})
			seen[name] = true
		}
		if !valid {
			_ = closeSession()
			continue
		}
		for _, tool := range adapted {
			reg.Register(tool)
			policies[tool.name] = server.Approval
		}
		reg.AddCloser(closeSession)
		logger.Info("MCP server connected", "server", server.ID, "tools", len(adapted))
	}
}

func connectMCP(ctx context.Context, logger *slog.Logger, server config.MCPServer) (*mcpsdk.ClientSession, []*mcpsdk.Tool, func() error, error) {
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "mullsmith", Version: "1"},
		&mcpsdk.ClientOptions{Logger: mcpSafeLogger(logger, server)})
	var (
		transport mcpsdk.Transport
		cleanup   func() error
	)
	switch server.Transport {
	case "streamable_http":
		transport, cleanup = mcpHTTPTransport(server)
	case "stdio":
		stdioTransport, err := newMCPStdioTransport(server)
		if err != nil {
			return nil, nil, nil, err
		}
		transport = stdioTransport
		cleanup = stdioTransport.Close
	default:
		return nil, nil, nil, fmt.Errorf("unsupported MCP transport")
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, err
	}
	closeSession := func() error {
		err := session.Close()
		if server.Transport == "streamable_http" {
			err = errors.Join(err, cleanup())
		}
		return err
	}
	all, err := listMCPTools(ctx, session)
	if err != nil {
		_ = closeSession()
		return nil, nil, nil, err
	}
	return session, all, closeSession, nil
}

func mcpHTTPTransport(server config.MCPServer) (mcpsdk.Transport, func() error) {
	endpoint, _ := url.Parse(server.URL) // config validation has already succeeded
	httpTransport := &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: mcpResponseHeaderTimeout}
	authTransport := &mcpAuthTransport{base: httpTransport, headers: mcpAuthHeaders(server)}
	httpClient := &http.Client{
		Transport: authTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !sameMCPOrigin(req.URL, endpoint) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	transport := &mcpsdk.StreamableClientTransport{Endpoint: server.URL, HTTPClient: httpClient,
		MaxRetries: 3, DisableStandaloneSSE: true, MaxEventSize: mcpMaxEventBytes}
	return transport, func() error {
		httpTransport.CloseIdleConnections()
		return nil
	}
}

func listMCPTools(ctx context.Context, session *mcpsdk.ClientSession) ([]*mcpsdk.Tool, error) {
	var all []*mcpsdk.Tool
	cursor := ""
	for {
		result, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		all = append(all, result.Tools...)
		if len(all) > mcpMaxTools {
			return nil, fmt.Errorf("server advertises more than %d tools", mcpMaxTools)
		}
		if result.NextCursor == "" {
			return all, nil
		}
		cursor = result.NextCursor
	}
}

// CheckMCPServers verifies that configured servers can negotiate a session and
// advertise a bounded tool list. It is used only by -check-config.
func CheckMCPServers(ctx context.Context, cfg *config.Config, logger *slog.Logger) []config.Check {
	checks := make([]config.Check, 0, len(cfg.MCP.Servers))
	for _, server := range cfg.MCP.Servers {
		probeServer := server
		var removeProbeState func()
		if server.Transport == "stdio" {
			tempState, err := os.MkdirTemp("", "mullsmith-mcp-check-"+server.ID+"-")
			if err != nil {
				checks = append(checks, config.Check{Name: "mcp." + server.ID, Detail: "create temporary state: " + err.Error()})
				continue
			}
			probeServer.StateDir = tempState
			removeProbeState = func() { _ = os.RemoveAll(tempState) }
		}
		probeCtx, cancel := context.WithTimeout(ctx, mcpStartupTimeout)
		session, advertised, closeSession, err := connectMCP(probeCtx, logger, probeServer)
		cancel()
		if session != nil {
			_ = closeSession()
		}
		if removeProbeState != nil {
			removeProbeState()
		}
		check := config.Check{Name: "mcp." + server.ID, OK: err == nil}
		if err != nil {
			// The remote error can contain an arbitrary response body. Keep the
			// operator-facing check useful without reflecting untrusted content or
			// credentials supplied by a badly behaved server.
			check.Detail = "connection, authentication or protocol negotiation failed"
		} else {
			check.Detail = fmt.Sprintf("connected; %d tool(s) advertised", len(advertised))
		}
		checks = append(checks, check)
	}
	return checks
}

func mcpParameters(schema any) (map[string]any, error) {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	if len(b) > mcpMaxSchemaBytes {
		return nil, fmt.Errorf("input schema exceeds %d bytes", mcpMaxSchemaBytes)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil || out == nil {
		return nil, fmt.Errorf("input schema must be a JSON object")
	}
	if schemaDepth(out, 0) > mcpMaxSchemaDepth {
		return nil, fmt.Errorf("input schema is nested too deeply")
	}
	return out, nil
}

func schemaDepth(v any, depth int) int {
	max := depth
	switch x := v.(type) {
	case map[string]any:
		for _, child := range x {
			if d := schemaDepth(child, depth+1); d > max {
				max = d
			}
		}
	case []any:
		for _, child := range x {
			if d := schemaDepth(child, depth+1); d > max {
				max = d
			}
		}
	}
	return max
}

func mcpToolName(serverID, remote string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(remote)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			if r <= unicode.MaxASCII {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		} else {
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "_-")
	if name == "" {
		return ""
	}
	return "mcp_" + serverID + "_" + name
}

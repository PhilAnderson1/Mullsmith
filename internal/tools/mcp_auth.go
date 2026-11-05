package tools

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"mullsmith/internal/config"
)

// mcpAuthTransport applies administrator-configured credentials to every
// request after cloning it. Redirect policy is enforced by the owning client,
// so credentials can only reach the configured origin.
type mcpAuthTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (t *mcpAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for name, values := range t.headers {
		clone.Header.Del(name)
		for _, value := range values {
			clone.Header.Add(name, value)
		}
	}
	return t.base.RoundTrip(clone)
}

func mcpAuthHeaders(server config.MCPServer) http.Header {
	headers := make(http.Header, len(server.Headers)+1)
	if server.BearerToken != "" {
		headers.Set("Authorization", "Bearer "+server.BearerToken)
	}
	for name, value := range server.Headers {
		headers.Set(name, value)
	}
	return headers
}

func sameMCPOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func mcpSecretValues(server config.MCPServer) []string {
	values := make([]string, 0, len(server.Headers)+1)
	if server.BearerToken != "" {
		values = append(values, server.BearerToken)
	}
	for _, value := range server.Headers {
		if value != "" {
			values = append(values, value)
		}
	}
	// Replace longer overlapping values first.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

func redactMCPText(text string, secrets []string) string {
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	return text
}

func redactMCPError(err error, secrets []string) error {
	if err == nil || len(secrets) == 0 {
		return err
	}
	return errors.New(redactMCPText(err.Error(), secrets))
}

func mcpSafeLogger(logger *slog.Logger, server config.MCPServer) *slog.Logger {
	secrets := mcpSecretValues(server)
	if logger == nil || len(secrets) == 0 {
		return logger
	}
	return slog.New(&mcpRedactingHandler{next: logger.Handler(), secrets: secrets})
}

type mcpRedactingHandler struct {
	next    slog.Handler
	secrets []string
}

func (h *mcpRedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *mcpRedactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, redactMCPText(record.Message, h.secrets), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(redactMCPAttr(attr, h.secrets))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *mcpRedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		clean[i] = redactMCPAttr(attr, h.secrets)
	}
	return &mcpRedactingHandler{next: h.next.WithAttrs(clean), secrets: h.secrets}
}

func (h *mcpRedactingHandler) WithGroup(name string) slog.Handler {
	return &mcpRedactingHandler{next: h.next.WithGroup(name), secrets: h.secrets}
}

func redactMCPAttr(attr slog.Attr, secrets []string) slog.Attr {
	value := attr.Value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(redactMCPText(value.String(), secrets))
	case slog.KindAny:
		if err, ok := value.Any().(error); ok {
			attr.Value = slog.AnyValue(redactMCPError(err, secrets))
		}
	case slog.KindGroup:
		group := value.Group()
		for i := range group {
			group[i] = redactMCPAttr(group[i], secrets)
		}
		attr.Value = slog.GroupValue(group...)
	}
	return attr
}

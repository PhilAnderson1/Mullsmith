# Gabkeep

Self-hosted, single-binary chat assistant with a ChatGPT-style web UI in front
of a local OpenAI-compatible LLM endpoint. See [`spec.md`](spec.md) for the
full specification.

**Status: M0 (skeleton).** Config loading/validation, HTTP server with security
headers, `/healthz`, `/api/v1/meta`, `-check-config`. Accounts land in M1.

## Build

```sh
go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" ./cmd/gabkeep
```

## Run

```sh
./gabkeep -config dev.yaml            # serve (dev config binds 192.168.0.3:8080)
./gabkeep -config dev.yaml -check-config   # validate config + probe LLM endpoint
./gabkeep -version
```

Relative paths in the config (e.g. `./data/db`) resolve against the config
file's directory, so the binary can run from anywhere. Secrets can be kept out
of the file with `${ENV_VAR}` / `${ENV_VAR:-default}` expansion.

## Layout

```
cmd/gabkeep/        main: flags, config load, startup wiring, graceful shutdown
internal/config/    YAML schema, env expansion, strict validation
internal/server/    routing, middleware chain, security headers, embedded assets
internal/llm/       OpenAI-compatible client (M0: /models probe only)
web/                embedded UI assets
```

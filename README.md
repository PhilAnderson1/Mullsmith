# Gabkeep

Self-hosted, single-binary chat assistant with a ChatGPT-style web UI in front
of any local OpenAI-compatible LLM endpoint (llama.cpp, Ollama, vLLM, …).

* **Single static Go binary** — no runtime dependencies, no build step for the
  UI (vanilla ES modules embedded via `go:embed`), pure-Go SQLite.
* **Accounts** — email + password (argon2id), sessions with an optional
  "Stay logged in" cookie, invite-only or closed registration, login lockout.
* **Streaming chat** — SSE token streaming, stop/regenerate, markdown answers,
  dark/light theme, responsive from 320 px phones to ultrawide.
* **Tools with an approval layer** — `web_search` (Brave) and SSRF-guarded
  `web_fetch`, per-conversation **scratchpad files** (`fs_list/fs_read/
  fs_write/fs_delete`, uploads from the browser, artifact download cards),
  and optional sandboxed **`code_run`** (container or isolated mode). Every
  tool call is audited and shown as a card in the UI; policy decides which
  ones ask for confirmation first.
* **Privacy** — everything lives on your machine: SQLite history, per-user
  scratchpad directories. Only what you ask the model to search/fetch ever
  leaves the box, and only to the providers you configure.

Status: **M0–M7(a) complete** (see [`spec.md`](spec.md) §13) — hardening
artifacts under [`docs/`](docs/). See [Security](#security) for residual risks.

## Requirements

* 64-bit Linux (amd64/arm64). Anything else that runs Go works too.
* An OpenAI-compatible endpoint for the model. v1 is text-only (current
  deployments use text chat models; image input support may come later).
* To build: Go ≥ 1.24 (Ubuntu's `apt install golang-go` is fine if recent
  enough; otherwise grab the latest from https://go.dev/dl/).

## Build

```sh
make build            # ./gabkeep, statically linked, version/commit/date stamped
# or without make:
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always) -X main.commit=$(git rev-parse --short HEAD) -X main.buildDate=$(date -u +%FT%TZ)" \
  -o gabkeep ./cmd/gabkeep
```

## Quick install on a fresh Ubuntu box

```sh
# 1. Binary
sudo install -m 0755 gabkeep /usr/local/bin/gabkeep

# 2. Service user + state dirs
sudo useradd --system --home-dir /var/lib/gabkeep --shell /usr/sbin/nologin gabkeep
sudo mkdir -p /etc/gabkeep /var/lib/gabkeep
sudo chown gabkeep:gabkeep /var/lib/gabkeep && sudo chmod 700 /var/lib/gabkeep

# 3. Config (template with explanations: config.example.yaml)
sudo cp config.example.yaml /etc/gabkeep/config.yaml
sudo nano /etc/gabkeep/config.yaml         # set llm.endpoint + model at minimum
sudo chmod 640 /etc/gabkeep/config.yaml && sudo chown root:gabkeep /etc/gabkeep/config.yaml

# 4. Secrets (optional; referenced by ${ENV_VAR} expansion)
printf 'GABKEEP_SEARCH_API_KEY=...\n' | sudo tee /etc/gabkeep/env
sudo chmod 640 /etc/gabkeep/env && sudo chown root:gabkeep /etc/gabkeep/env

# 5. systemd
sudo cp deploy/gabkeep.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now gabkeep

# 6. Verify
/usr/local/bin/gabkeep -config /etc/gabkeep/config.yaml -check-config
sudo -u gabkeep gabkeep -config /etc/gabkeep/config.yaml admin create-user \
  -email you@example.org -name You -password 'a long passphrase'
curl -sk https://127.0.0.1:8080/healthz || curl -s http://127.0.0.1:8080/healthz
```

Then either enable `server.tls` in the config (with a real cert — Let's
Encrypt via `certbot` if public), or terminate TLS in a reverse proxy
(caddy/nginx) and set `server.trusted_proxies`. Gabkeep warns at startup when
bound to a non-loopback address without TLS — on a trusted LAN that may be
acceptable; read [Security](#security) first.

### CLI

```text
gabkeep -config CFG                          serve
gabkeep -config CFG -check-config            validate config + probe LLM endpoint
gabkeep -version                             version/commit/build date
gabkeep -config CFG admin invite [n]         mint invite codes (invite-only mode)
gabkeep -config CFG admin create-user -email E -name N -password P
gabkeep -config CFG admin reset-password <email>   (prompts; revokes all sessions)
```

Relative paths in the config resolve against the config file's directory, so
the binary can run from anywhere. `${VAR}` / `${VAR:-default}` expansion keeps
secrets out of the YAML.

## Backups

All state lives in two directories named by the config:

* `database.dir` — SQLite (`gabkeep.db` + WAL files)
* `scratchpad.root` — per-user `uploads/`, `artifacts/`, `work/`

Plus `/etc/gabkeep/config.yaml` and `/etc/gabkeep/env`.

**Consistent hot backup** (server can stay up; uses the `sqlite3` CLI so the
WAL is folded in correctly):

```sh
#!/bin/sh -e
DEST=/backups/gabkeep/$(date +%F)
mkdir -p "$DEST"
install -d -m 700 "$DEST"
sqlite3 /var/lib/gabkeep/db/gabkeep.db ".backup '$DEST/gabkeep.db'"
rsync -a --delete /var/lib/gabkeep/scratchpad "$DEST/scratchpad"
tar czf "$DEST/config.tgz" -C /etc gabkeep
```

Cold backup instead: `systemctl stop gabkeep && cp -a /var/lib/gabkeep …`.
Restore by stopping the service, copying the files back (preserve
`gabkeep:gabkeep` ownership and `0700`), and starting again. The scratchpad
holds untrusted uploaded content — keep backups encrypted at rest (`age`,
restic, or an encrypted mount); the database contains full conversation
history.

## Security

Design: `spec.md` §9 (auth, CSRF, XSS/Markdown sandbox, SQL, upload/MIME,
SSRF egress guard, scratchpad path sandbox, code-execution sandboxing, TLS,
rate limiting, logging, secrets, dependency policy) with a §9.16 OWASP Top-10
map. Release gates and what we actually ran:

```sh
make check            # go vet + go test -race + gosec + govulncheck
make fuzz-short       # 60 s of coverage-guided fuzzing on the path sandbox
```

Artifacts for the current release live in [`docs/reports/`](docs/reports)
(gosec, govulncheck, OWASP ZAP baseline) and the §9.16 re-audit with evidence
per line, false-positive justifications included, is
[`docs/security-audit-2026-10.md`](docs/security-audit-2026-10.md).

### Residual risks (read this)

* **No 2FA in v1.** Password + session cookies only. Use closed/invite-only
  registration and strong passphrases; keep the box LAN-only or behind a proxy
  with mTLS/VPN if that matters to your threat model.
* **Plain HTTP is opt-in.** If you run without TLS, everything — passwords,
  cookies, history — is readable on the network path. Gabkeep prints a startup
  warning and refuses to set `Secure` cookies in that mode (`auth.cookie_secure`).
* **The LLM sees everything.** Conversations, file excerpts you attach, tool
  output and search queries are sent to `llm.endpoint`; search queries and
  fetched URLs go to the configured search provider (Brave). Treat those
  endpoints/providers as trusted sinks.
* **Prompt injection (§9.13) is mitigated, not solved.** Web pages, search
  results and file contents are wrapped as *data, not instructions*, tool
  approval gates exist, and denials are honoured — but a model can still be
  manipulated into *asking* you to approve something. Keep `code_run: confirm`
  (the default) and never combine `code_run: auto` with open registration —
  gabkeep warns when you do.
* **Code execution is defence-in-depth, not a VM.** `container` mode uses
  docker (default-off network); `isolated` mode needs gabkeep to run as root
  with `run_as_user` to drop privileges — as non-root it degrades to
  rlimits/no-new-privs only (gabkeep logs the degradation loudly). Files a
  script produces can be shared back into the chat: assume anyone who can
  approve runs can read/write that user's scratchpad.
* **Uploads are for download, not browsing.** Uploaded HTML/SVG is stored but
  only ever served as `Content-Disposition: attachment` — yet once the user
  downloads and opens it locally, it runs with their local privileges.
  Extension blocklist + MIME sniffing (`http.DetectContentType`) reduce the
  worst cases; they are not antivirus.
* **Single process, single SQLite.** Rate limits and per-user caps blunt DoS,
  but one heavy user can degrade everyone; there is no horizontal scaling.

## Development

```sh
make test          # unit + integration (httptest, temp SQLite, fake LLM)
make race          # same with -race
make lint vuln     # gosec + govulncheck
make fuzz-short    # path-sandbox containment fuzzer
make dist          # stamped release binary + sha256 under dist/
```

`dev.yaml` is my local config (LAN bind + invite-only); copy it and point it
at your endpoint. The UI lives in `web/` and is embedded into the binary at
build time — rebuild and hard-refresh after editing assets.

### Layout

```text
cmd/gabkeep/        main: flags, config load, startup wiring, graceful shutdown, admin CLI
internal/config/    YAML schema, env expansion, strict validation, startup warnings
internal/auth/      argon2id, sessions, CSRF tokens, invite codes, login limiter
internal/store/     SQLite (modernc.org) + migrations; users/conversations/messages/files/tool_calls
internal/server/    routing, middleware, SSE chat, files API, maintenance sweeps
internal/llm/       OpenAI-compatible streaming client (tools + reasoning channels)
internal/agent/     context budgeting + turn grouping fed to the model
internal/tools/     registry + web_search/web_fetch/fs_*/files_share/code_run + SSRF egress guard
internal/scratch/   path sandbox (§9.7): openat chain, O_NOFOLLOW, containment fuzzer
internal/runner/    code execution: raw | isolated (rlimits, uid drop) | container (docker)
web/                embedded SPA: app.js, SSE client, DOM-only markdown renderer, CSS
```

# Gabkeep

[![CI](https://github.com/PhilAnderson1/Gabkeep/actions/workflows/ci.yml/badge.svg)](https://github.com/PhilAnderson1/Gabkeep/actions/workflows/ci.yml)

Self-hosted, single-binary chat assistant with a ChatGPT-style web UI in front
of any local OpenAI-compatible LLM endpoint (llama.cpp, Ollama, vLLM, …).

* **Single static Go binary** — no runtime dependencies, no build step for the
  UI (vanilla ES modules embedded via `go:embed`), pure-Go SQLite.
* **Accounts** — email + password (argon2id), sessions with an optional
  "Stay logged in" cookie, invite-only or closed registration, login lockout.
* **Streaming chat** — SSE token streaming, stop/regenerate, markdown answers,
  dark/light theme, responsive from 320 px phones to ultrawide.
* **Vision** — with `llm.vision: true`, images you attach to a message
  (png/jpeg/webp) travel to the endpoint inline, so a vision-capable model
  actually sees the picture; text-only endpoints keep the old manifest-only
  behaviour untouched.
* **Tools with an approval layer** — `web_search` (Brave) and SSRF-guarded
  `web_fetch`, per-conversation **scratchpad files** (`fs_list/fs_read/
  fs_write/fs_delete`, uploads from the browser, artifact download cards, a
  file manager with Uploads / Artifacts / Work tabs),
  and optional sandboxed **`code_run`** (bubblewrap jail — see
  [Code execution](#security)). Every
  tool call is audited and shown as a card in the UI; policy decides which
  ones ask for confirmation first.
* **Privacy** — everything lives on your machine: SQLite history, per-user
  scratchpad directories. Only what you ask the model to search/fetch ever
  leaves the box, and only to the providers you configure.

Status: **M0–M7(a) complete** (see [`spec.md`](spec.md) §13). Hardening and
audit artifacts stay with the operator as machine-local files (untracked
`local-only/docs/`). See [Security](#security) for residual risks.

## Requirements

* 64-bit Linux (amd64/arm64). Anything else that runs Go works too.
* The `bubblewrap` package (`apt install bubblewrap`) — only if you enable
  `code_execution`; gabkeep refuses to start with code execution on when a
  working bwrap is missing (spec §9.9).
* For `code_execution` under systemd: `Delegate=yes` on the unit (the shipped
  `packaging/systemd/gabkeep.service` has it, with `DelegateSubgroup=payload`) so each run
  can get a per-run cgroup resource meter — kernel ≥ 5.7 with cgroup v2. No
  meterable subtree ⇒ code execution refuses to start, never runs un-metered.
* An OpenAI-compatible endpoint for the model. Text is the baseline; if the
  endpoint serves a vision model, set `llm.vision: true` and attached images
  (png/jpeg/webp) are sent inline as `image_url` content parts.
* To build: Go ≥ 1.24 (Ubuntu's `apt install golang-go` is fine if recent
  enough; otherwise grab the latest from https://go.dev/dl/).

## Build

```sh
make build            # bin/gabkeep, statically linked, version/commit/date stamped
# or without make:
mkdir -p bin
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always) -X main.commit=$(git rev-parse --short HEAD) -X main.buildDate=$(date -u +%FT%TZ)" \
  -o bin/gabkeep ./cmd/gabkeep
```

## Quick install on a fresh Ubuntu box

```sh
# 1. Binary
sudo install -m 0755 bin/gabkeep /usr/local/bin/gabkeep

# 2. Service user + state dirs
sudo useradd --system --home-dir /var/lib/gabkeep --shell /usr/sbin/nologin gabkeep
sudo mkdir -p /etc/gabkeep /var/lib/gabkeep
sudo chown gabkeep:gabkeep /var/lib/gabkeep && sudo chmod 700 /var/lib/gabkeep

# 3. Config (template with explanations: configs/gabkeep.yaml)
sudo cp configs/gabkeep.yaml /etc/gabkeep/config.yaml
sudo nano /etc/gabkeep/config.yaml         # set llm.endpoint + model at minimum
sudo chmod 640 /etc/gabkeep/config.yaml && sudo chown root:gabkeep /etc/gabkeep/config.yaml

# 4. Secrets (optional; referenced by ${ENV_VAR} expansion)
printf 'GABKEEP_SEARCH_API_KEY=...\n' | sudo tee /etc/gabkeep/env
sudo chmod 640 /etc/gabkeep/env && sudo chown root:gabkeep /etc/gabkeep/env

# 5. systemd
sudo cp packaging/systemd/gabkeep.service /etc/systemd/system/
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

Let's Encrypt keys stay `0600 root:root` by certbot policy, and gabkeep runs
unprivileged. The shipped unit solves this without a second on-disk copy:
`ExecStartPre` (root) stages cert+key into the RAM-only `RuntimeDirectory`
`/run/gabkeep`, and a certbot *deploy hook* (`systemctl try-restart gabkeep`)
picks up renewals. That line ships **commented out** — the default install is
plain HTTP on loopback (private networks only); un-comment it, set
`YOUR.HOSTNAME`, and enable `server.tls` to serve HTTPS directly. If you
terminate TLS at a proxy instead, bind gabkeep to loopback and skip all of it.

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
* `scratchpad.root` — per-user `uploads/`, `artifacts/`, `work/`. Files live on
  disk under their real names and the path is their only identity — there is no
  file registry to keep in sync. `uploads/` is durable user data; `artifacts/`
  and `work/` expire on the mtime sweep (`artifact_ttl`, `work_ttl`), so losing
  them costs generated output, not originals. The assistant never writes into
  `uploads/`; it can delete files there only through `fs_delete`, which asks
  your confirmation every time — scripts in the `code_run` jail never can.

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

Artifacts for the current release (gosec, govulncheck, OWASP ZAP baseline)
and the §9.16 re-audit with evidence per line — false-positive justifications
included — are operator machine-local files under `local-only/docs/`, kept
out of the repository like the dev and scan configs they belong to. Any of
them is reproducible from the commands above.

### Residual risks (read this)

* **No 2FA in v1.** Password + session cookies only. Use closed/invite-only
  registration and strong passphrases; keep the box LAN-only or behind a proxy
  with mTLS/VPN if that matters to your threat model.
* **Plain HTTP is opt-in.** If you run without TLS, everything — passwords,
  cookies, history — is readable on the network path. Gabkeep prints a startup
  warning; session cookies drop their `Secure` flag automatically so login
  works over plain HTTP (they keep it whenever the request really arrives over
  HTTPS, including via a trusted reverse proxy).
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
* **Code execution is defence-in-depth, not a VM.** Every run is jailed by
  [bubblewrap](https://github.com/containers/bubblewrap): its own
  user/mount/PID/net namespaces, a root filesystem containing only a
  read-only `/usr` plus the user's own scratchpad at `/scratchpad` — root
  mounted READ-ONLY, with `work/` the single writable overlay, while
  `artifacts/` and `uploads/` are read-only overlays, so a script can never
  litter the root beside the three directories — the database, other
  users' files and server configs have no path inside the jail — with
  rlimits, a scrubbed environment and no-new-privs underneath, and no
  network unless you set `allow_network: true` (gabkeep warns when you do).
  Every run also gets its own cgroup v2 meter: total memory (no swap), task
  count and CPU are capped for the whole process tree, so a multiplying
  script cannot exhaust the host (spec §9.9; needs the unit's delegated
  subtree, refuse-not-degrade).
  `bwrap` is a hard requirement when code execution is on: missing or
  non-functional bwrap stops gabkeep at startup, never a silent downgrade.
  Files a script produces can be shared back into the chat: assume anyone
  who can approve runs can read/write that user's scratchpad.
* **Uploads are for download, not browsing.** Every download is served as
  `Content-Disposition: attachment` — uploaded bytes never render inside the
  app's origin. But once the user downloads and opens a file locally, it runs
  with their local privileges. The optional `scratchpad.block_extensions`
  gate keeps chosen types off the disk entirely; it is disk hygiene, not
  antivirus.
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

`local-only/dev.yaml` (kept out of the repo, like my other machine-specific
files under `local-only/`) is my local dev config: loopback `:8081`, its own
`local-only/data/`; run `gabkeep -config local-only/dev.yaml` from the repo
root. Copy `configs/gabkeep.yaml` and point it at your endpoint. The UI lives
in `web/` and is embedded into the binary at build time — rebuild and
hard-refresh after editing assets.

### Layout

```text
cmd/gabkeep/        main: flags, config load, startup wiring, graceful shutdown, admin CLI
internal/config/    YAML schema, env expansion, strict validation, startup warnings
internal/auth/      argon2id, sessions, CSRF tokens, invite codes, login limiter
internal/store/     SQLite (modernc.org) + migrations; users/conversations/messages/tool_calls (no files table — the disk is the file state)
internal/server/    routing, middleware, SSE chat, files API, maintenance sweeps
internal/llm/       OpenAI-compatible streaming client (tools + reasoning channels)
internal/agent/     context budgeting + turn grouping fed to the model
internal/tools/     registry + web_search/web_fetch/fs_*/files_share/code_run + SSRF egress guard
internal/scratch/   path sandbox (§9.7): openat chain, O_NOFOLLOW, containment fuzzer
internal/runner/    code execution: bubblewrap jail + rlimit launcher (§9.9)
web/                embedded SPA: app.js, SSE client, DOM-only markdown renderer, CSS
```

# Mullsmith guide

Mullsmith connects your browser to a model endpoint you already run or subscribe
to. It stores accounts, conversations and files on your server. It does not run
or download the language model itself.

Start with the installation instructions, then choose the deployment that fits
who will use your server.

- [Install Mullsmith](#install-mullsmith)
- [Models and thinking controls](#models-and-thinking-controls)
- [Choose your deployment](#choose-your-deployment)
- [Internet access and HTTPS](#internet-access-and-https)
- [Registration modes](#registration-modes)
- [Enable tools](#enable-tools)
- [Everyday administration](#everyday-administration)
- [Troubleshooting](#troubleshooting)
- [Building from source](#building-from-source)
- [Upgrade or remove](#upgrade-or-remove)

The [commented configuration](configs/config.yaml) is the full option reference.

**The YAML examples below are fragments:** edit the corresponding entries in your
existing configuration; do not replace the whole file or add duplicate sections.
Use two spaces per indentation level, never tabs. Restart after configuration changes.

## Install Mullsmith

### Debian and Ubuntu

Download the `.deb` for your machine from
[Releases](https://github.com/PhilAnderson1/Mullsmith/releases), then:

```sh
sudo apt install ./mullsmith_*.deb
```

### RPM-based distributions

Download the `.rpm` for your machine from
[Releases](https://github.com/PhilAnderson1/Mullsmith/releases), then install with DNF:

```sh
sudo dnf install ./mullsmith-*.rpm
```

### Tar archive

For a Linux tar archive, extract it, enter the extracted directory and run:

```sh
sudo ./install.sh
```

Native packages install the binary at `/usr/bin/mullsmith`; the tar installer
uses `/usr/local/bin/mullsmith`. Both use `/etc/mullsmith` for configuration and
`/var/lib/mullsmith` for application data.

### Create your first account

The installer enables and starts the Mullsmith service.
The supplied configuration serves HTTP on port 3210 on all IPv4/IPv6 interfaces,
with invite-only registration.

Create your first Mullsmith account, substituting your email, name and password:

```sh
sudo mullsmith admin create-user -email you@example.org -name 'Your name' -password 'password at least 10 characters'
```

Open `http://YOUR-SERVER-IP:3210` and log in to check the installation. Then edit
`/etc/mullsmith/config.yaml`, setting the endpoint and model details as described below.

## Models and thinking controls

Mullsmith works with a single configured OpenAI-compatible Chat Completions
endpoint, which can offer multiple models. Set model ID names exactly as your
endpoint advertises them.
These example URLs must match the actual IP addresses and ports you chose for your model servers.

| Server | Example `llm.endpoint` | `llm.endpoint_type` |
|---|---|---|
| Gufo | `http://127.0.0.1:8080/v1` | `gufo` |
| llama.cpp | `http://127.0.0.1:9931/v1` | `llama_cpp` |
| Ollama, OpenAI-compatible API | `http://127.0.0.1:11434/v1` | `ollama_openai` |
| vLLM | `http://127.0.0.1:8000/v1` | `vllm` |
| LM Studio | `http://127.0.0.1:1234/v1` | `lm_studio` |
| OpenRouter | `https://openrouter.ai/api/v1` | `openrouter` |

For a model on llama.cpp:

```yaml
llm:
  endpoint: "http://127.0.0.1:9931/v1"
  endpoint_type: "llama_cpp"
  # Uncomment and enter your API key if required.
  # api_key: ""
  models:
    - id: "your-model-id"
      default: true
      thinking_profile: "none"
      vision: false # Set to true if the model and endpoint support image input.
      # context_size: 32768 # Set to the endpoint's configured context window, or omit for discovery.
```

`thinking_profile` determines which thinking controls appear for a model, such as
on/off or low/medium/high. Choose the matching profile from
[thinking-profiles.yaml](configs/thinking-profiles.yaml)—for example, `"qwen3_8"`
for the Qwen3.8 family of models. Use `"none"` to hide thinking controls.

Set the model's context size and image support as appropriate:

- `context_size`: the context window actually configured on your endpoint,
  which may be smaller than the model's theoretical maximum. Omit it to use
  discovery if the endpoint reports it. Without either, the usage meter shows
  token counts without a known capacity.
- `vision: true`: enable image input only when the model and endpoint support it.

Add more entries under `llm.models` to offer more models, with one marked
`default: true`. Give each its own thinking profile, context size and image support.

If your endpoint or model's thinking options are not listed, you may be able to
add them by editing `endpoints.yaml` or `thinking-profiles.yaml` in `/etc/mullsmith`.

Restart Mullsmith after making changes.

```sh
sudo systemctl restart mullsmith
```

Log in to Mullsmith and start a new chat to test the endpoint after adding it to your configuration file.

### Check configuration and manage the service

Run the command for the operation you need:

```sh
# Validate configuration and test the endpoint connection.
sudo mullsmith -config /etc/mullsmith/config.yaml -check-config

# Show service status.
sudo systemctl status mullsmith

# Restart after configuration changes.
sudo systemctl restart mullsmith

# Stop the service.
sudo systemctl stop mullsmith

# Start the service.
sudo systemctl start mullsmith
```

`-check-config` validates settings, checks relevant paths and tests the connection
to the model endpoint using the supplied API key. It does not exercise every tool
or replace a real chat test. An endpoint outage can make this check fail even
when the YAML is correct.

If your configuration uses environment variables, load the same values for CLI
commands as for the service; see [secrets](#secrets). Certificate files staged in
`/run/mullsmith` exist only while the service is running.

## Choose your deployment

### Trust model

Mullsmith is designed to serve directly on your local network or the public
internet, with accounts restricted to people you trust not to deliberately abuse
the service.

Open registration is intended for networks where everyone who can reach Mullsmith
is trusted, such as a private home LAN. It is not recommended on shared or
untrusted networks, or when Mullsmith is accessible from the public internet.
In those environments, enable HTTPS and use invite-only or closed registration.

Authenticated users can consume model resources, store files and use enabled
tools. These capabilities expose more of the application than the login page
does. Access controls, quotas and sandboxing provide protection, but Mullsmith is
not intended as a public service for untrusted account holders. This is especially
important when code execution is enabled.

Trusted users can still make mistakes or run unsafe model-generated code, so
sandboxing and resource limits remain important.

### Personal computer

**Use this when:** only browsers on the server itself need access.

```yaml
server:
  listen: "127.0.0.1:3210"
  tls:
    enabled: false
```

Open `http://127.0.0.1:3210` and log in using the account created earlier.

Mullsmith will only be available on this machine.

### Trusted home network

**Use this when:** your household uses Mullsmith from computers, tablets or phones.

```yaml
server:
  listen: ":3210"
  host: "YOUR.HOSTNAME:3210"
  tls:
    enabled: false
auth:
  registration: "open"
```

Open registration lets trusted household users create their own accounts.
You can use `invite-only` or `closed` if you prefer to control account creation.

**Check:** another household device can open Mullsmith, create an account and sign in.

**Understand:** HTTP sends passwords and conversations without encryption.
Use it only where you trust the network. On shared, guest or otherwise untrusted
networks, use HTTPS and invite-only or closed registration. Saving the site as an
installable web app generally requires HTTPS.

### Public internet, private accounts

**Use this when:** you or people you invite need access away from home.

Configure HTTPS as described below, then use:

```yaml
server:
  listen: ":3210"
  trusted_proxies: []
  tls:
    enabled: true
auth:
  registration: "invite-only"
```

Use `closed` if you do not want further signups; you can still add new accounts
via the command line tool. The defaults limit connections, request rates, failed
login attempts and each user's file storage. Start with these defaults; see
[Limits](#limits) if you need to adjust them.

**Check:** from outside your home network, the HTTPS URL has a valid certificate
and login works.

## Internet access and HTTPS

Mullsmith can serve HTTPS directly without a reverse proxy. You need a hostname,
a certificate for that hostname, and two configuration changes.

### 1. Set up a hostname

If you already have a hostname pointing to this server, use it. Otherwise, create
one with a service such as [Dynu](https://www.dynu.com/), which offers free dynamic
DNS, or use your own domain.

If your public IP address changes, follow your dynamic DNS provider's instructions
to keep the hostname updated automatically.

The examples below use `YOUR.HOSTNAME`; replace it with your actual hostname.

### 2. Obtain or reuse an SSL/TLS certificate

If the machine already runs an HTTPS web server, you may already have a suitable
certificate. You can reuse it **if it covers the hostname you will use for
Mullsmith**. Keep its existing renewal arrangement and skip certificate issuance.
A certificate is not tied to a particular port.

Otherwise, use [Let's Encrypt](https://letsencrypt.org/getting-started/) to obtain
a free certificate through its client software, such as Certbot. On Debian/Ubuntu:

```sh
sudo apt update
sudo apt install certbot
sudo certbot certonly --standalone -d YOUR.HOSTNAME
```

Before running the last command, use port forwarding on your router to forward
TCP port **80** to port **80** on your machine's local IP address. Allow port 80
through the machine's firewall if enabled. No other program can be using port 80
while standalone Certbot runs. Keep the forwarding rule for certificate renewals.

If another web server uses port 80, use Certbot's web-server or webroot integration
instead. If inbound port 80 is unavailable, use DNS validation. See the
[Certbot guide](https://eff-certbot.readthedocs.io/en/stable/using.html) and
[Let's Encrypt validation methods](https://letsencrypt.org/docs/challenge-types/)
for these alternatives; for other distributions, follow the
[Certbot installation instructions](https://certbot.eff.org/instructions).

Certbot prints the certificate paths. They normally look like:

```text
/etc/letsencrypt/live/YOUR.HOSTNAME/fullchain.pem
/etc/letsencrypt/live/YOUR.HOSTNAME/privkey.pem
```

Use the actual paths printed, which may have a suffix such as `-0001`.

### 3. Configure the startup unit

The service needs its own readable copies of the certificate and key. Run:

```sh
sudo systemctl edit --full mullsmith
```

Uncomment the existing `ExecStartPre=...` line by removing its leading `#`, and
replace both occurrences of `YOUR.HOSTNAME` with your certificate directory name.
Save and close the editor.

At each service start, this command copies the certificate and key to
`/run/mullsmith`, where Mullsmith can read them. Certbot continues to manage
the originals.
If reusing certificates managed elsewhere, substitute those paths instead.

### 4. Enable HTTPS in Mullsmith

Edit these entries in `/etc/mullsmith/config.yaml`:

```yaml
server:
  listen: ":3210"
  tls:
    enabled: true
    cert_file: "/run/mullsmith/cert.pem"
    key_file: "/run/mullsmith/key.pem"
auth:
  registration: "invite-only"
```

Use `closed` instead if all accounts already exist. Then restart:

```sh
sudo systemctl restart mullsmith
```

For internet access, allow incoming TCP port **3210** to this machine, forwarding
it through your router if needed. Open `https://YOUR.HOSTNAME:3210` and check that
the browser accepts the certificate. Test from outside your home network too.
The listener now uses HTTPS rather than HTTP; update bookmarks accordingly.

### 5. Keep certificates renewed

Check that Certbot's scheduled renewal is enabled and test it:

```sh
sudo systemctl list-timers --all certbot.timer
sudo certbot renew --dry-run
```

If the Debian/Ubuntu package's timer is disabled, enable it with
`sudo systemctl enable --now certbot.timer`. Other installation methods may use
a different timer or cron job.

Mullsmith loads certificates at startup. To restart it after successful Certbot
renewals, create an executable deploy hook:

```sh
sudo install -d -m 755 /etc/letsencrypt/renewal-hooks/deploy
sudo tee /etc/letsencrypt/renewal-hooks/deploy/mullsmith.sh >/dev/null <<'HOOK'
#!/bin/sh
systemctl try-restart mullsmith
HOOK
sudo chmod 755 /etc/letsencrypt/renewal-hooks/deploy/mullsmith.sh
```

If you use another certificate manager, add the same restart command to its
successful-renewal hook. Keep any existing web-server renewal hooks in place.

### Use an existing reverse proxy

Let the proxy serve HTTPS and forward to a loopback HTTP listener:

```yaml
server:
  listen: "127.0.0.1:3210"
  tls:
    enabled: false
  trusted_proxies:
    - "127.0.0.1/32"
```

This example assumes the proxy connects from `127.0.0.1`. For other topologies,
trust only the proxy addresses you actually use and restrict direct backend
access. The proxy must overwrite forwarded client/protocol headers, including a
single correct `X-Forwarded-Proto` value. Disable response buffering for streaming
chat and allow sufficient time for long responses. Never trust arbitrary internet
clients as proxies.

## Registration modes

Set `auth.registration` according to who can reach your server:

| Mode | Behaviour | Suitable use |
|---|---|---|
| `open` | Anyone who can reach Mullsmith can sign up | Localhost or a private LAN where everyone with access is trusted |
| `invite-only` | Signup requires an invitation code | Shared networks or internet access, with invitations given only to trusted people |
| `closed` | Browser signup is disabled; existing accounts can still log in | Any deployment once accounts are created, or when accounts are created through the administrative CLI |

See [accounts and invitations](#accounts-and-invitations) to create accounts or
issue codes.

## Enable tools

The model chooses which offered tools to request. Tool results go back to the
model, so contents read from files or fetched pages may be sent to your configured
endpoint. A local model keeps that processing local; a remote provider receives it.

| Tool | Purpose and access | Supplied policy |
|---|---|---|
| `web_search` | Searches through Brave; requires a Brave Search API key | Disabled; automatic when enabled |
| `web_fetch` | Reads public web pages | Enabled; automatic |
| `web_download` | Downloads public URLs into the user's scratchpad | Enabled; automatic |
| `fs_list`, `fs_read` | Lists and reads the user's scratchpad files | Always available; automatic |
| `fs_write` | Writes within the user's work/artifact areas | Always available; automatic |
| `fs_delete` | Deletes permitted scratchpad files, including uploads | Always available; confirmation |
| `files_share` | Creates a downloadable artifact from a scratchpad file | Always available; automatic |
| `display_image` | Displays a PNG, JPEG or WebP artifact inline in the chat | Always available; automatic |
| `view_image` | Sends selected scratchpad images to a vision-capable model | Offered to configured vision models; automatic |
| `code_run` | Runs configured interpreters in the execution sandbox | Disabled; confirmation when enabled |

Web fetch and download block non-public network destinations. This restriction
does not apply to the configured model/SMTP endpoints, or code execution when you
explicitly allow its network access.

Enable search under `tools.web_search` with `enabled: true`, `provider: "brave"`
and your API key. No search key is required for direct web fetch/download.

### MCP tool servers

Mullsmith can expose tools from administrator-configured MCP servers using
Streamable HTTP or a local `stdio` process. MCP support is optional and no
server is enabled by default. Add a remote server to the top-level `mcp`
section:

```yaml
mcp:
  servers:
    - id: "deepwiki"
      name: "DeepWiki"
      transport: "streamable_http"
      url: "https://mcp.deepwiki.com/mcp"
      approval: "confirm"
```

Restart Mullsmith after changing the list. Tools from servers without an
`oauth` block appear to every Mullsmith account as names beginning
`mcp_<server-id>_`. `approval` may be `confirm` or `auto`; confirmation is the
default. Calls share the ordinary per-turn tool-call limit and are recorded in
conversation history like built-in tools.

The supplied configuration contains commented examples for DeepWiki, Weather
and Wikidata. Add only servers you intend to use: their tool descriptions are
sent with model requests, and tool arguments and results pass through the
external server. An administrator-configured HTTP URL may deliberately point
to a local service; MCP connections therefore do not use the public-address
guard applied to `web_fetch` and `web_download`. Remote MCP authentication is
optional. For a bearer token, add:

```yaml
      bearer_token: "${MCP_SERVICE_TOKEN}"
```

For a service using an API-key header instead, add:

```yaml
      headers:
        X-API-Key: "${MCP_SERVICE_API_KEY}"
```

Use environment variables without an empty fallback so a missing secret stops
startup rather than silently making unauthenticated requests. Mullsmith redacts
configured MCP credential values from its logs and diagnostics, refuses headers
that could interfere with HTTP or MCP routing, and never forwards credentials
across an origin-changing redirect. `-check-config` connects with the configured
credential, so an authentication failure is reported without exposing it.

These credentials define one system-wide external identity. Every Mullsmith
account offered that server's tools can act through the same identity; use
per-user OAuth instead when the external service must distinguish users.

For per-user OAuth, first register Mullsmith as an OAuth application with the
service provider. Give the provider this exact callback URL:

```text
https://mullsmith.example.org:3210/api/v1/mcp/oauth/callback
```

The provider supplies a client ID and, where required, a client secret. It also
documents its issuer URL. Generate Mullsmith's token-encryption key once:

```sh
openssl rand -base64 32
```

Then configure the public host, encryption key and integration:

```yaml
server:
  host: "mullsmith.example.org:3210"

mcp:
  enable_oauth: true
  token_encryption_key: "paste-the-generated-value-here"
  servers:
    - id: "service"
      name: "Service"
      transport: "streamable_http"
      url: "https://mcp.example.org/mcp"
      oauth:
        client_id: "provider-supplied-client-id"
        client_secret: "provider-supplied-client-secret"
        issuer: "https://login.example.org"
        scopes: ["provider-scope"]
        authorization_params:
          access_type: "offline"
      approval: "confirm"
```

Omit `client_secret` when the provider registers Mullsmith as a public client.
The optional `scopes` list limits the permissions requested initially. The
optional `authorization_params` map supplies provider-specific OAuth options;
Mullsmith rejects parameters that would override its callback, state or PKCE
protection.

For a server supporting OAuth dynamic client registration, omit `client_id`
and `client_secret` and set `dynamic_client_registration: true`. Mullsmith
registers separately for each user and stores the issued client credentials
with that user's encrypted tokens.
The callback scheme comes from `server.tls.enabled`; the port in `server.host`
overrides the listener port. Run `-check-config` to see the resulting callback
URL, and register that same URL with the provider.

After restart, each user opens **Settings → Connections** and selects
**Connect**. Mullsmith uses Authorization Code with PKCE and stores the resulting
tokens encrypted under that user's account. Only that user receives the
connected tools. Disconnect removes the stored tokens locally; changing or
losing `token_encryption_key` requires users to reconnect. Keep that key stable
and back it up with the database. OAuth discovery and token requests must use
public HTTPS endpoints and are protected by Mullsmith's private-network guard.

A local MCP server must already be installed by the administrator. Configure
its absolute executable path and arguments; Mullsmith invokes it directly and
never through a shell:

```yaml
mcp:
  state_dir: "/var/lib/mullsmith/mcp"
  servers:
    - id: "local_tools"
      name: "Local tools"
      transport: "stdio"
      command: "/usr/local/bin/local-mcp-server"
      args: ["--stdio"]
      environment:
        EXAMPLE_API_KEY: "${MCP_EXAMPLE_API_KEY:-}"
      approval: "confirm"
```

Each process receives a minimal environment and uses
`mcp.state_dir/<server-id>` for its working directory, home, caches and
persistent state. Only variables explicitly listed under `environment` are
added; Mullsmith's LLM, email and other credentials are not inherited. The
process runs as the `mullsmith` service account under the same systemd
restrictions as the application. With the supplied unit it can write beneath
`/var/lib/mullsmith`, its private temporary area and runtime directory, but can
read files elsewhere when normal Unix permissions allow and make network
connections available to the service; it does not use the web tools' public-
address guard. A server needing another writable directory requires that exact
directory in the unit's `ReadWritePaths`; do not disable
`ProtectSystem=strict`. All configured stdio servers share the service account
and should therefore be treated as trusted administrator-installed software.

### Approvals and execution limits

`tools.agent.approval` accepts `auto` (run immediately) or `confirm` (ask the user).
An approval may be remembered for the current conversation. Use an optional tool's
`enabled` setting to disable it; `confirm` is not a disable switch.

`tools.agent.tool_cards` accepts `useful` (the default) or `all`. `useful` hides
successful status-only cards while retaining approvals, failures, search results,
downloads, images and other structured results. Calls remain in conversation
history and are still supplied to the model; this setting changes presentation only.

Keep deletion and code execution on `confirm` unless you deliberately want them
to run unattended. Read the requested action: web pages and uploaded files can
contain instructions intended to manipulate the model, and approval does not
eliminate that risk.

A **round** is one batch of tool requests from a model response; it may contain
several **calls**. The supplied per-turn limits are 20 tool rounds and 100
proposed tool calls. They bound a single response workflow, not the lifetime of
a conversation. The code execution timeout applies to each run; one turn can
contain multiple sequential runs.

### Optional code execution

Install bubblewrap (for example, `sudo apt install bubblewrap`). Then enable:

```yaml
tools:
  code_run:
    enabled: true
    timeout: 30s
    max_memory: 1GiB
    max_output: 256KiB
    allow_network: false
  agent:
    approval:
      code_run: "confirm"
```

Mullsmith checks both the bubblewrap jail and cgroup delegation at
startup; if either cannot work, it refuses to start with code execution enabled.

Raspberry Pi systems may have the cgroup memory controller disabled by default.
This only needs changing when code execution is enabled. Check with:

```sh
cat /sys/fs/cgroup/cgroup.controllers
```

If `memory` is absent, append `cgroup_enable=memory` to the existing single line
in `/boot/firmware/cmdline.txt`, reboot, and run the check again. Mullsmith can
then enforce its aggregate memory limit on executed code.

Each run gets a fresh bubblewrap jail. Selected system files are read-only;
the user's `work/` and `artifacts/` directories are writable, and `uploads/` is
read-only. Other users' files, the database and server configuration are not
mounted into the jail. Mullsmith separately creates cgroups to limit memory,
CPU and task count across the process tree, and enforces the execution deadline.
The sandbox shares the host kernel; it is not a virtual machine.

It is recommended to keep network access for code execution turned off. Enabling
it lets all Mullsmith users run code that connects to local-network services and
internet destinations reachable from the server, subject to its firewall rules.
This could be used to probe internal services, send accessible files to external
servers or misuse your internet connection. The web tools' restrictions on
non-public addresses do not apply to executed code.

**Code execution is for trusted users.** Application storage accounting is not a
hard filesystem quota: code can consume disk space or inodes in ways ordinary
byte accounting does not fully capture. The default configuration is suitable
for general use with trusted users. For additional hardening, place the scratchpad
on a separate, size-limited filesystem, either on a dedicated partition or in a
fixed-size file mounted through a loop device. The default location is
`/var/lib/mullsmith/scratchpad`; mount it before starting the service and preserve
ownership by `mullsmith`. If it contains existing files, move them to the new
filesystem while the service is stopped rather than hiding them beneath a mount.

### Files and retention

Each user has three areas:

- `uploads/`: original uploaded files, retained until deleted.
- `work/`: temporary work, expired by the maintenance sweep after the configured
  retention period (24 hours by default).
- `artifacts/`: shared outputs, expired after seven days by default.

Retention is based on file modification time; cleanup happens on a later sweep,
not at the exact expiration instant. Download outputs you want to keep. Files are
served as downloads; opening downloaded content locally is outside the sandbox.

## Everyday administration

### Accounts and invitations

Existing accounts can log in whether registration is open, invite-only or closed.
Run administrative commands on the server, using the installed configuration:

```sh
# Create an account without browser registration.
sudo mullsmith admin create-user -email you@example.org -name 'Your name' -password 'password at least 10 characters'

# Generate three signup codes for invite-only registration.
sudo mullsmith admin invite 3

# List accounts, including IDs for unconfirmed signups.
sudo mullsmith admin list-users

# Send a password-reset email, or prompt for a password if email reset is unavailable.
sudo mullsmith admin reset-password you@example.org

# Disable an account and revoke its sessions.
sudo mullsmith admin disable-user you@example.org

# Re-enable a disabled account; this does not confirm a pending signup.
sudo mullsmith admin enable-user you@example.org

# Permanently delete an account and its scratchpad files; an account ID also works.
sudo mullsmith admin delete-user you@example.org
```

A completed password reset revokes existing sessions.

### Password reset & email confirmation

Email is optional and disabled by default. Enable it to offer password-reset
links, confirm new signups before login and confirm email-address changes.

```yaml
server:
  host: "mullsmith.example.org:3210"

email:
  enabled: true
  smtp:
    host: "smtp.example.org"
    port: 587
    authenticated_smtp: true
    username: "mullsmith@example.org"
    password: "${MULLSMITH_SMTP_PASSWORD}"
    from: "mullsmith@example.org"
```

`server.host` is the browser-visible hostname and optional port used in email
links; their HTTP/HTTPS scheme follows `server.tls.enabled`. Port 587 uses
STARTTLS; port 465 uses implicit TLS. For a trusted LAN relay that needs no
login, set `authenticated_smtp: false` and its actual host/port. Authenticated
SMTP requires TLS.

Put the SMTP secret in the service environment as below. Check logs and perform
a real test signup/reset. Mail is queued and retried; a successful HTTP response
does not prove delivery. Reset links expire after 30 minutes, signup confirmation
links after 24 hours; expired pending signups are cleaned up automatically.

### Secrets

Secrets may be written directly in the protected configuration file, or supplied
through `${ENV_VAR}` / `${ENV_VAR:-default}` references. The service reads optional
`/etc/mullsmith/env`, for example:

```sh
MULLSMITH_LLM_API_KEY='your-provider-key'
MULLSMITH_SEARCH_API_KEY='your-brave-key'
MULLSMITH_SMTP_PASSWORD='your-smtp-password'
```

Creating this file alone does not change the configuration: set the relevant
`api_key` or `password` entry to reference the variable. Protect the file:

```sh
sudo chown root:mullsmith /etc/mullsmith/env
sudo chmod 640 /etc/mullsmith/env
```

A CLI process does not automatically read systemd's environment file. If you keep
it in simple shell-compatible `NAME='value'` form, run a configuration check with:

```sh
sudo sh -c 'set -a; if [ -f /etc/mullsmith/env ]; then . /etc/mullsmith/env; fi; set +a; exec mullsmith -config /etc/mullsmith/config.yaml -check-config'
```

The same wrapper can run an `admin` command instead of `-check-config`. Restart
the service after changing secrets. Keep backups containing credentials private.

### Limits

Use `server:` for HTTP transport controls and `app_limits:` for application use.
The template supplies conservative defaults for a low-traffic server.

| What you want to constrain | Settings to review |
|---|---|
| Connections and stalled clients | `server.max_connections`, `concurrent_requests`, read/write timeouts |
| Concurrent model work | `app_limits.concurrent_streams_per_user`, `concurrent_streams_total` |
| Per-user request/message rates | `app_limits.requests_per_minute`, `chat_messages_per_hour` |
| Stored conversations/messages | `app_limits.max_conversations_per_user`, `max_messages_per_user` |
| File storage and entry count | `scratchpad.user_quota`, `max_entries` |
| Upload/download workload | Scratchpad upload size, web download size and `app_limits` transfer concurrency |
| Tool activity in a single turn | `tools.agent.max_tool_rounds`, `max_tool_calls_per_turn` |

Choose a total generation concurrency your model server can handle.

### Backups and storage locations

With the default paths, preserve `/etc/mullsmith` and `/var/lib/mullsmith`.
They contain configuration, secrets, the SQLite database and users' files.

The simplest consistent backup is to stop the service, copy both directories
while preserving ownership and permissions, then start it again. For online
database backups, use SQLite's backup facility rather than copying a live `.db`
file without its WAL. Files may change while a separate file backup runs.

To restore, stop the service, restore configuration and data with their original
permissions, then start and verify login and file access. Restore a binary
compatible with the saved schema. Store backups securely: they contain private
conversations and potentially credentials.

If you move `database.dir` or `scratchpad.root` outside `/var/lib/mullsmith`, grant
the service account ownership and add the new locations to a systemd drop-in:

```ini
[Service]
ReadWritePaths=/srv/mullsmith-db /srv/mullsmith-files
```

Retain `ProtectSystem=strict`; ownership alone does not override the service's
filesystem restrictions. Mounting a dedicated scratchpad filesystem under the
default `/var/lib/mullsmith` path avoids changing this allowlist.

## Troubleshooting

Start with:

```sh
sudo systemctl status mullsmith
sudo journalctl -u mullsmith -n 100 --no-pager
mullsmith -version
```

| Symptom | Check |
|---|---|
| Page unreachable | Service status, listener address, host firewall, routing and whether the URL needs HTTP or HTTPS. Check Mullsmith's logs for errors: `sudo journalctl -u mullsmith -n 100 --no-pager` |
| Page loads but chat fails | Endpoint address, API key and exact model ID; run `-check-config` with the service's environment |
| New configuration rejected | YAML indentation, duplicate/unknown keys and option values; compare with the supplied template |
| HTTPS fails after restart | Certificate hostname, expiry and readability; certificate staging command and file paths |
| Enabling code execution prevents startup | Bubblewrap, unprivileged namespaces, cgroup v2 and systemd delegation; inspect the logged probe failure |
| Thinking menu missing | Selected model's `thinking_profile`, catalogue paths and endpoint compatibility |
| Signup unavailable | Registration mode, invitation code, signup limits or email confirmation |
| Reset email missing | SMTP settings, service environment, delivery logs and junk folder |
| Upload/write rejected | File-size limit, byte quota, entry ceiling, blocked extensions, filesystem space and permissions |

`/healthz` indicates the HTTP service responds; it does not prove model generation,
SMTP delivery or every tool is working. Verify those features with real actions.
Use the original sandbox probe error to diagnose execution problems rather than
removing its isolation or running Mullsmith as root.

## Building from source

Install Git, Make and the Go version required by [go.mod](go.mod), then run:

```sh
git clone https://github.com/PhilAnderson1/Mullsmith.git
cd Mullsmith
make build
```

The resulting `bin/mullsmith` contains the web application. To install it as a
systemd service, run `sudo ./packaging/install.sh`, then follow
[account setup](#create-your-first-account) and [model configuration](#models-and-thinking-controls).

The [Makefile](Makefile) also provides test, race, browser and security-check
targets for development; these are not required to build or run Mullsmith.

## Upgrade or remove

Install the newer package with the same `apt` or `dnf` command. For a tar install,
stop the service and run the new archive's installer; it starts the service again.
Review configuration changes and check the logs after upgrading. The tar installer
preserves existing configuration files and updates its supplied systemd unit.
A unit customised with `systemctl edit --full mullsmith` remains under `/etc`
and continues to override the supplied unit; review new unit changes when upgrading.

Database upgrades apply ordered schema migrations automatically. Back up the
database before upgrading, and do not start an older Mullsmith binary against a
database that has already been opened by a newer release.

| Installation | Remove software | Data behaviour |
|---|---|---|
| Debian/Ubuntu | `sudo apt remove mullsmith` | Keeps configuration and user data |
| Debian/Ubuntu, complete removal | `sudo apt purge mullsmith` | **Deletes configuration, history, files and service account** at the packaged locations |
| RPM | `sudo dnf remove mullsmith` | Keeps generated state and service account; modified configuration may become `.rpmsave` files |
| Tar archive | `sudo ./uninstall.sh` from the archive | Asks for confirmation, then **deletes software, configuration and data** at the default locations; optionally removes the service account |

For complete removal of an RPM installation, including **all accounts, history,
files and configuration** at the default locations:

```sh
sudo systemctl disable --now mullsmith
sudo dnf remove mullsmith
sudo rm -rf /etc/mullsmith /var/lib/mullsmith /run/mullsmith
sudo rm -rf /etc/systemd/system/mullsmith.service /etc/systemd/system/mullsmith.service.d
sudo systemctl daemon-reload
sudo userdel mullsmith
if getent group mullsmith >/dev/null; then sudo groupdel mullsmith; fi
```

`userdel` may also remove the service group; the last command removes it if it remains.

Custom data locations require separate management. Do not install a native package
over a tar installation without resolving the old service unit: a unit in
`/etc/systemd/system` overrides the packaged unit.

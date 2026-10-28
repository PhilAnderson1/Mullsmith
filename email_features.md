# Plan: password reset + email confirmation (`email_features`)

Status: planning only — nothing implemented. Written against master `3e0be46`.

## 1. Scope

Two features behind one master switch, plus an SMTP layer both depend on:

1. **Password reset** — a "Forgot password?" link on the login page → enter email →
   receive a link → open a set-new-password page. Completing it revokes all sessions
   (same doctrine as `admin reset-password`).
2. **Email confirmation** — when enabled, new sign-ups store their address **only
   in `unconfirmed_email`** (the `email` column stays empty) with status `disabled`;
   clicking the emailed link copies the address into `email` and activates the
   account — literally the same copy operation as a **Settings address change**,
   which works identically (§5). Login and every credential-facing operation only
   ever see confirmed addresses. When the feature is off, sign-up writes the address
   straight into `email` and logs the user in, exactly as today.
   Admin-CLI-created users bypass the flow (address written to `email` directly,
   status active — otherwise you can lock yourself out of a fresh box with a broken
   SMTP).
3. **SMTP sending** — config-driven; `authenticated_smtp: true` uses username/password
   over a STARTTLS connection, `false` sends unauthenticated (relayer LAN box etc.).

Supporting pieces decided and folded in: pending sign-ups **auto-expire after 24 h**
(no dead-data build-up), and the admin CLI grows `list-users` / `delete-user` for
recovery. All open decisions are resolved — see §11.

Out of scope (deliberately): HTML mail (plain text only), account
deletion emails, rounded-corner branded email templates.

## 2. Configuration

New top-level `email:` section (template `configs/config.yaml`, strict-known-fields
means every key lives in the struct):

```yaml
email:
  enabled: false                    # the "email_features" master switch
  base_url: ""                      # absolute origin links are built from, e.g. https://host.example:3210
  smtp:
    host: ""                        # hostname or address
    port: 587
    authenticated_smtp: true        # true → AUTH LOGIN with username/password, STARTTLS required
    username: ""
    password: "${GABKEEP_SMTP_PASSWORD:-}"   # env expansion works (values only, §9.12)
    from: "gabkeep@localhost"       # envelope + From header
```

Upgrade-compatibility rule (normative):

- **Enabling the feature later never touches existing accounts.** Flipping
  `enabled: false → true` must not disable, flag, or "back-fill confirmation" on any
  pre-existing user: they were created active and stay active forever, no retroactive
  confirmation demand. Only sign-ups registered *while* the feature is enabled start
  in the pending (`disabled`) state. Migration 0008 therefore contains no `UPDATE`
  on `users`, and no startup sweep may reconcile old rows into a pending state.
- **Feature off ⇒ every new account is automatically confirmed.** With
  `enabled: false` (the default), `handleRegister` keeps today's exact behaviour:
  status `active`, session cookie issued, user lands straight in chat. "Confirmed"
  is simply what active means when mail isn't in play — there is no separate
  confirmed flag to set.
- A user whose address was already confirmed stays confirmed when the feature is
  switched on later. They only enter the confirm flow for an address by *changing*
  it in Settings (§5, `unconfirmed_email`) — enabling the feature never puts an
  existing, confirmed address into a pending state.

Notes:
- `base_url` is required when `enabled: true`. We removed `public_url` long ago;
  deriving the origin from the `Host` header would let a spoofed header inject an
  attacker domain into confirmation/reset **links** — so mail links only ever come
  from operator config. Validation: must parse as `http(s)://host[:port]` with no path.
- Secrets via `${VAR}` expansion — free, the loader already does it for values.
- Validation rules (`Config.Validate`, mirroring the `tools.web_search` pattern at
  `internal/config/config.go:693`):
  - `enabled: true` → `smtp.host` non-empty, port 1–65535, `from` contains `@`,
    `base_url` valid; `authenticated_smtp: true` → `username` non-empty.
  - `authenticated_smtp: true` with `port: 25` → validation **error** (we refuse
    credentialed cleartext; 587/465/other are the supported auth ports).
- `-check-config` (`Config.CheckIO`, `internal/config/config.go:796`): when enabled,
  dial the server and report the result — **warn, never fail** (decided): a transient
  SMTP blip must not block the standard deploy pattern (`-check-config` gates every
  restart). Prints `[ok] email.smtp <host:port> auth=true` when reachable, `[warn]`
  with the dial error otherwise; the check never counts toward the exit code. It
  checks reachability + TLS capability only — no message is sent, no credentials
  logged, and note that a dial cannot prove the username/password work anyway.
- Extend `admin reset-password <email>`: when `email.enabled` and SMTP is reachable,
  also email the user a reset link instead of printing a generated password
  (fallback: current behaviour, unchanged, on any mail error). It matches on the
  confirmed `email` column, so like login it never sees a NULL-email pending sign-up
  — consistent, and the operator has no way to name an address the account hasn't
  proven it owns.
- New commands (decided §11.3): `admin list-users` prints
  `id  email|<pending: unconfirmed_email>  status  created_at` for every row — the
  only way to see/ name pending sign-ups at all; `admin delete-user <id-or-email>`
  removes the row (id addresses pending users; email matches the confirmed column),
  `auth_tokens` cascade via FK, scratchpad bytes fall to the existing orphan sweep.
  Refuse nothing on status grounds — the operator is trusted (same standing as
  `create-user`).

## 3. Store

New migration `internal/store/migrations/0008_email_tokens.sql` + a small
migration-runner change (both proven experimentally against this app's exact
DSN, modernc sqlite v1.61):

> **Why the runner change is mandatory.** The app opens SQLite with
> `_pragma=foreign_keys(1)` and `migrate()` executes each migration body inside
> a transaction (`internal/store/store.go:80`). In SQLite, `PRAGMA foreign_keys`
> is a **no-op inside a transaction**, and with enforcement on, `DROP TABLE` on
> a parent fires the children's `ON DELETE` actions — so the intuitive
> "create-new/copy/drop/rename" migration for `users` **silently deletes every
> session** (verified: exec succeeds, error is nil, sessions go to 0 — under
> CLI sqlite 3.46 and modernc alike). Reorder tricks don't help: RENAME drags
> child `REFERENCES` along even with `legacy_alter_table=ON`, and
> `defer_foreign_keys=ON` does not defer the DROP-time implicit cascade either.
> The only safe recipe is the official one — enforcement off **outside** the
> transaction — which needs a pinned connection, so it can't live in a `.sql`
> file.
>
> **Runner change (~10 lines, `migrate()`)**: if a migration body carries the
> marker comment `-- +no-foreign-keys`, pin one connection
> (`sqlDB.Conn(ctx)`), `PRAGMA foreign_keys=OFF` on it, run the tx on that same
> conn, `PRAGMA foreign_keys=ON`, release. Every other migration runs exactly
> as today. Post-rebuild the FK text in `sessions` still resolves (proven:
> ghost-user session insert rejected, legit one accepted).

```sql
-- 0008_email_tokens.sql  (-- +no-foreign-keys)
-- users.email becomes nullable (never-confirmed sign-ups hold their only
-- address in unconfirmed_email until the link is clicked). A '' placeholder
-- is not an option: inline UNIQUE collides on the second pending sign-up
-- (NULLs are distinct under UNIQUE; '' is not) — hence the rebuild.

CREATE TABLE users_new (
  id                    TEXT PRIMARY KEY,
  email                 TEXT UNIQUE COLLATE NOCASE,  -- NULL until confirmed
  name                  TEXT NOT NULL,
  password_hash         TEXT NOT NULL,
  status                TEXT NOT NULL DEFAULT 'active',
  default_system_prompt TEXT NOT NULL DEFAULT '',
  unconfirmed_email     TEXT,                        -- NULL = nothing pending
  created_at            INTEGER NOT NULL,
  updated_at            INTEGER NOT NULL
);
INSERT INTO users_new (id, email, name, password_hash, status,
                       default_system_prompt, created_at, updated_at)
  SELECT id, email, name, password_hash, status,
         default_system_prompt, created_at, updated_at FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;

CREATE TABLE auth_tokens (
  kind       TEXT NOT NULL,             -- 'confirm_email' | 'reset_password'
  token_hash BLOB NOT NULL,             -- sha256 of raw URL token; raw never stored
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  email      TEXT NOT NULL,             -- address the mail went to (confirm tokens bind to it)
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER,
  PRIMARY KEY (kind, token_hash)
);
CREATE INDEX idx_authtokens_expiry ON auth_tokens(expires_at);
```

Migration test (mandatory, per the probe findings): seed a DB through 0007 with
users **and sessions**, apply 0008, assert sessions all survive, `users` data
and `default_system_prompt` intact, duplicate confirmed emails still rejected
NOCASE, two NULL-email rows coexist, and the `auth_tokens` FK rejects a
ghost-user token row.

**Column semantics (the invariant this whole feature rests on):**

- `users.email` — NULL, or an address **whose ownership was proven** (via a
  confirmation click) or supplied by the trusted operator (admin CLI). Never
  empty-string; NULL means "no confirmed address yet".
- `users.unconfirmed_email` — NULL ("nothing pending"), or the address awaiting a
  confirmation click. Applies uniformly to sign-ups and Settings changes.
- `status` — `disabled` + NULL email is precisely "pending sign-up"; `active` +
  NULL email must never exist and no code path creates it.
- **Credential-facing reads** (login, forgot-password lookup) query `email` only —
  a pending sign-up is invisible to them by construction, no special-case checks.
- **Availability at sign-up** is checked against **both** columns (`WHERE email = ?
  OR unconfirmed_email = ?`): otherwise a typo'd pending sign-up's address could be
  claimed by a second registration, stranding both. A pending duplicate therefore
  gets today's 409 `duplicate_email`. Two Settings pendings may collide (unique
  index never sees them) — final arbitration stays at confirm time (rule below).

Design points:
- **Address binding**: a `confirm_email` token authenticates *control of the mailbox
  it was emailed to*, so the address must be fixed at mint time — `auth_tokens`
  gains `email TEXT NOT NULL` (the address the mail went to; plaintext is fine, it
  isn't a secret). Confirmation is applied **only** when the user's pending target
  still equals the token's email — see the rules below. This is what makes an
  already-delivered link unable to confirm a *different* address the account moved
  to in the meantime.
- Reissue semantics: minting a new token of a kind for a user **invalidates prior
  unconsumed tokens of that kind** (`DELETE ... WHERE kind=? AND user_id=? AND used_at IS NULL`
  in the same tx as the insert). One live link per purpose per user; a clicked-old
  link then simply 400s.
- `users.status` stays `active | disabled` — pending sign-ups use `disabled` *and*
  a NULL `email`, and the existing middleware rejection of non-active users
  (`internal/server/auth_middleware.go`, login check at
  `internal/server/api_auth.go:267`) already covers "can't log in until confirmed"
  with zero new logic (NULL-email users are additionally unreachable via
  `UserByEmail`).

New `internal/store` methods (in `users.go` / new `tokens.go`):

- `CreateToken(ctx, kind, userID string, expiresAt time.Time) (raw string, err)` —
  generates via `auth.NewSessionToken` (32-byte urlsafe, already exists in
  `internal/auth/tokens.go`), stores only the hash.
- `ConsumeToken(ctx, kind, raw string, now time.Time) (userID string, found bool, err)` —
  single transaction: look up by `(kind, sha256(raw))`, require `used_at IS NULL AND
  expires_at > now`, mark used, return user id. Unknown/expired/used all collapse to
  `found=false` (handlers show one generic message).
- `DeleteUnusedTokens(ctx, kind, userID)` — folded into `CreateToken` per above.
- `CompletePasswordReset(ctx, userID, hash, now)` — one transaction over
  `SetPassword` + `SetUserStatus('active')` + `DeleteSessionsForUser(_, id, nil)`,
  so a reset can never be half-applied (the three statements `handleChangePassword`
  scatters across calls today is fine for its flow; this one must be atomic).
- **Confirmation semantics** (`ConfirmEmail(ctx, raw, now)`, one transaction) —
  **one rule for both flows**, exactly the copy operation, regardless of whether the
  token came from a sign-up or a Settings change:
  1. *Valid* — token's email equals `users.unconfirmed_email` (non-NULL) ⇒
     `UPDATE users SET email = unconfirmed_email, unconfirmed_email = NULL,
     status = 'active'` — the unique index re-read at this instant is the final
     availability arbiter; if another account claimed the address meanwhile, the
     pending value is cleared and `ErrEmailTakenNow` is returned (handler shows a
     plain "someone already uses that address"; nothing else about the account
     changes). Setting `status='active'` in the same statement is harmless for
     already-active accounts (Settings change) and is what activates sign-ups —
     no separate activation path exists anywhere.
  2. *Stale or unknown* — token absent/expired/used, or its email no longer matches
     the pending target (user changed address again, or cancelled) ⇒ generic
     `found=false`; one message for every flavour of dead link.
- New store methods: `SetUnconfirmedEmail(ctx, userID, email, now)` (NULL clears —
  also the cancel path; used by sign-up too, which inserts the row with `email =
  NULL, unconfirmed_email = <address>`); `EmailAvailable(ctx, email)` checking
  `email = ? OR unconfirmed_email = ?` for the sign-up pre-flight (store.Register's
  unique insert still arbitrates `email`; the pending-column check closes the other
  half). `UserByEmail` stays exactly as is — NULL-email rows simply never match.
- **Maintenance sweeps** (hourly tick, `internal/server/maintenance.go`), two
  additions next to session cleanup:
  1. `DELETE FROM auth_tokens WHERE expires_at < now`.
  2. **Pending sign-up expiry (decided)**:
     `DELETE FROM users WHERE status='disabled' AND email IS NULL AND created_at <
     now - 24h`. The `disabled + NULL email` predicate is exactly "never-confirmed
     sign-up" (per the invariant, no other row matches — admin-disabled accounts
     have confirmed emails and are never touched). `auth_tokens` rows cascade via
     FK; the abandoned scratchpad directory is reclaimed by the existing unknown-user
     sweep (§8 doctrine, no new code). This also ages out the "zombie" rows created
     by turning the feature off (§11) — dead data cannot build up. No mail on
     expiry (the address was never proven deliverable); a log line suffices. The
     24 h window runs from `created_at`; resends do **not** extend it. Confirmation
     token TTL is set equal (24 h) so a click near the boundary just races the sweep
     and gets the generic invalid-link message.

## 4. `internal/mail` — SMTP client + async outbox

Go's stdlib `net/smtp` can't do AUTH-over-STARTTLS cleanly, and we don't want a new
dependency for this. Hand-roll a minimal client (~150 lines) on `net` + `bufio`:

- `Dial` → `EHLO` → if server offers STARTTLS: upgrade (tls.Client, ServerName =
  smtp host) → `EHLO` again → if `authenticated_smtp`: `AUTH LOGIN` (base64 user/pass;
  PLAIN variant is the same exchange) → `MAIL FROM:<from>` → `RCPT TO` → `DATA`
  (dot-stuffed, headers `From`, `To`, `Subject`, `Date`, `Message-ID`) → `QUIT`.
- TLS policy: `465` (or future `implicit_tls: true` key if ever needed) → connect
  through TLS from the start; everything else requires STARTTLS *offered* before
  `AUTH` — if not offered while `authenticated_smtp: true`, fail the send loudly
  (never send credentials in the clear). Unauthenticated sends over port 25 without
  STARTTLS are allowed (LAN relay use case) but logged as a warning.
- Timeouts on dial/read/write (e.g. 10s/30s); never block an HTTP handler.

**Outbox** (send is async — SMTP hiccups must not turn "request reset link" into a
500 or a 30-second hang):

- `type Outbox struct { ch chan job }`, buffered ~64, one worker goroutine started
  from `server.New` when `email.enabled`; `TryEnqueue` drops with a warn-log when
  full (user still sees the generic "if that address exists, a mail is on its way").
- Retries: 3 attempts per job, 10s/60s backoff, then drop + error-log. A separate
  durable queue (DB-backed) is overkill for a self-hosted tool; mail that can't be
  delivered after 3 tries is an operator-log problem.
- Public surface: `mail.NewSender(cfg)`, `Outbox.Send(to, subject, body string)`;
  message builders `ConfirmationMail(baseURL, link)` (states the 24 h deadline —
  "confirm within 24 hours or the account is deleted; you can sign up again"),
  `ResetMail(baseURL, link)` and `AddressChangedMail(toOld)` — plain text, link on
  its own line.

## 5. Tokens & handlers (`internal/server/api_email.go`)

Four public endpoints in `internal/server/api_email.go` (plus the authenticated
cancel-email route in §5), all **public** ones with no session, POST + JSON like the
rest of the API (`DisallowUnknownFields` via `readJSON`), and gated on
`s.cfg.Email.Enabled` (routes registered always, handlers 404 when disabled —
mirrors the registration-mode pattern; keeps the JS one code path).

Token lifetime: **30 min** for reset, **24 h** for confirmation — deliberately
equal to the pending sign-up expiry window (§3): the account won't outlive the link,
and a Settings-change link older than a day is stale anyway. One-shot consumption
per §3.

| Route | Body | Behaviour |
|---|---|---|
| `POST /api/v1/auth/forgot-password` | `{email}` | Rate-limit (per-IP 5/15min **and** per-email 3/15min). Look up user by confirmed `email`; if found **and status active**, mint `reset_password` token + queue mail. Unknown email, disabled user (pending sign-ups have NULL `email` and can't match at all), or mail-queue-full → identical 200 `{"ok":true}`. No enumeration (same doctrine as login §9.1: equal-cost where practical — deliberately *no* dummy Argon2 here since no password is verified; a 200-everywhere response is the oracle-killer). |
| `POST /api/v1/auth/reset-password` | `{token, password}` | Per-IP limit 10/15min. `ConsumeToken` → not found/expired ⇒ 400 `invalid_token` "link is invalid or expired". Policy-check `password` (`auth.CheckPasswordPolicy`), hash through `s.hashRun` gate, `CompletePasswordReset` (set hash, `status='active'` — see interplay below, revoke **all** sessions). Respond 200; user then logs in normally (don't auto-session — keeps this handler stateless and cookie-free). |
| `POST /api/v1/auth/confirm-email` | `{token}` | Per-IP limit 10/15min. `ConfirmEmail` (§3) — the one copy rule serves both flows (sign-up activation and Settings change are the same operation); stale/unknown/expired links all return the generic 400 `invalid_token`. Respond 200 `{ok}`. |
| `POST /api/v1/auth/resend-confirm` | `{email}` | Per-IP 5/hour **and** per-email 3/hour. If a `disabled` user holds that address in `unconfirmed_email`: reissue the token (invalidating the old one per §3) and re-queue the mail. Anything else (confirmed address, unknown address) → the identical generic 200 — no enumeration oracle. This is the escape hatch for never-confirmed sign-ups (they are invisible to forgot-password; see below). |

Registration interplay (`handleRegister`, `internal/server/api_auth.go:99`):

- When `email.enabled` is **false**: unchanged from today — user created `active`
  (automatically confirmed), session cookie set, `201` with the user payload, SPA
  enters chat.
- When `email.enabled` is **true**: create the user with `Status: "disabled"` and
  **`email = NULL`, the submitted address in `unconfirmed_email`**; the availability
  pre-flight checks **both columns** (`EmailAvailable`, §3 — a taken address either
  way returns today's 409 `duplicate_email`). Mint a `confirm_email` token **bound
  to the pending address** and queue the mail **after** the successful
  `store.Register` + scratchpad provisioning; **do not** create a session or return
  201-with-cookie. Respond `201 {"email": <submitted>, "email_confirmation_required":
  true}` (echoed from the request, for the SPA's "check your inbox" screen — the DB
  row itself has no confirmed address yet). Invites still burn at registration time
  (a never-confirmed sign-up consumes its code — document it).
- **Never-confirmed sign-ups cannot reset their password** — and that is coherent,
  not a gap: with `email` NULL they have no credential-facing identity to reset
  (forgot-password's `UserByEmail` lookup simply misses them → generic 200), and a
  mailbox they haven't proven they control is not something to mail password-reset
  links to. The self-service escape is `resend-confirm` (the address came from their
  own registration form moments ago, so re-mailing it there leaks nothing new).
- **Turning the feature off with pending sign-ups = accepted zombies (decided).**
  All email endpoints 404 while `email.enabled` is false, so pending accounts can't
  confirm until the operator re-enables — and if they never do, the 24 h pending
  expiry sweep (§3) deletes them. No special gating logic beyond the plain
  "disabled ⇒ endpoints off" rule.
- **Re-confirmation interplay**: an account that's active but has a pending address
  change can use forgot-password (lookup hits the confirmed `email`, and any
  `confirm_email` tokens are *not* revoked by a reset — they belong to a different
  purpose and the token-email binding already defangs stale ones). The reset mail
  goes to the confirmed address only.

Settings email change (`handleMeUpdate`, `internal/server/api_auth.go:332`):

- Feature **off**: `email` updates directly, exactly today's behaviour — and the
  write also NULLs `unconfirmed_email` (if the feature ran earlier and left a
  pending target, switching it off must not strand a stale value that a future
  re-enable would resurrect).
- Feature **on**: a changed `email` never writes the `email` column. The handler
  syntax-validates it, rate-limits (`confirm:` + IP, 3/15min — same oracle discipline
  as forgot-password), calls `SetUnconfirmedEmail` + mints a `confirm_email` token
  **bound to the new address** + queues the confirmation mail to it. The response
  (and later `GET /auth/me`) reports the change as pending. Unchanged email, or
  PATCHes that don't touch email, behave exactly as today.
- **Notify the old address**: queue `AddressChangedMail` to the *current* (confirmed)
  address — it is confirmed by definition, this is a session-authenticated action.
  A stolen session cookie therefore can't quietly reroute the account: the old
  mailbox gets the warning, and even if the attacker confirms first, the reset flow
  keeps working against whichever address is live.
- **Cancel a pending change**: authenticated `POST /api/v1/auth/me/cancel-email` —
  clears `unconfirmed_email` and deletes that user's unconsumed `confirm_email`
  tokens. (Explicit call, not silently assumed: re-requesting via a second PATCH
  already re-points the pending target and invalidates the old token per §3's
  reissue rule, so cancel is the only missing self-service escape.)
- **No sign-up interplay possible here**: Settings is behind an authenticated
  session, which a pending sign-up (NULL `email`, `disabled`) can never obtain, so
  the only way to be mid-`unconfirmed_email` on this page is a genuine address change
  on an already-active account. Confirming it keeps `status='active'` (no-op) and
  just moves the address. Login always uses the confirmed `email`, so a pending
  change can never lock someone out mid-edit.

`handleMeta` (`internal/server/server.go:211`) gains
`"email_features": s.cfg.Email.Enabled` — the SPA renders the forgot link and
confirmation copy from it, exactly like `remember_login` / `registration` do today.
`mePayload` gains `"unconfirmed_email"` (empty string when none).

Server wiring (`Server` struct + `New`): one `mailer *mail.Outbox` field, nil when
disabled; handlers check `s.cfg.Email.Enabled` first so a nil mailer is unreachable.

## 6. SPA (`web/index.html`, `web/assets/app/app.js`, `api.js`)

Follow the existing auth-view pattern — same `view-auth` card, sibling forms toggled
by hash route (`#login` / `#register` today, `show()` calls at app.js ~148):

- `web/index.html`: add inside `#form-login` a `switch-line` `#login-forgot`
  ("Forgot password? `#forgot` link"); new forms `#form-forgot` (email input +
  submit → static "if an account exists for that address, a reset link is on its
  way"), `#form-reset` (new password + confirm, autocomplete `new-password`),
  `#form-confirm` (auto-submits once on mount, shows ok/invalid), and
  `#register-confirm-note` (the "check your email" panel, with a **Resend** button
  wired to `resend-confirm`, prefilled with the just-registered address; copy states
  the 24 h confirm-or-lose deadline).
- `app.js` auth router: recognize `#forgot`, `#reset` (token required), `#confirm`
  (token required); token arrives as `https://host/#reset?t=<token>` — read via
  `new URLSearchParams(location.hash.slice(...))`. After consuming a confirm/reset
  hash, rewrite `location.hash` so tokens don't linger in the URL/history (the
  existing `location.hash = ""` cleanup at app.js:118 already does this style).
  Reset success → swap to login form with a success note; confirm success → swap to
  login with "email confirmed, sign in".
- Register flow: on `201` with `email_confirmation_required`, route to the
  check-inbox panel instead of `enterChat()`.
- Settings view: when `me.unconfirmed_email` is non-empty, show a notice under the
  email field ("awaiting confirmation: x@y — Cancel") wired to the cancel endpoint;
  a successful email PATCH swaps the field to that notice + "check your inbox"
  toast. Field keeps showing the current confirmed address as its value.
- `api.js`: `forgotPassword(email)`, `resetPassword(token, password)`,
  `confirmEmail(token)`, `resendConfirm(email)`; reuse the existing error-render
  path so API error codes surface in the form's `form-error` `<p>` like
  login/register do.
- Hide/show discipline: `#login-forgot` only visible when `meta.email_features`
  (same `show()` pattern as `#login-remember-row`).

## 7. Security checklist (house doctrine — each item maps to an audit finding style)

- **No enumeration**: forgot-password is 200-everywhere; confirm/reset with a wrong
  token is one generic 400 regardless of reason.
- **Tokens**: 256-bit from the existing CSPRNG path; stored sha256-only (same as
  sessions); one-shot; short TTL; reissue invalidates siblings; rate-limited per IP
  *and* per email; links are HTTPS-borne (operator `base_url`).
- **Credentials**: SMTP password only ever from expanded env/config; never logged —
  keep it out of `slog` args, and the client must fail-closed on STARTTLS-absent
  before `AUTH`.
- **Reset revokes all sessions** (`DeleteSessionsForUser(.., nil)`), matching the
  §9.1 doctrine of `handleChangePassword` and the admin CLI.
- **Disabled until confirmed** is enforced by the *existing* status check at login
  and in auth middleware — no new gate to get wrong.
- **No new dependency**, so nothing to add to `THIRD_PARTY_LICENSES`; Dependabot
  unaffected.
- Mail body contains no secrets beyond the token itself; token in a URL is
  unavoidable for this UX — mitigate with 30-min TTL + one-shot (the SPA scrubs it
  from the address bar after use).

## 8. Tests (conventions: table tests in-package, static appjs greps, 5s-poll async asserts)

- `internal/mail`: fake SMTP server over `net.Listener` (in-test, plaintext + a
  TLS/STARTTLS variant with a self-signed cert) asserting: auth-requires-STARTTLS
  refusal, successful auth send, unauth send, retry-then-drop, dot-stuffing of the
  body.
- `internal/store`: token create/consume/expired/reused/reissue-invalidates sweep;
  plus a compatibility test — apply 0008 to a DB with existing active users and
  assert the migration changes no `users.status` (the upgrade rule from §2).
  Pending-expiry sweep: `disabled + NULL email` older than 24 h is deleted (with
  its `auth_tokens` cascaded); younger pending rows, active users, and
  admin-disabled rows (confirmed email) survive.
- `internal/config`: validation matrix for `email:` (enabled-without-host fails,
  auth-on-25 fails, `base_url` path rejected; disabled section needs nothing else).
- `internal/server` (extend `server_test.go` harness): forgot returns identical
  200 for unknown email vs existing user (byte-compare envelopes); reset happy path
  sets hash + revokes sessions + old session cookie then 401s; confirm flips status
  and login then succeeds; disabled-when-config-off (404); rate-limit responses.
  Sign-up with feature on: row lands `disabled` + NULL `email` + address in
  `unconfirmed_email`, no session cookie; second sign-up against that pending
  address → 409 (`EmailAvailable` covers both columns); forgot-password for the
  pending address → generic 200 and **no mail queued**; `resend-confirm` reissues
  (old token dies) for a pending address and is the same generic 200 for anything
  else. Email-change flows: PATCH(email) with feature on writes only
  `unconfirmed_email` (login still works on the old address); confirm swaps +
  clears pending; confirm-after-second-change is the generic 400 (token-email
  binding); taken-in-the-meantime clears pending and reports it; cancel endpoint
  clears pending + tokens; PATCH(email) with feature off writes `email` directly.
- `appjs_test.go`: static greps for the new IDs/handlers (note: those tests grep raw
  source — add the expected `#form-forgot`, `#reset`, `#confirm` strings there).
- `cmd/gabkeep`: `list-users` output covers pending rows (`<pending: …>` column);
  `delete-user` by id (pending) and by confirmed email; unknown target → non-zero
  exit, nothing deleted.

## 9. Docs to update in the same commit(s)

- `configs/config.yaml`: the `email:` block with comments (comments are verbatim —
  the expandEnv fix makes `${...}` inside them safe).
- `README.md`: quickstart mention + a short "Password reset & email confirmation"
  section; note SMTP relayer example for `authenticated_smtp: false`.
- `spec.md`: API table (§5) for the five new routes (forgot / reset / confirm /
  resend-confirm / cancel-email); §9 security note; config docs
  row; `-check-config` probe list.
- `packaging/build-release.sh` PACKAGED_INSTALL heredoc: mention the new section for
  package installs.
- `internal/config` header comment if it lists sections.

## 10. Delivery order (each step independently commitable + CI-green)

1. **config + validation + template + check-config probe** (no behaviour change;
   `email.enabled` defaults false).
2. **migration 0008 (`+no-foreign-keys` marker + rebuild + `auth_tokens`),
   runner marker support, store token methods, maintenance sweep** + store tests
   incl. the sessions-survive rehearsal (seed through 0007).
3. **`internal/mail` package** (client + outbox + builders) + fake-server tests.
4. **Server: meta flag, four public endpoints + cancel route, register interplay,
   wiring** + API tests.
5. **SPA: forms, router, api.js** + appjs static tests.
6. **Admin CLI: `list-users` + `delete-user`** (§11.3) + the
   `admin reset-password` mail enhancement from §2.
7. **Docs pass** (§9).

Run full `make check` (PATH including `~/go/bin`) before every push per house rule;
gosec will eye the SMTP password handling and the hand-rolled net code — annotate
with `#nosec` + justification only where truly warranted, same as cookies.go.

## 11. Decided (review complete — no open questions remain)

1. **Feature-off zombies: accepted, bounded by auto-expiry.** Email endpoints stay
   fully gated on `email.enabled` (plain 404 rule, no exceptions); a pending
   sign-up caught in the dark ages out via the 24 h pending-expiry sweep (§3)
   instead of lingering as a zombie row. Re-enabling the feature within 24 h
   revives its confirm link (token TTL matches the window).
2. **Pending sign-ups expire after 24 h from `created_at`** (decided; supersedes the
   earlier "never auto-delete"). Rationale: dead data must not accumulate — zombie
   rows, typo'd addresses and abandoned sign-ups all age out on the hourly tick;
   scratchpad bytes go to the existing orphan sweep. Resends do not extend the
   window; confirmation token TTL = 24 h so links and rows die together.
3. **Admin recovery tooling ships with this feature**: `admin list-users`
   (id / email-or-`<pending x@y>` / status / created) and
   `admin delete-user <id-or-email>` — without them a pending row (NULL email) is
   unaddressable by any existing command and recovery means raw sqlite surgery.
   Deletion is by-id or by-confirmed-email; FK cascade + orphan sweep clean up.
4. **Invite codes burn at registration, not at confirmation.** An abandoned,
   never-confirmed sign-up consumes its code; the admin mints another. Chosen for
   abuse-resistance: hoarding/recovering codes by starting-then-dumping sign-ups
   gets nothing.
5. **SMTP `-check-config` probe warns, never fails** — deploy-gating on a mail
   server nobody can fully verify without sending mail would block restarts for
   no safety gained.
6. **Reset only reaches confirmed addresses.** Because a never-confirmed sign-up has
   `email = NULL`, forgot-password structurally cannot see it — so a reset cannot
   double as the sign-up escape hatch (an earlier draft relied on that).
   Confirmation mail is the *only* self-service activation path; `resend-confirm`
   is therefore mandatory. If SMTP is down or the address was mistyped: the user
   resends when mail recovers, the admin deletes the row, or 24 h does it
   automatically and they sign up again.
7. **Admin-disabled vs pending both = `disabled`**, distinguishable by the NULL
   `email` predicate; the expiry sweep touches only `disabled + NULL email`, so an
   admin-disabled account (confirmed email) is never auto-deleted. If we later add
   admin "ban" as a distinct state, it needs its own value before any of these
   predicates must change.

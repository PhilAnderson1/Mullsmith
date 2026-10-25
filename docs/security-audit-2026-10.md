# Security audit — §9.16 OWASP map re-audit (2026-10-25)

Scope: HEAD at M7 (post `97e7d04` + docs/hardening commits). Method: each row
of the spec §9.16 map re-checked against current code, named tests, and the
scan artifacts in [`reports/`](reports). Everything below was executed on
2026-10-25 unless noted.

## Scans run for this audit

| Gate | Tool | Result | Artifact |
|---|---|---|---|
| SCA | `govulncheck ./...` (Go vuln DB, call-graph aware) | **clean, exit 0** | [`govulncheck-2026-10-25.txt`](reports/govulncheck-2026-10-25.txt) |
| Static analysis | `gosec ./...` | **0 issues**; 29 suppressions, each an inline `#nosec RULE -- justification` at the call site | [`gosec-2026-10-25.txt`](reports/gosec-2026-10-25.txt) |
| DAST | OWASP ZAP 2.17.0 baseline (`-quickurl`) against an isolated TLS staging instance (config [`scan-zap.yaml`](../scan-zap.yaml): loopback bind, own DB/scratchpad, tools disabled) | **0 High, 1 Medium (false positive, analysed below), 1 Low (by design), 6 Informational** | [`zap-baseline-2026-10-25.html`](reports/zap-baseline-2026-10-25.html) |
| Fuzzing | `go test ./internal/scratch -fuzz FuzzPathContainment -fuzztime 60s` | **11,075,526 execs, no failures** (plus the seed corpus incl. traversal/symlink cases) | run log 2026-10-25 |
| Race | `go test -race ./...` | all packages pass | — |

### ZAP triage

* **Medium — "Absence of Anti-CSRF Tokens" (3 instances): false positive.**
  The flagged node in every instance is `<form method="dialog">` — the UI's
  confirm dialog, which by HTML semantics never issues a request. The real
  mutations (login, register, chat, files…) are fetch calls that must carry
  the `X-Gabkeep-CSRF` header mirrored from the `gabkeep_csrf` cookie
  (double-submit, middleware in `internal/server/middleware.go`); a
  cross-origin HTML form cannot set custom headers. The middleware itself
  rejects requests lacking the header — observed live during the scan
  (`WARN csrf rejection path=/api/v1/auth/login status=403`) and re-verified
  post-scan with a native POST + hostile `Origin` → 403
  (`TestChatRequiresAuthAndCSRF` covers the API side).
* **Low — "Cookie No HttpOnly Flag" (1): by design.** This is the
  `gabkeep_csrf` cookie; the double-submit pattern requires client JS to read
  it. The *session* cookie is `HttpOnly` + `SameSite=Lax` (+ `Secure` when
  `auth.cookie_secure`/TLS). gosec sees the same shape (G124) and carries a
  `#nosec` with the same reasoning at `internal/auth/cookies.go`.
* **Informational** (auth-request identified ×2, sensitive-data-in-URL ×4 —
  ZAP force-submitted the JS-driven login/register forms as native GETs,
  which the app never does; suspicious comments; cache-control re-examine;
  session-management identified) — reviewed, no action.

## §9.16 map, row by row

| Risk | Spec | Verified where (code → tests) |
|---|---|---|
| A01 Broken Access Control | §9.2, §9.7, §9.5 | Every store query scoped by `user_id` (`internal/store/*.go`); routing default-deny (`internal/server/server.go`); 404-not-403 for foreign objects; `TestConversationCrossUserIsolation`, `TestConversationLifecycle`; CSRF middleware + `TestChatRequiresAuthAndCSRF`; path sandbox below |
| A02 Cryptographic Failures | §9.1, §9.10, §9.12 | argon2id via `internal/auth/argon2.go` (`TestArgonRoundtrip`, policy in `TestPasswordPolicy`); sha256-hashed session tokens (`TestSessionTokens`); TLS config + HSTS + conditional `Secure` cookies (`TestWarnings`, staging scan headers); `${ENV}` secrets (`TestEnvExpansion`) |
| A03 Injection | §9.3, §9.4, §9.9 | 100% parameterized SQL (G202 `#nosec`s are placeholder-builder boilerplate only); DOM-only markdown renderer, never `innerHTML` (`web/assets/app/markdown.js`); CRLF-safe structured logging (`slog`); code exec via `exec.Cmd` argv, no shell (`internal/runner`, `TestCodeRun*`) |
| A04 Insecure Design | §1.1, §6.1, §9.13 | Approval policy fail-closed for unknown tools (`TestRegistryPolicyFailClosed`); 10-min approval timeout ⇒ deny (`TestApprovalTimeoutDenies`); opt-in code exec; invite-only default |
| A05 Security Misconfiguration | §2.2, §9.3, §9.8, §9.10 | Strict YAML (`TestUnknownKeyRejected`), startup warnings incl. open-registration + code_run-auto + no-TLS (`TestWarnings`); security headers middleware incl. strict CSP (`internal/server/middleware.go`; CSP confirmed in scan headers) |
| A06 Vulnerable Components | §9.15 | govulncheck clean (above); pinned deps in `go.sum`; no JS dependencies at all (vendored-by-hand ES modules) |
| A07 Auth Failures | §9.1, §9.11, §8 | Lockout `TestLoginLocks`; rate limiter `TestLimiter`; fixation-safe login (fresh session id); indistinguishable unknown-email/wrong-password responses (`TestValidationCases` + handlers) |
| A08 Integrity Failures | §9.15, §9.3, §9.9 | `make dist` emits sha256; no auto-update; strict JSON decoding (`DisallowUnknownFields` where it matters); runtime allowlist (`TestCodeRunResolvesRuntimeFromConfig`) |
| A09 Logging | §9.14 | Structured events: login ok/fail, csrf rejection, upload, tool approvals; no secrets in logs (`api_key` never logged — `TestBraveSearchKeyNotInURL`); 404 flood visible in staging log |
| A10 SSRF | §9.6 | `internal/tools/egress.go` — full matrix: `TestSSRFBlocklistMatrix`, poisoned-DNS answer sets, literal IPs, redirect-to-private, no-cookie client, scheme+hop caps |

## Path sandbox & uploads (§9.7/§9.4)

Single chokepoint `internal/scratch/path.go`: `openat2`-style chain with
`O_NOFOLLOW` at every component, containment re-checked at each step; all
HTTP and tool file ops route through it. Evidence: dedicated unit suite
(`internal/scratch/path_test.go`: symlink-to-secret, symlinked dir,
`..`/absolute/nested-escape matrices) **plus** the coverage-guided fuzz run
above. Model-side refusals: `TestFsToolsRejectEscapes`,
`TestUploadsNamespaceProtected` (uploads are read-only for the model),
`TestCodeRunStagingCannotEscapeSandbox`. Upload serving: `Content-Disposition:
attachment` always, MIME sniffing, extension blocklist
(`TestSweepExpiredArtifacts` covers TTL lifecycle).

## gosec suppression families (29, all inline-justified)

G115 bounded int→uint conversions (ULID byte packing, argon2 params, rlimits);
G124 conditional cookie `Secure` (http dev mode is an explicit config
choice); G202 placeholder-string SQL builders; G204 runner launching
*configured* binaries (the feature itself, approval-gated); G304 reading the
operator-specified config path; G703 ULID-composed scratch paths (sandbox
inputs are server-generated).

## Open items carried to release notes

Registration emails are unverified in v1 (invite-only is the mitigation,
warned at startup); TLS termination left to operator choice. Both are stated
in the README Security section. ~~`isolated` code mode degrades without
root~~ — resolved 2026-10-26: multi-mode code execution was removed entirely;
runs now always execute inside a bubblewrap jail (own namespaces, no host-file
visibility, no network unless `allow_network`), and gabkeep refuses to start
when bwrap is unavailable rather than degrade.

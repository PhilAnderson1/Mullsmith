# Correctness audit

Date: 2 November 2026

Status: F1–F9 addressed in the working tree. The original findings and audit
evidence below are retained; implementation and validation are recorded at the
end of this document.

Scope: the current working tree, including the uncommitted changes. The review
looked for logic errors, incorrect assumptions, edge cases and inconsistent
behaviour in the Go server and JavaScript client. These are confirmed findings,
not a guarantee that every possible defect has been identified. No application
code or production data was changed during the audit.

## F1 — High: reused tool-call IDs can delete earlier history

**Location:** `internal/store/messages.go`, `turnRowsWhere` (around line 255);
`internal/agent/generate.go`, `callAccumulator.absorb`.

Regeneration selects tool-result rows using the endpoint-provided tool-call ID.
Those IDs are accepted without ensuring uniqueness across the conversation. If
an endpoint reuses an ID in another turn, regenerating the newer turn also
selects and deletes the older turn's tool result. This removes chat history;
it does not delete the corresponding scratchpad files.

**Verification:** an isolated store test created two turns with the same call
ID. The newer turn's retire set included the older result, and regeneration
deleted that older result.

**Proposed fix:** assign application-owned unique call IDs, preserving the same
canonical identity through streaming, execution, persistence and replay. Persist
generation ownership on every result and retire rows by that ownership rather
than by an endpoint ID that may repeat. Check history pairing and client card
identity for the same assumption.

## F2 — Medium: opening a conversation URL can load the wrong conversation

**Location:** `web/assets/app/app.js`, `enterChat` (around line 170) and
`onHashChange` (around line 1466).

Startup awaits `onHashChange()`, but that function does not return the promise
from `selectConversation()`. Startup can therefore continue before the requested
conversation has loaded, clear its hash and select the newest conversation
instead. The outcome depends on response timing.

**Verification:** an isolated Chromium test performed a full page load of an
older conversation URL with delayed conversation metadata. The app selected the
newest conversation instead. A same-page hash change is a different path and
does not reproduce this startup defect by itself.

**Proposed fix:** return or await `selectConversation()` from `onHashChange()` so
startup waits for navigation to finish before deciding whether to select a
fallback conversation. Add a full-load regression case targeting an older
conversation, with delayed metadata.

## F3 — Medium: regeneration can reject a stopped tool turn

**Location:** `internal/chat/execution.go`, `appendToolResult` (around line 277);
`internal/store/messages.go`, `BeginTurnRegeneration` newestness check (around
line 360); `internal/store/turn_regeneration_test.go`, turn fixtures.

Production tool-result rows are written without a generation ID. If generation
stops after a tool executes, its result can be newer than the last assistant
row. Regeneration treats that result as unrelated activity and rejects the
retry with "message is not the newest in its conversation" even though it
belongs to the turn being retried.

The existing turn tests miss this because their fixtures populate the result's
generation ID, unlike the production writer.

**Verification:** an isolated store test used the production row shape for a
turn ending after tool execution. Regeneration incorrectly returned
`ErrNotLastMessage`.

**Proposed fix:** carry the generation ID into result persistence and use that
ownership consistently in newestness checks and retirement. Align fixtures with
the production writer and cover regeneration after cancellation or failure
following tool execution. This shares part of the ownership correction in F1.

## F4 — Medium: configured download timeouts are not honoured

**Location:** `internal/chat/execution.go`, `execTimeout` (around line 37) and
the execution context (around line 152); `internal/config/config.go`, download
timeout defaults.

`web_download` defaults to a 60-second timeout, but the chat executor gives every
non-code tool a 30-second outer deadline. A valid slow download is consequently
cancelled before its configured timeout. Configuring `web_fetch` above 30
seconds has the same limitation.

**Verification:** an isolated chat test confirmed a 30-second execution deadline
against the configured one-minute download timeout.

**Proposed fix:** derive execution deadlines from the relevant tool settings.
Keep any outer margin explicit so that the tool's own timeout can take effect
and increasing a configured timeout has the documented result.

## F5 — Medium: unknown-tool errors are malformed and disappear from history

**Location:** `internal/chat/execution.go`, unknown-tool branch in `runToolCall`
(around line 75).

The unknown-tool branch combines an already JSON-quoted tool name with a
partially quoted error string. For example, the model receives:

```text
{"error":"unknown tool "does_not_exist""}
```

That is invalid JSON. This branch also skips normal tool-call audit and
tool-result persistence. A live error card is emitted, but there is no durable
call/result for history to render after reload.

**Verification:** an isolated chat test invoked the branch and confirmed that
the returned content fails JSON validation. The persistence bypass is explicit
in the early-return path.

**Proposed fix:** serialize the complete error value correctly and record
unknown-tool refusals through the existing durable rejection path, preserving
normal persistence-error handling and live/history agreement.

## F6 — Medium: text truncation can incorrectly report binary content

**Location:** `internal/tools/webfetch.go`, response truncation and UTF-8
validation (around lines 130–145).

`web_fetch` cuts the response at its byte limit before checking whether it is
valid UTF-8. When that cut splits a multibyte character, an otherwise valid
plain-text response is rejected as binary instead of returning truncated text.

**Verification:** an isolated HTTP test returned 1,023 ASCII characters followed
by `é`, with a 1,024-byte limit. The tool returned "response is binary, not text".

**Proposed fix:** distinguish an incomplete trailing UTF-8 character caused by
truncation from invalid binary content, and return text ending at a valid
character boundary. Retain binary rejection for genuinely invalid content.

## F7 — Medium: failed older-message requests cannot be retried

**Location:** `web/assets/app/navigation.js`, `earlierButton` (around line 129).

The "Load earlier messages" click listener uses `{ once: true }`. After a
network or server error, the button remains visible but its listener has been
removed. Further clicks do nothing.

**Verification:** an isolated Chromium test failed the first page request and
clicked the still-visible button again. Only one request reached the endpoint.

**Proposed fix:** retain the listener and use a loading flag or temporary
disabled state to prevent concurrent requests. Re-enable retries on failure;
replace or remove the button only after a successful page load.

## F8 — Low: large file listings can omit the newest files

**Location:** `internal/server/api_files.go`, `dirEntries` and its 500-entry cap
(around lines 435–458); `web/assets/app/app.js`, truncated-list message (around
line 1124).

The server stops after the first 500 entries encountered by the alphabetical
directory walk, then sorts only that subset by modification time. A newly
created file can be excluded entirely while the client claims "Only the most
recent files are shown."

**Verification:** an isolated scratchpad test created 500 alphabetically earlier
files followed by `z-newest.txt`. The response was truncated and omitted the
newest file.

**Proposed fix:** select the newest entries during a bounded scan, or provide
pagination and describe the listing accurately. Do not claim that an
alphabetically selected subset represents the newest files.

## F9 — Medium: deployment backups break repository-wide Go checks

**Location:** deployment backup directories under `backups/`, including
`backups/mullsmith-before-card-order-20261102T050747Z` and subsequent backups.

Privileged backups created during the recent deployments have restrictive
directory permissions inside the checkout. The development user cannot traverse
them, and Go's recursive package discovery fails before it reaches the tests.
Being ignored by Git does not exclude these directories from Go traversal.

**Verification:** `go test -race ./...` failed with a permission-denied error
while opening a deployment backup directory. Explicitly addressing the
application package trees succeeded.

**Proposed fix:** keep deployment backups outside the checkout, or establish
secure ownership/access that permits development commands to traverse them.
Preserve protection for backed-up configuration and database contents; do not
solve this by making sensitive backups world-readable. Update the deployment
procedure so subsequent backups do not recreate the problem.

## Verification and limits

- `go test -race ./cmd/... ./internal/... ./web/...` passed for all application
  packages. The usual `go test -race ./...` command was blocked by F9.
- Temporary Go overlays reproduced F1, F3, F4, F5, F6 and F8 without adding test
  files to the repository.
- An isolated Chromium session with a temporary database and fake model
  reproduced F2 and F7.
- The audit did not modify application code, production data or deployment
  settings. The temporary test artifacts were written under `/tmp`.
- Findings are based on the current working tree. Line numbers are navigation
  aids and may change as fixes are implemented.

## Implementation — 2 November 2026

Implemented under the agreed premises: no historical-data compatibility or
backfill, file-list pagination rather than a permanent result cap, and no
deployment backups.

| Finding | Action taken |
|---|---|
| F1 | The generator now mints a unique application ID for every call, regardless of the endpoint ID. Identity remains stable across streamed arguments, cards, execution and protocol replay. Turn retirement uses explicit generation ownership, so a repeated call ID cannot select another generation's results. |
| F2 | `onHashChange` returns the conversation-loading promise, allowing startup to await the requested conversation before considering a fallback. |
| F3 | Successful, failed and rejected tool results now persist their generation ID. Regeneration ignores the turn's own later result rows. A regression drives the real result writer and regenerates a turn ending immediately after a refused tool call. |
| F4 | Web tools receive their configured HTTP timeout plus a five-second outer execution margin. Other tools keep their existing execution budgets. |
| F5 | Unknown tools and unavailable image-view calls use the durable rejection path, including correctly serialized JSON, audit records, result messages and matching live/history cards. |
| F6 | Truncation removes an incomplete trailing UTF-8 character before validation; invalid content elsewhere is still rejected. Tests cover two-, three- and four-byte characters. |
| F7 | Earlier-message buttons retain their handler, disable during a request and re-enable after failure, allowing retries without duplicate concurrent loads. |
| F8 | The Files API now returns cursor pages: 50 entries by default, at most 200, ordered by modification time descending and path ascending. Nanosecond timestamps and the selected filter are retained in the cursor. The client offers Load more files, retains failed-page cursors for retry, deduplicates paths and resets pages on tab switches and file-operation refreshes. |
| F9 | Existing deployment backups were deleted as authorized. The obsolete checkout-backup rule was removed from `.gitignore`, and README now states that deployments do not create backups. Recursive Go commands work again. |

File listing reuses the existing fd-pinned, batched scratchpad walker. It scans
at most 200,000 entries per layout directory at the existing depth limit and
retains only one page plus a sentinel in its selection heap. Directory file
counts are separately bounded. Incomplete listing scans return an explicit
error instead of a partial success claiming to contain the newest files.

Pagination reflects the current disk state on each request. A deleted cursor
file does not invalidate the cursor. Files added or moved ahead of the current
cursor appear on refresh; this does not introduce a filesystem index or stored
snapshot.

No schema migration or historical row backfill was added. Generation ownership
is populated for newly produced tool results; existing rows remain unchanged.
The schema change is a comment describing the existing column's ownership.

### Validation after implementation

- `go test ./...` passed.
- `go test -race ./...` passed, including repository-wide package discovery.
- `go vet ./...` passed.
- `gosec ./...` reported zero issues.
- `make vuln` reported no vulnerabilities affecting the application. It also
  noted one module-level vulnerability whose affected code is not called.
- `make test-js` passed all 55 tests, including failed earlier-page retry.
- `make test-browser` passed all 15 Chromium scenarios, including full startup
  navigation to an older conversation and file-page retry/tab-reset behaviour.
- Go regressions cover repeated IDs within a response and across requests,
  generation-scoped retirement, durable refusals, stopped-turn regeneration,
  configured timeouts, UTF-8 boundaries and pagination over more than 500 files.
  File pagination tests also cover timestamp ties, filter-bound cursors, cursor
  deletion, new files, cancellation and explicit scan-budget failure.

Deployed on 2 November 2026 as `52a39a3-dirty-20261102T055923Z`, without taking
deployment backups. Production configuration validation, service health, running
binary identity and served client-module checks passed. The live page includes
the Load more files control. No database reset or production configuration
change was required.

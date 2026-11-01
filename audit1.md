# Unused functions and unconsumed data audit

Date: 2026-11-01

## Scope and conclusion

Audit of the Go server and JavaScript client for functions without callers, obsolete execution paths, and data that is calculated, stored, or transmitted without being consumed by the application.

There are specific leftovers and incomplete data flows worth cleaning up, rather than widespread dead functions. The most significant finding is the loss of the model's finish reason: replies stopped by a token limit or filtering can be recorded as complete.

No application code was changed during this audit. References describe the working tree at the time of review.

## Findings and proposed fixes

### D1 — The model's reason for stopping is discarded

**Priority:** Correctness fix.

The streaming parser populates `Delta.Finish` from the upstream `finish_reason`, but the generator never reads it. A focused reproduction confirmed that `stop`, `length`, and `content_filter` all become `status="complete"` with no error. An answer cut short by the token limit can therefore appear successfully finished.

**Proposed fix:** Carry the finish reason through the generation result and persistence. Define explicit handling for normal completion, output-limit termination, and filtering. Give incomplete answers a durable explanation that appears both live and after reload. Add regression coverage for each outcome, including tool-call rounds.

**References:** [internal/llm/stream.go:516](internal/llm/stream.go#L516), [internal/agent/generate.go:115](internal/agent/generate.go#L115).

### D2 — Structured tool output is stored and transmitted but never displayed

**Priority:** Targeted data-flow cleanup, with a missing truncation indication.

Code-run stdout/stderr and filesystem metadata travel through `Result.Data`, stored display JSON, SSE, and the client projection. The renderer consumes URL results and shared-file information; it ignores these other structured payloads. It also copies the display's `truncated` flag into the client view object without rendering an indication.

The model-facing tool result has a separate consumer and must be preserved. The unnecessary copy here is the display payload for information the current UI does not use.

**Proposed fix:** Define the display data each tool actually needs. Remove unused display payloads or render the useful information deliberately. Show a notice when display data has been dropped by the display budget. Keep the same projection for live and reloaded cards, and preserve model-facing results.

**References:** [internal/tools/codeexec.go:326](internal/tools/codeexec.go#L326), [internal/tools/files.go:145](internal/tools/files.go#L145), [web/assets/app/thread.js:182](web/assets/app/thread.js#L182).

### D3 — Account responses perform an unnecessary filesystem scan

**Priority:** Performance cleanup.

`mePayload()` walks the user's scratchpad to calculate quota usage. The shipped client never reads quota from this account payload; the Files screen obtains its quota separately from the file-list endpoint. This adds filesystem work to authentication and profile responses without benefiting the UI.

**Proposed fix:** Remove quota calculation and the unused quota field from the account payload, after checking any documented API compatibility requirements. Keep quota reporting in the Files response where the UI consumes it.

**References:** [internal/server/api_auth.go:397](internal/server/api_auth.go#L397), [web/assets/app/app.js:1561](web/assets/app/app.js#L1561), [web/assets/app/app.js:1642](web/assets/app/app.js#L1642).

### D4 — History responses send model-facing data the client discards

**Priority:** Payload and projection cleanup.

Tool-result messages are returned to the browser and then filtered out by `isDisplayable()`. Tool cards also receive the model-facing `result` alongside `display`, although current cards use `display`; `result` is read only by the legacy fallback when no usable display payload is present.

These stored results remain necessary for server-side context replay. The redundant data is in the browser history response.

**Proposed fix:** Slim the browser history projection while preserving pagination and any documented API contract. Avoid transmitting hidden tool-result bodies unnecessarily, and include the legacy result payload only where the fallback needs it. Preserve stored protocol data and legacy card rendering. Verify page boundaries and live/reloaded equivalence after changing the projection.

**References:** [internal/server/api_conversations.go:96](internal/server/api_conversations.go#L96), [internal/server/api_conversations.go:421](internal/server/api_conversations.go#L421), [web/assets/app/app.js:738](web/assets/app/app.js#L738), [web/assets/app/thread.js:147](web/assets/app/thread.js#L147).

### D5 — A client rendering branch has no producer

**Priority:** Dead-code cleanup.

`msgNode()` checks `m.tool_note` and calls `noteCard()`, but the server never produces `tool_note`. Terminal explanations now use `status_note`. The branch is effectively dead despite `noteCard()` having an apparent caller.

**Proposed fix:** Remove the obsolete `tool_note` branch and `noteCard()`. Keep terminal explanations on the shared `status_note`/round-note path.

**References:** [web/assets/app/app.js:783](web/assets/app/app.js#L783), [web/assets/app/app.js:1396](web/assets/app/app.js#L1396).

### D6 — Markdown styles no longer match the rendered elements

**Priority:** Rendering fix.

The markdown styles target `.msg-body.md`, but the renderer now puts the `md` class on the nested `.stream-text` element. Those selectors consequently do not apply to the current message structure.

**Proposed fix:** Retarget the styles to the current prose wrapper. Check paragraphs, headings, lists, blockquotes, inline code, and links in both completed live messages and reloaded history. Include a real-browser check of the resulting styles.

**References:** [web/assets/styles.css:685](web/assets/styles.css#L685), [web/assets/app/app.js:762](web/assets/app/app.js#L762), [web/assets/app/app.js:2076](web/assets/app/app.js#L2076).

### D7 — Superseded store methods survive through tests

**Priority:** API and test consolidation.

`AttachFilesToMessage`, `IsLastMessageInConversation`, and `DeleteMessage` have no production callers. Production uses atomic send and turn-regeneration operations. The free `DisplayJSON` function and `Result.DisplayJSON` convenience method likewise have only test callers; the production path clamps a card once and calls `Card.JSON()`.

**Proposed fix:** Consolidate tests around the operations production actually uses. Remove redundant methods and wrappers where no fixture need remains. If a helper is retained solely for fixtures, document that purpose clearly. Preserve intentional test hooks rather than deleting every function outside the production call graph.

**References:** [internal/store/attachments.go:39](internal/store/attachments.go#L39), [internal/store/messages.go:169](internal/store/messages.go#L169), [internal/store/messages.go:183](internal/store/messages.go#L183), [internal/tools/display.go:72](internal/tools/display.go#L72).

### D8 — The runner's warnings return value is always empty

**Priority:** Signature and control-flow cleanup.

`runner.NewForConfig()` declares a warnings slice but never adds anything to it. Its caller nevertheless loops over the returned warnings. The loop has no effect with the current implementation.

**Proposed fix:** Simplify the return signature and remove the ineffective warning loop. Keep actual setup failures and existing independent configuration warnings intact.

**References:** [internal/runner/runner.go:113](internal/runner/runner.go#L113), [internal/runner/runner.go:142](internal/runner/runner.go#L142), [cmd/mullsmith/main.go:166](cmd/mullsmith/main.go#L166).

## Reachability results and intentional exceptions

Go reachability analysis from the production executable identified ten unreachable functions. All ten have test callers:

- `auth.HashWorkCount`
- `auth.Limiter.Len`
- `auth.LoginLocks.Len`
- `server.Server.Handler`
- `store.DB.AttachFilesToMessage`
- `store.DB.IsLastMessageInConversation`
- `store.DB.DeleteMessage`
- `store.SetContextHistoryCap`
- `store.DB.ExecForTest`
- `store.DB.CreateUser`

Several are legitimate test hooks or fixture helpers. D7 identifies the subset worth consolidating because production moved to newer operations. A lack of production callers alone is not a reason to delete a test seam.

Including test entry points left one entirely unused helper: `startCallIDs()` in [internal/server/api_chat_phantom_test.go:78](internal/server/api_chat_phantom_test.go#L78). It can be removed.

Source tracing also identified the test-only `DisplayJSON` wrappers described in D7. Reachability tools are conservative around reflection; their output was checked against actual references rather than treated as an exhaustive removal list.

Audit-only database fields, such as tool-call timing, have a documented purpose outside the UI and should be retained. JSON serialization, interface dispatch, callbacks, and compatibility fallbacks were considered when assessing apparent unused data and functions.

## Verification

- Go function reachability analysis from `./cmd/mullsmith`, followed by analysis including test entry points with `-test ./...`.
- Type-aware Go field-usage scan and manual tracing of producers and consumers across the server/client boundary.
- Temporary Go overlay reproduction: upstream finish reasons `stop`, `length`, and `content_filter` each produced `status="complete"` with no error, confirming D1. The test recorded current behaviour; its passing does not mean that behaviour is correct.
- Focused Node checks confirmed the structured filesystem/code display payloads and display truncation flag produce no additional visible output in the card renderer.
- `go vet ./...` passed.
- `make test-js` passed all six JavaScript test files.

Temporary audit helpers were kept under `/tmp`; no application code or production deployment was changed.

## Recommended order

1. Fix D1 and preserve truthful terminal outcomes through persistence and rendering.
2. Fix D6 and the truncation indication in D2.
3. Remove unnecessary work and payloads in D3, D4, and the remaining parts of D2.
4. Consolidate obsolete paths and helpers in D5, D7, and D8, and remove the unused test helper.


## Implementation record — 2026-11-01

D1–D8 have been addressed. Following the user's instruction, the implementation uses the current data and stream contracts without historical-data fallbacks. No database reset, data deletion, or production deployment was performed.

| Finding | Action taken |
| --- | --- |
| D1 | The generator consumes the upstream finish reason. Output-limit, filtering, and unexpected stops produce an error status with a durable explanation. Content, status, and note are finalized together in one database update. Incomplete rounds do not execute tool proposals; their transient proposal cards are removed when the stream settles. |
| D2 | Removed filesystem/code display data that the UI did not consume; the separate model-facing results remain intact. Search-card data now contains only titles and URLs. Code-run summaries retain timeout and output-truncation indications. The shared live/history card renderer displays an omission notice when the display budget drops details. |
| D3 | Account identity responses no longer calculate or include quota usage. The Files endpoint continues to report quota where it is displayed. |
| D4 | History pagination now selects visible messages before applying the page limit. Model-only tool-result and synthetic image rows remain in context replay. Browser history sends tool display payloads without duplicate model-facing results. The redundant client-side visibility filter was removed. |
| D5 | Removed the unproduced `tool_note` branch and `noteCard()`. Terminal explanations use the durable round/status note. |
| D6 | Retargeted markdown CSS from `.msg-body.md` to the actual `.stream-text.md` prose wrapper. Chromium checks cover styles before and after reload. |
| D7 | Removed `AttachFilesToMessage`, `IsLastMessageInConversation`, `DeleteMessage`, both `DisplayJSON` wrappers, and the entirely unused `startCallIDs` test helper. Attachment tests now exercise the production atomic write operations. Intentional fixture helpers and fault-injection hooks remain. |
| D8 | Simplified runner construction to return `(Runner, error)` and removed the always-empty warnings result and its caller loop. |

Additional simplification within these changes:

- Removed reconstruction of stored cards from historical model-facing results.
- Removed the pre-round stream fallback, pre-start token buffer, and duplicate terminal `message_id`/timing fields. Per-round outcomes own row identity and timing. Transport loss and persistence failure still settle announced rounds explicitly.
- Removed row-only regeneration compatibility. New assistant writes receive a generation identity; regeneration consistently operates on a generation's whole turn.
- Removed tests dedicated to historical data/wire compatibility, updated fixtures to the current contract, and updated `spec.md` to describe the resulting behaviour.

Implementation verification:

- `go vet ./...` passed.
- `go test -race ./...` passed across all packages.
- `make test-js` passed all six JavaScript test files.
- `make test-browser` passed all nine Chromium scenarios, including live/reloaded equivalence, blank-bubble prevention, regeneration, abort, output-limit/filtering explanations, markdown styles, and interrupted tool proposals.
- New Go regressions cover normal and incomplete finish reasons, suppression of incomplete tool execution, visible-message pagination, omission of model-only history data, and retention of stored tool results for context replay.
- `git diff --check` passed.

The final Go reachability scan, including application and test entry points
(`deadcode -test ./...`), reported no unreachable functions.

Before deployment, `make lint vuln` also passed: gosec reported zero issues,
and govulncheck reported no vulnerabilities affecting the application.

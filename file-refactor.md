# Refactor plan: the disk becomes the single source of truth for files

## 0. Goal and mandate

Remove the `files` table (and the registry concept) entirely. After this
refactor, **a file exists iff its bytes exist on disk**, and everything the
Files page shows, lists, downloads and deletes is the plain directory content.
The two parallel registries — DB rows vs directory entries — that today need
dedup maps, cascade deletes, path-alias fixes and an orphan sweeper to agree,
collapse into one.

Explicitly decided by the owner (2026-10-26):

- **No data migration.** All existing uploads/artifacts/work files in
  production will be deleted; the `files` table is simply dropped.
- The refactor adopts the three semantic decisions below **together**, as one
  change — no half-removal of the table.

### The three semantic decisions

| Fact the table used to hold | Disk-native replacement |
|---|---|
| Display filename of an upload (bytes stored under a ULID) | **The real filename lives on disk.** Uploads are stored under the (sanitized) name the user gave, with collision-renaming `report.pdf` → `report (1).pdf`. |
| "This file was officially shared" (artifact row, possibly pointing into `work/`) | **Sharing = copying the file into `artifacts/`.** The directory *is* the state. Re-sharing a `work/` file delivers a new snapshot (`report (1).txt`, …); old chat cards keep pointing at the bytes that were delivered. |
| Sliding `expires_at` TTL | **mtime-based sweep per directory:** `artifacts/` older than `artifact_ttl` (168 h) is swept; `work/` older than a new `work_ttl` (24 h default — exactly today's de-facto orphan-sweep age for unregistered files) is swept; `uploads/` never expires. |

A fourth consequence: **message attachments reference paths, not ids.** The
`attachments` table survives but stores `path TEXT` instead of `file_id`.

## 1. What the `files` table does today, and where each job goes

Read from `internal/store/files.go`, `internal/server/api_files.go`,
`api_workfiles.go`, `artifacts.go`, `maintenance.go`, `internal/tools/files.go`:

| Today (table job) | After |
|---|---|
| Upload bytes stored as `uploads/<ULID><ext>`; human name only in `files.name` | Stored as `uploads/<sanitized display name>`; collision bumps ` (N)` |
| `files.media_type` cached at upload (sniff + ext hint) | Sniffed on the fly at download (exactly what `handleWorkDownload` already does) or from a small ext map for the model manifest |
| `files.sha256` computed at upload and at share | Dropped — nothing reads it |
| `kind = upload | artifact` | Top-level directory *is* the kind (`uploads/`, `artifacts/`, `work/`) |
| `files_share` upserts a row pointing at the file **where it lies** (often inside `work/`) | `files_share` **copies** the file into `artifacts/<name>` (NOREPLACE + collision bump); no row |
| Artifact TTL slides on re-share via `expires_at` | mtime + `artifact_ttl`; a fresh share copy has fresh mtime |
| `attachments(message_id, file_id)` | `attachments(message_id, path)` |
| Attachment manifest to the model (`name/path/media/bytes` from row) | Same fields from `stat(path)` + ext classifier |
| `GET /files` merges rows + unregistered `work/` files, deduped by `RegisteredPathsUnder`, cursor-paginated | One physical walk of the three directories, newest-mtime first, capped (the `workEntries` walker generalized) |
| `DELETE /files/{id}` deletes row then unlinks | `DELETE /files?path=` unlinks (file or tree — already implemented for `work/`) |
| Work delete cleans artifact rows under the tree (`DeleteArtifactsUnder`, LIKE-escaped) | Nothing to clean: rows don't exist. Chat cards already rebuild from `tool_calls` audit and 404 gracefully (this is today's behavior for work-located cards) |
| Orphan sweep: bytes without rows older than 24 h | **Dies entirely.** Its definition ("no row") no longer exists. Its actual effect — deleting unregistered `work/` files after 24 h — becomes the explicit `work_ttl` mtime rule |
| Deleted user → rows cascade → bytes orphaned → swept | Sweep rule: scratchpad dir whose ULID is not in `users` gets deleted (age-gated) |
| Quota accounting (`DirSize` walks, `commitMu`, watchdog) | **Unchanged** — it was always disk-based |
| Jail mount layout (§9.9), `checkWriteTarget` allowlist | **Unchanged** |

Components deleted outright: `internal/store/files.go` (all of it),
`internal/server/artifacts.go` (the sink), `tools.ArtifactSink` /
`ArtifactRecord`, `maintenance.go`'s known-set walk,
`RegisteredPathsUnder`, `DeleteArtifactsUnder`, `FindArtifactByPath`,
`ExpiredArtifacts`, `ExistingFileSet`, `ListFiles` cursor machinery for
files, the share-time SHA-256 hashing, and the upload-time `store.File`
insert + rollback-on-DB-failure path.

## 2. New API surface

Routes (`internal/server/server.go`):

| Method | Path | Change |
|---|---|---|
| POST | `/api/v1/files` | unchanged shape; response now `{name, path, size, created_at, media_type, download_url}` — no `id` |
| GET | `/api/v1/files?kind=` | `kind` ∈ `""`(All) `uploads` `artifacts` `work`. Physical listing; `{items, truncated, quota}`; **no more `next_cursor`** (cap 500/dir like the work view) |
| GET | `/api/v1/files/download?path=` | NEW: replaces `GET /files/{id}/download` **and** `GET /files/work/download`. `path` must be 2+ segments under one of `uploads|work|artifacts`. Sniffed media type, regular files only, inline allowlist (§9.4) unchanged, RFC 5987 disposition from the disk basename |
| DELETE | `/api/v1/files?path=` | NEW: replaces `DELETE /files/{id}` **and** `DELETE /files/work`. Same fd-pinned file-or-tree semantics as today's `deleteWorkTree`, generalized to the three directories (layout dirs themselves stay unaddressable — `ParseRel` ≥2-segment rule) |
| GET/DELETE | `/api/v1/files/{id}…`, `/api/v1/files/work…` | removed |

Ownership model is unchanged: `path` is always joined under the *session
user's* root via the fd-pinned sandbox (§9.7), so one user can never address
another's bytes; every rejection answers `404 not_found` (never "forbidden").
`isKnownPath` and the `bodyLimit` upload exemption already key on the
`/api/v1/files` prefix and keep working.

Upload naming rules (server-side sanitizer, extends `sanitizeDisplayName`):

- basename, control chars → `_`, trimmed, length-capped at 200 (keep);
- a leading `.` becomes `_` + the dots (`.env` → `_env`, `..x` → `_x`): dot
  files are invisible to the lister and would become ghosts;
- the result is a single path segment — no `/`, no NUL — enforced again by
  `scratch.ParseRel` at every use.

## 3. New maintenance sweep (`internal/server/maintenance.go`)

Hourly pass, bounded, no DB known-set:

1. `.upload-*` temps in the scratch root older than 24 h → remove (unchanged;
   with the rename-failure rollback gone, these are the only crash leftovers).
2. ULID user dirs **not present in `users`** (added
   `store.UserIDs() ([]string, error)`) → remove every regular file older
   than 24 h (same age-gated per-file walk as today).
3. Per-user walk, per top-level directory:
   - `uploads/` — never swept;
   - `work/` — regular files with mtime older than `work_ttl` (default 24 h);
   - `artifacts/` — regular files with mtime older than `artifact_ttl`;

   Expiry is per-FILE, at any depth: a file in a subdirectory ages on its own
   mtime and a fresh file keeps its full path (sweeping never reorganizes).
   - empty subdirectories under `work/` / `artifacts/` whose own mtime is
     older than that directory's TTL → `rmdir` deepest-first (Lstat-checked,
     never a symlink; a non-empty rmdir simply fails and is retried next
     pass — a folder holding one fresh file survives whole). Files vanish on
     the TTL rule, so without this the desk and the deliverables box fill
     with empty folders.
   Symlinks and special files: left alone with a warning (unchanged).
4. Fail-closed stance: a user dir that cannot be walked is logged and left
   untouched (sweeping less is always safe; sweeping more is not).

## 4. Step-by-step implementation

Each step compiles and passes `go build ./... && go vet ./...` on its own;
commit per step. Steps 1–3 are independent of 4–6.

### Step 1 — schema: drop the registry, re-key attachments

New `internal/store/migrations/0006_drop_files.sql`:

```sql
DROP TABLE IF EXISTS attachments;   -- first: it FK-references files
DROP TABLE IF EXISTS files;          -- its indexes go with it

CREATE TABLE attachments (
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  path       TEXT NOT NULL,          -- uploads/<name>, canonical (ParseRel rejoined)
  PRIMARY KEY (message_id, path)
);
```

Foreign keys are on (`foreign_keys(1)` in the DSN), so the drop order
matters. All pre-existing rows vanish — sanctioned by the mandate above.

Store changes (`internal/store/`):

- Delete `files.go`. Define `type Attachment struct{ Path string }` next to
  `Message`; `Message.Files []File` → `Message.Attachments []Attachment`,
  with `Name()` sugar = `path.Base`.
- `AttachFilesToMessage(ctx, messageID, userID string, paths []string)` —
  ownership was a row lookup; ownership is now *structural* (the caller has
  already validated the path resolves inside that user's `uploads/`), so it
  is a plain insert batch in one tx.
- `filesForMessages` selects `message_id, path`; `attachFiles` fills
  `.Attachments`.
- `files_test.go` → rewrite as attachment tests (attach, batch, dedup PK,
  cascade with the message).

### Step 2 — scratch: collision-safe placement

Add to `internal/scratch` (with unit tests):

- `func PlaceNoReplace(root, userID, rel, tempHostPath string) error` —
  opens the containing directory of `rel` through the existing fd-pinned
  chain and calls `unix.Renameat2(AT_FDCWD, temp, dirfd, name, RENAME_NOREPLACE)`
  (Linux ≥ 3.15; document as a hard requirement next to the existing
  user-namespaces one). Maps `EEXIST → ErrExists`; callers bump the name and
  retry.
- `func FreeName(root, userID, dir, desired string) (rel string, err error)`
  (optional helper — the two callers loop `PlaceNoReplace` directly): given
  `report.pdf`, yields `report.pdf`, `report (1).pdf`, … capped at 200 tries
  then `ErrExists`.
- `func UserIDs` counterpart lives in the store, not here.

### Step 3 — uploads: real names on disk

`internal/server/api_files.go`:

- Keep the entire streaming pipeline (concurrency gate, disk floor, budget,
  sniff head, SHA **removed**, size cap, temp file) exactly as is.
- `sanitizeUploadName` (§2 rules) turns the display name into the disk
  segment.
- `commitUpload` runs **inside `commitMu`** as today, but places with
  `PlaceNoReplace` + `(N)` bump loop instead of plain `os.Rename`
  (which would silently overwrite a same-named upload). Quota logic,
  fail-closed `DirSize` behavior, error codes: unchanged.
- Delete the `CreateFile`-row block and its rollback; on any post-rename
  error the file simply stays — it is now a real upload, not an orphan.
- Response JSON = §2 (path-based `download_url`).

### Step 4 — list / download / delete: one physical file API

- Generalize `workEntries` → `dirEntries(root, userID, dir, cap)` covering
  all three directories (dirs listed with bounded counts everywhere —
  `fs_write` can create nested `artifacts/` too). Folder rows keep a single
  kind marker (rename `work-dir` → `dir`, CSS updated in one place).
- `handleFileList` becomes: validate `kind`, walk the selected dir (or all
  three for All), merge-sort by mtime desc, cap 500/dir, emit
  `{items, truncated, quota}`. Items: `kind` (`upload|artifact|work|dir`),
  `path`, `name`, `size`, `modified_at`, `download_url`; `artifacts` items
  also carry computed `expires_at = mtime + artifact_ttl` so the UI's
  "expires …" line survives. No cursor.
- Merge `handleWorkDownload`/`handleFileDownload` into one
  `handleFileDownload` on `?path=`: 2+ segments, first segment ∈ the three
  dirs, `scratch.Stat` regular-file check, `scratch.OpenFile`, sniff,
  `inlineSafeTypes` + `neverInlineExts` (§9.4) unchanged.
- Merge both delete handlers into `handleFileDelete` on `?path=`: same
  guard, `scratch.Remove` / `scratch.RemoveTree`. **No row cleanup** — that
  whole block (and the "artifact rows left behind" logging) disappears.
- Delete `api_workfiles.go`; its route registrations move to the four
  §2 routes.
- Attachment validation in `api_chat.go`: `validateAttachments` →
  `validateAttachmentPaths` — `ParseRel`, first segment `uploads`, ≥2
  segments, `scratch.Stat` regular-file check, same count cap and same
  `attachment_not_found` 404s (never leaks existence). `attachment_ids` →
  `attachment_paths` in the request body.
- `attachmentManifest(files []store.Attachment)`: per path `scratch.Stat`
  for bytes (skip silently if gone — the model gets told about what
  exists); media from a small ext classifier (share `tools.mediaTypeForPath`
  by moving it to `internal/scratch` or duplicating ~15 lines server-side).
- `api_conversations.go`: message JSON `files` array built from
  `.Attachments` (`name`, `path`, `download_url`, `size` via one stat pass —
  or omit size; the UI chip tolerates ""). No `expires_at`, no `id`.

### Step 5 — `files_share` becomes a copy

`internal/tools/files.go`:

- Remove `ArtifactSink`, `ArtifactRecord`, the `Sink` field, and the
  whole-file SHA-256 hash pass.
- `FilesShare{ UserQuota int64; QuotaMu *sync.Mutex }` — sharing now
  **consumes quota** (a copy is a write), so it takes the same
  disk-floor + `lockQuota` + `checkQuota` sequence as `fs_write`.
- Rules:
  - `uploads/` source → refused (unchanged, audit finding 8 rationale);
  - source already under `artifacts/` → no copy; deliver it as-is (its
    existing mtime governs expiry);
  - `work/…` (or any other allowed path) → stream-copy to
    `artifacts/<basename>` via `PlaceNoReplace` + bump loop under the quota
    lock. **Each share is a new snapshot**: re-sharing an edited draft
    creates `report (1).txt` rather than mutating what an earlier card
    points at — strictly better than today's upsert, which silently
    changes what old cards deliver.
- Result JSON: `{path, name, bytes, url: "/api/v1/files/download?path=…",
  note}` (drop `file_id`). Update the tool `Description()` (mention
  snapshot semantics and that uploads can't be shared — both already
  required).
- `internal/tools/build.go`: drop the `artifacts ArtifactSink` parameter
  from `BuildFor`; wire `QuotaMu` into `FilesShare`. `server.go`: drop
  `artifactSink` construction, delete `internal/server/artifacts.go`.

### Step 6 — maintenance rewrite

As §3: replace the row-driven expiry block and orphan known-set walk with
the directory/mtime rules; add `store.UserIDs()`; keep `.upload-*` cleanup,
`removeIfOld`, hourly cadence, bounded passes, and the startup-run semantics
(`RunMaintenance` signature unchanged).

### Step 7 — frontend

`web/assets/app/api.js`:

- `uploadFile` unchanged; `listFiles(kind)` unchanged call shape;
  `deleteFile(id)` → `deleteFile(path)` = `DELETE /api/v1/files?path=…`;
  `listWork`/`deleteWork` removed (tabs call `listFiles(kind)`).

`web/assets/app/app.js`:

- `state.filesCursor` / "load more" machinery removed; the `truncated` note
  (already there for Work) shows on every tab.
- `fileRow`: single delete path — `api.deleteFile(f.path)` for every kind;
  the `isDir || f.kind === "work"` branch collapses away; the 404 =
  "already gone, refresh" ghost handling and full `loadFiles(true)` reload
  stay exactly as battle-tested.
- `artifactFromAudit`: gate on `typeof parsed.path === "string"` instead of
  `file_id`.
- Composer: `state.pending[].id` → `.path`; message body sends
  `attachment_paths`.
- `fileApiUrl`'s starts-with-`/api/v1/files/` guard already admits
  `download?path=…` URLs (percent-encoded, no whitespace/quotes) — verify
  with a test pin, don't relax it.
- `web/index.html`: badge CSS `work-dir` → `dir` (and confirm tab labels).

### Step 8 — config, docs, prompts

- `internal/config`: add `scratchpad.work_ttl` (default 24 h) next to
  `artifact_ttl`; validation `> 0`; document "0 disables" is **not** offered
  for TTL knobs (fail-closed doctrine: outputs must expire).
- `README.md` security + config sections; `internal/agent/context.go`
  `CodeRunGuidance` and tool descriptions if wording drifted ("share copies
  the file into artifacts/").
- `spec.md`: §4 schema (files gone, attachments re-keyed), §5.3 route table
  rewritten to §2 above, §8 background jobs (mtime sweep rules, orphan sweep
  removed — with a sentence on *why* it can't exist anymore), §9.4 (upload
  naming + collision rule replaces "stored name is the file id"), §10.2
  Files view row, §12 risk table row ("artifact TTL + orphan sweep" →
  "mtime sweep"). Note in §8 that disk = truth is a **design principle**:
  anything that cannot be represented on disk is not a feature.

### Step 9 — test matrix

| Area | Work |
|---|---|
| `internal/store` | 0006 migration applies on a DB carrying 0005 data (rows discarded, no FK error); attachments by path, dedup, cascade |
| `internal/server` | upload keeps the real name; two same-named uploads → ` (1)` on disk; path download inline/attachment/FIFO/refused exts; delete file, tree, layout-dir refusal, cross-user 404; listing kind filters + folder rows + computed `expires_at`; share copies into `artifacts/`, re-share bumps, share-of-artifact no-ops, share quota refusal; attachment_paths validation incl. `../` and `uploads` itself; sweep: uploads survive, work file dies at `work_ttl`, artifact at ttl, empty work/ and artifacts/ subdirs rmdir'd deepest-first
   (non-empty folder with one fresh file survives whole), unknown-user dir removed, `.upload-*` gone, walk-error leaves tree untouched |
| `internal/tools` | `files_share` result shape + bytes landed; quota-locked; existing write-target tests unchanged |
| `web` (`appjs_test.go`) | pinned text updates: `deleteFile(f.path)`, `attachment_paths`, `parsed.path`, generation guard unchanged |
| `internal/runner` | **nothing changes** (jail untouched) — re-run the suite as root + production-shape unit anyway as part of the final gates |

Full gates before deploy: `gofmt -l . && go vet ./... && go test ./... -p 4`,
`-race` on touched packages, `gosec` 0 issues, `govulncheck` 0.

### Step 10 — deploy runbook (production)

1. `sudo sqlite3 "file:/var/lib/gabkeep/db/gabkeep.db" ".backup /root/gabkeep.pre-filedrop.db"`
   (rollback anchor — the old binary expects the `files` table).
2. Stop service; wipe the bytes (the three directories stay, contents go):
   `sudo rm -rf /var/lib/gabkeep/scratchpad/*/{uploads,artifacts,work}/*`
   — and optionally the known root litter **only if the owner changes his
   mind** (standing instruction: leave `bin_listing.txt`,
   `usr_bin_listing.txt`, `mydir`).
3. `make build && sudo install -m 0755 gabkeep /usr/local/bin/gabkeep`;
   start — migration 0006 runs on boot.
4. Smoke (existing cookie-jar recipe): login, upload `smoke.txt` (verify the
   on-disk name), upload it again (verify `smoke (1).txt`), list All/uploads,
   download inline + attachment, attach to a chat message, ask for a share
   (verify the copy under `artifacts/` + card renders from audit), delete
   via Files page, journal clean, quota bar moves.

Rollback = restore the DB backup + old binary + re-provision scratch dirs
(anything uploaded to the new schema has ULID-less names the old code can
still serve, but with no rows it would be swept as "orphans" — acceptable
for a rollback day).

## 5. Consequences and risks (accepted, documented)

- **Old chat cards / attachments 404 after the wipe** — sanctioned: all
  saved files are deleted as part of this change; downloads fail
  gracefully, exactly like deleting a file today.
- **Ghost chips by design:** deleting an upload does not (and will not)
  reach into `attachments` to rewrite history — a chat chip is a historical
  record that the file *was* attached. Download 404s. This matches today's
  behavior for shared work files.
- **Re-share duplicates** (snapshot semantics) instead of one row sliding a
  TTL — copies expire on their own mtime.
- **Filename collisions are visible to users** (`report (1).pdf`) — that is
  the cost of the name living on disk, and what desktop OSes do too.
- **`work/` files disappear after 24 h of no touching** — already the true
  behavior (orphan sweep); now it is an explicit, named rule instead of an
  emergent side effect of "no row".
- Listing is capped (500/dir), no pagination — same as today's Work tab.
- `mtime` replaces `created_at` everywhere the UI showed dates.
- Unicode-lookalike names can "collide" with themselves across NFC/NFD
  spellings — cosmetic only, bumping handles it.
- SQLite gains `Renameat2` as a kernel requirement (3.15+); already far
  below what bwrap user-namespaces require.

## 6. What this kills forever (the mess being removed)

`files` table + its three indexes; `artifactSink`/`ArtifactSink`; the
share-time and upload-time SHA-256 passes; row↔bytes drift of every kind:
F4 path-alias dedup, `DeleteArtifactsUnder`, `FindArtifactByPath` on work
delete, `RegisteredPathsUnder` list-dedup, upload-row-insert rollback, the
entire orphan "known set" sweeper, and the two parallel list/download/delete
code paths — replaced by one walker, one download handler, one delete
handler, over directories that mean what their names say.

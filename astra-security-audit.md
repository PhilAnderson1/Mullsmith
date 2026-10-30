# Astra security audit

Date: 2026-10-30. Scope: bubblewrap execution, AI filesystem tools, and host filesystem helpers/background cleanup that operate on script-controlled files. Source code was not modified.

## Assessment

Two high-priority host-side race conditions undermine the sandbox boundary: directory creation can follow a swapped parent symlink, and TTL cleanup can delete through one. Neither requires defeating bubblewrap itself: a script creates malicious filesystem state inside its writable bind mount and the host daemon subsequently acts on it with host privileges. Fix these before treating hostile code execution as securely isolated.

Findings below are source-review findings, not demonstrated end-to-end exploits. Severity assumes an attacker can execute code and race host filesystem operations. Host effects are limited by the daemon's OS permissions; running it as root substantially increases impact.

## 1. High — directory creation has a parent-symlink race

**Location:** `internal/scratch/path.go:269`, `MkdirAll`; reached from `CreateFile` at line 225 and therefore `fs_write`.

Unlike the file-open helpers, `MkdirAll` builds absolute strings, calls `os.Mkdir`, then checks each component with `os.Lstat`. Once an existing directory has passed its check, a script can rename it and replace it with a symlink before the next iteration. Both the next mkdir and its Lstat follow symlinks in intermediate components. Checking the final component does not protect its parents.

Example interleaving: the host checks `work/a`; the script replaces `work/a` with an absolute symlink to a host directory; the host creates `work/a/new`. The new directory is created outside the user's tree. The symlink target need not exist inside the jail: the host resolves it in its own namespace. Concurrent filesystem requests and a running script can overlap; the quota mutex does not constrain the script's syscalls.

The subsequent fd-based file open can reject the substituted symlink, but the external directory has already been created. This finding demonstrates an unauthorized directory-creation primitive, not by itself arbitrary file-content overwrite.

**Fix:** implement mkdir traversal with a trusted directory fd, `mkdirat`, and `openat(O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC)` for each component. Do not resume traversal through reconstructed absolute strings. Reject unexpected existing objects. Add a synchronized regression test replacing a checked parent before the next mkdir, asserting no outside directory is created even when the operation ultimately fails.

## 2. High — TTL sweeper can unlink outside the user's tree

**Location:** `internal/server/maintenance.go:177–253`, `sweepTTL` / `removeIfOld`; the same string-based removal helper is used by `sweepUnknownUser`.

`filepath.WalkDir` supplies path strings. `removeIfOld` then calls `os.Lstat(p)` and `os.Remove(p)`. The empty-directory cleanup also uses Lstat followed by Remove on absolute paths. Lstat avoids following the final symlink only; intermediate symlinks are still followed.

A script can prepare an old file at `work/a/victim`, allow the sweeper to discover it, then replace `work/a` with a symlink to an external host directory containing `victim`. Swapping after Lstat and before Remove bypasses even the external target's age check. The host unlink resolves the replacement parent and deletes the external entry. Alternatively, swapping a directory between WalkDir's classification and directory read can redirect the walk itself. Static symlink tests do not cover these races.

Impact includes deletion of other users' files or service files writable by the daemon, given the relevant names/paths and a successful race. This is an escape through the host cleanup process, despite the jail's read-only root.

**Fix:** enumerate and traverse using pinned directory fds with no-follow opens; inspect entries with `fstatat(AT_SYMLINK_NOFOLLOW)` and unlink relative to the same pinned parent using `unlinkat`. Apply this to file and directory cleanup and orphan scans. If exact-object age semantics matter, additionally coordinate mutation or use a controlled quarantine protocol; fd-relative parent resolution alone does not make a final-name age check atomic. Add parent-swap tests for file deletion, empty-directory deletion, and traversal.

## 3. Medium — launcher loses `--die-with-parent`

**Location:** `internal/runner/bwrap.go:128,258–262`; `internal/runner/sandbox.go`, final `syscall.Exec(s.Binary, s.Args, s.Env)`.

`bwrapArgs` starts with `--die-with-parent`. The launcher supplies that slice directly as execve's argv, whose first entry is the program name, not an option. Consequently bubblewrap receives `--die-with-parent` as argv[0] and does not process that option. The other flags remain arguments; this does not remove `--unshare-all` or disable the entire jail.

This defeats an explicitly intended lifecycle safeguard. Parent-death signaling and cgroup cleanup provide additional defenses, so surviving descendants after abrupt daemon death require runtime verification rather than being assumed. The startup probe uses `exec.CommandContext`, which constructs argv correctly, and therefore does not reproduce this discrepancy.

**Fix:** prepend `s.Binary` to `s.Args` when invoking `syscall.Exec`, keeping the spec's Args definition as arguments excluding argv[0]. Add a launcher argument-capture test and an integration test abruptly killing the server/launcher and checking that all descendants terminate.

## 4. Conditional medium — trusted roots and bind sources are not pinned

**Location:** `internal/scratch/path.go:130` (`openParent` initial open); `internal/scratch/scratch.go:98–109` (`EnsureUserDirs`); `internal/runner/bwrap.go:198–211,179–183` (Stat/lexical validation and bind sources).

The user-root open lacks O_NOFOLLOW; provisioning uses path-based MkdirAll; the runner follows symlinks with Stat and validates work-directory containment lexically before passing host path strings to bubblewrap. A pre-existing or concurrently substituted symlink at a user root or layout directory can therefore redirect host filesystem tools or bind an unintended host directory into the jail. `AdoptOwnership` also uses path-based Chown when run as root and should share the hardened provisioning boundary.

**Prerequisite:** control over layout entries or their parent outside the normal jail. The present jail mounts its root read-only and the tool write/delete guards refuse layout-directory targets, so this review did not establish a direct way for an ordinary script to replace these top-level entries. Treat this as a missing validation boundary and deployment/import/local-writer risk, not a proven remote escalation chain.

**Fix:** open the configured root once under a documented trusted-parent policy; open validated user IDs and layout directories relative to it with no-follow directory opens. Pin bind sources through descriptors supported by the launcher/bubblewrap integration, with deliberate fd lifetime management, rather than validate-then-reopen strings. Fail closed on symlink layouts. Use fd-based ownership changes.

## 5. Medium availability — unbounded host enumeration and soft disk limits

**Location:** `internal/scratch/path.go:323–329` (`ReadDir(-1)`); `internal/tools/files.go:114–129` (200-entry limit after loading everything); `internal/scratch/scratch.go` (`DirSize`, `DirSizeExceeds`); `internal/tools/codeexec.go` (`watchStorage`).

A script can create large numbers of empty files/directories that consume little counted file content but exhaust inodes and make host enumeration expensive. `fs_list` reads every entry before applying its response cap. Quota walks and RemoveTree also allocate/traverse without a firm entry budget. These operations run in the daemon, outside the run's cgroup memory ceiling. Polling disk usage and per-file RLIMIT_FSIZE do not provide a hard aggregate filesystem or inode limit; unlinked-but-open files also escape pathname-based byte accounting while consuming disk.

**Fix:** enforce per-user filesystem byte and inode/project quotas where available; put scratch storage on a separately bounded filesystem. Stream/paginate directory reads and impose traversal work/cancellation budgets. Treat incomplete accounting as unknown. Add inode-flood and large-list tests with bounded host memory/time expectations.

## Additional observations and hardening

- `RemoveTree` (`internal/scratch/path.go:397`) and quota scans use string-based WalkDir. They can traverse a concurrently substituted parent outside the tree. RemoveTree's eventual deletes go through the safer `Remove` helper, so this is not the same arbitrary-delete primitive as the sweeper. Convert enumeration to the same fd-based traversal to avoid external metadata access and accounting manipulation.
- Regular-file checks do not reject hard links. No normal-jail mechanism to hard-link an inaccessible host file was established; separate bind mounts also constrain linking. If externally populated scratch trees are supported, do not assume O_NOFOLLOW proves inode provenance. Prefer private filesystems/import-by-copy, and atomic replacement for writes; consider rejecting multiply linked files where compatible with product semantics.
- `allow_network` adds `--share-net`, exposing the host network including reachable loopback/private services. This is an explicit operator opt-in, not a default-network bypass. If the intended permission is Internet-only, use a separate network namespace with an enforced egress gateway rather than host networking.
- The mount specification exposes all of host `/usr` read-only. Read-only prevents modification, not disclosure of custom secrets placed there. Prefer a minimal immutable runtime image and document that the exposed runtime tree must contain no secrets.
- No application seccomp policy or nested-user-namespace restriction is installed by the shown launcher. Consider a tested syscall policy and disabling additional user namespaces where runtime compatibility permits. This is defense in depth against kernel attack surface, not evidence of a particular kernel exploit or a demonstrated namespace escape.

## Guards that are already effective in the reviewed paths

`ParseRel` rejects absolute paths, traversal segments, backslashes and control characters before I/O. User IDs are ULID-validated and supplied from the server invocation (`internal/server/api_chat.go:713–714`), rather than tool arguments. Intermediate and leaf opens generally use O_NOFOLLOW; file opens use O_NONBLOCK and fd-level regular-file checks, and writes check type before truncation. Normal deletes use unlinkat against a pinned parent. Write targets are normalized before enforcing work/artifacts restrictions. These protections should be preserved and extended to provisioning, mkdir, scans and maintenance.

The runner has no intended unjailed fallback, uses namespace isolation and a scrubbed payload environment, mounts uploads read-only, and sets no-new-privileges and resource limits. Per-run cgroups are armed before launch and startup/run paths refuse unavailable metering. These are substantial safeguards, but do not protect host code that follows attacker-controlled paths.

## Validation and limits

Ran `go test ./internal/scratch ./internal/tools ./internal/runner`. Scratch and tools passed (cached). The restricted initial runner invocation failed on namespace/socket permissions. An approved unrestricted rerun passed; `go test -v ./internal/runner` then established that most full-jail tests **skipped** because per-run cgroup placement was unavailable (`permission denied`). The simple bubblewrap startup probe and mount-spec tests passed. A passing package result is therefore not full runtime isolation verification.

No destructive exploit was run and no temporary exploit/test code was added. The concurrency findings are established from operation ordering and unsafe path resolution, but exploitation reliability was not measured. This review is not a kernel/bubblewrap vulnerability assessment or an exhaustive audit of all application endpoints. Run adversarial, synchronized race tests and full integration tests on a properly delegated disposable host after implementing the fixes.

Recommended order: fix TTL deletion and mkdir races first; correct argv handling; unify fd-based traversal and trusted-root/bind setup; enforce hard storage limits; then validate lifecycle and syscall/network hardening on the production-equivalent test host.

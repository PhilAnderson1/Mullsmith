# Third-Party Notices

Gabkeep is distributed as a single static Go binary. This file and the
`THIRD_PARTY_LICENSES/` directory accompany every release artifact and credit
the third-party modules whose code is linked into that binary, as their
licenses require when distributing compiled works.

`THIRD_PARTY_LICENSES/` mirrors Go module paths: each module keeps its
original license file names verbatim, so a module whose licensing spans
several files (e.g. `modernc.org/sqlite`, whose bundled SQLite is public
domain) ships several files. Where a module embeds further upstreams, its own
`LICENSE-3RD-PARTY.md` accounts for them and is included as-is.

The set below is the complete direct + indirect module graph of `go.mod`, so
it stays honest across platforms even though release builds are linux/amd64.

| Module | Version | Licence | Role in Gabkeep |
|---|---|---|---|
| `golang.org/x/crypto` | v0.58.0 | BSD-3-Clause | argon2id password hashing (spec §9.1) |
| `golang.org/x/sys` | v0.49.0 | BSD-3-Clause | raw unix syscalls behind the scratchpad path sandbox (§9.7) |
| `golang.org/x/term` | v0.47.0 | BSD-3-Clause | hidden password prompt in `admin create-user` |
| `gopkg.in/yaml.v3` | v3.0.1 | MIT + Apache-2.0 (dual — see its `LICENSE` and `NOTICE`) | configuration file parsing (§2) |
| `modernc.org/sqlite` | v1.61.0 | BSD-3-Clause; bundled SQLite is public domain, bundled sqlite_vec MIT | pure-Go SQLite driver (keeps builds CGO-free) |
| `modernc.org/libc` | v1.79.0 | BSD-3-Clause (+ its third-party notices) | C runtime transpiled to Go, required by the SQLite driver |
| `modernc.org/mathutil` | v1.7.1 | BSD-3-Clause | math support for `libc` |
| `modernc.org/memory` | v1.12.1 | BSD-3-Clause (+ Go and mmap-go notices) | memory management for `libc` |
| `github.com/dustin/go-humanize` | v1.1.0 | MIT | human-readable sizes used by the SQLite driver |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause | UUID generation used by the SQLite driver |
| `github.com/mattn/go-isatty` | v0.0.24 | MIT | terminal detection (platform-dependent driver input) |
| `github.com/ncruces/go-strftime` | v1.1.0 | MIT | SQLite `strftime` support |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause | big-integer arithmetic for `mathutil` |

The Go toolchain and standard library — also BSD-3-Clause — are statically
linked into the binary; their licence is part of the Go distribution
(<https://go.dev/LICENSE>) and is not duplicated here.

The web UI contains no third-party JavaScript or CSS: the app is hand-written
vanilla ES modules and CSS with no build step, and icons are inline SVG.
Fonts are the system stack. Nothing in `web/` carries upstream licence files.

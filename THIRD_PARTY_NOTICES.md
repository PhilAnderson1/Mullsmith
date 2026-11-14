# Third-Party Notices

Mullsmith is distributed as a single static Go binary. This file and the
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

| Module | Version | Licence | Role in Mullsmith |
|---|---|---|---|
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | Apache-2.0 and MIT (licensing transition; see its `LICENSE`) | MCP protocol client and Streamable HTTP/stdio transports |
| `github.com/google/jsonschema-go` | v0.4.3 | MIT | JSON Schema handling used by the MCP SDK |
| `github.com/segmentio/asm` | v1.1.3 | MIT | optimized JSON support used by the MCP SDK |
| `github.com/segmentio/encoding` | v0.5.4 | MIT | bounded JSON decoding used by the MCP SDK |
| `github.com/yosida95/uritemplate/v3` | v3.0.2 | BSD-3-Clause | URI template handling used by the MCP SDK |
| `golang.org/x/oauth2` | v0.35.0 | BSD-3-Clause | MCP SDK authentication support (not exposed in Stage 1) |
| `golang.org/x/sync` | v0.24.0 | BSD-3-Clause | concurrency support used by the MCP SDK |
| `golang.org/x/time` | v0.15.0 | BSD-3-Clause | rate support used by the MCP SDK |
| `golang.org/x/crypto` | v0.58.0 | BSD-3-Clause | argon2id password hashing (spec §9.1) |
| `golang.org/x/sys` | v0.49.0 | BSD-3-Clause | raw unix syscalls behind the scratchpad path sandbox (§9.7) |
| `golang.org/x/term` | v0.47.0 | BSD-3-Clause | hidden password prompt in `admin create-user` |
| `gopkg.in/yaml.v3` | v3.0.1 | MIT + Apache-2.0 (dual — see its `LICENSE` and `NOTICE`) | configuration file parsing (§2) |
| `modernc.org/sqlite` | v1.61.1 | BSD-3-Clause; bundled SQLite is public domain, bundled sqlite_vec MIT | pure-Go SQLite driver (keeps builds CGO-free) |
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

The web UI bundles **KaTeX v0.19.1** (MIT) for math rendering, including its
JavaScript module, stylesheet, and WOFF2 fonts. Its license is included at
`web/assets/vendor/katex-0.19.1/LICENSE` and embedded in the executable.
The stylesheet is reduced to reference the bundled WOFF2 fonts only.
Source: <https://github.com/KaTeX/KaTeX/releases/tag/v0.19.1>.
The rest of the UI uses hand-written ES modules, CSS, and inline SVG with
no build step. All math assets are served locally; no CDN is required.

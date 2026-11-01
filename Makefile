# Mullsmith build. Version stamping: -version prints these values.
# CGO is never required (pure-Go sqlite), so builds are portable static binaries.

VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildDate=$(DATE)

.PHONY: build test test-js race vet lint vuln fuzz-short check dist

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/mullsmith ./cmd/mullsmith

test:
	go test ./...

# Executable client checks (finding 8): pure ES modules driven by node's
# built-in test runner — zero dependencies, no bundler (spec §10 stays
# true). The Go appjs tests remain for static asset contracts only.
test-js:
	node --test "web/test/*.test.mjs"

race:
	go test -race ./...

vet:
	go vet ./...

# gosec must report no issues (§9.15 release gate); suppressions are per-line
# "#nosec RULE -- justification" comments, which gosec honors by default.
lint:
	gosec ./...

vuln:
	govulncheck ./...

# Short bounded fuzzing of the scratchpad path sandbox (§9.7). CI can raise
# -fuzztime; the seed corpus lives in internal/scratch.
fuzz-short:
	go test ./internal/scratch -run '^$$' -fuzz 'FuzzPath' -fuzztime 60s

check: vet race lint vuln

# Release artifact: single static binary stamped with version/commit/date.
dist: build
	@mkdir -p dist
	@cp bin/mullsmith dist/mullsmith-$(VERSION)-linux-amd64
	@sha256sum dist/mullsmith-$(VERSION)-linux-amd64 | tee dist/mullsmith-$(VERSION)-linux-amd64.sha256
	@echo "dist/mullsmith-$(VERSION)-linux-amd64 (+ .sha256)"

# Gabkeep build. Version stamping: -version prints these values.
# CGO is never required (pure-Go sqlite), so builds are portable static binaries.

VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildDate=$(DATE)

.PHONY: build test race vet lint vuln fuzz-short check dist

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o gabkeep ./cmd/gabkeep

test:
	go test ./...

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
	@cp gabkeep dist/gabkeep-$(VERSION)-linux-amd64
	@sha256sum dist/gabkeep-$(VERSION)-linux-amd64 | tee dist/gabkeep-$(VERSION)-linux-amd64.sha256
	@echo "dist/gabkeep-$(VERSION)-linux-amd64 (+ .sha256)"

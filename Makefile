PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
BINARY := kenogram
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/idolum-ai/kenogram/internal/version.Version=$(VERSION) -X github.com/idolum-ai/kenogram/internal/version.Commit=$(COMMIT) -X github.com/idolum-ai/kenogram/internal/version.Date=$(DATE)
GOCACHE ?= /tmp/kenogram-go-build
GOMODCACHE ?= /tmp/kenogram-go-mod

.PHONY: build release-dist release-smoke install install-release uninstall test test-evidence test-race integration e2e e2e-release e2e-openclaw e2e-composition e2e-telegram-canary vet check architecture stdlib-only docs-freshness secrets workflow-sanity smoke fmt

build:
	mkdir -p bin
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go build -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/kenogram

release-dist:
	@if [ "$(VERSION)" = "dev" ]; then echo "VERSION=vX.Y.Z is required" >&2; exit 2; fi
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) RELEASE_COMMIT=$(COMMIT) RELEASE_DATE=$(DATE) ./scripts/package-release.sh "$(VERSION)" dist

release-smoke:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) ./scripts/check-release.sh

install: build
	mkdir -p $(BINDIR)
	install -m 0755 bin/$(BINARY) $(BINDIR)/$(BINARY)

install-release:
	@if [ "$(VERSION)" = "dev" ]; then ./scripts/install-release.sh; else ./scripts/install-release.sh "$(VERSION)"; fi

uninstall:
	rm -f $(BINDIR)/$(BINARY)

test:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./...

test-evidence:
	mkdir -p artifacts
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test -json ./... > artifacts/test.json
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test -coverprofile=artifacts/coverage.out ./...

test-race:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test -race ./...

vet:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go vet ./...

fmt:
	test -z "$$(gofmt -l cmd internal)"

check: fmt test vet build release-smoke architecture stdlib-only docs-freshness secrets workflow-sanity smoke

architecture:
	bash scripts/check-architecture.sh

stdlib-only:
	bash scripts/check-stdlib-only.sh

docs-freshness:
	bash scripts/check-docs-freshness.sh

secrets:
	bash scripts/check-secrets.sh

workflow-sanity:
	bash scripts/check-workflows.sh

smoke: build
	bash scripts/smoke.sh

integration:
	KENOGRAM_INTEGRATION=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/integration -count=1 -timeout=5m -v

e2e: e2e-release e2e-openclaw e2e-composition

e2e-release:
	KENOGRAM_E2E=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/e2e -run TestEngramReleaseInsideKenogram -count=1 -timeout=10m -v

e2e-openclaw:
	KENOGRAM_OPENCLAW_E2E=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/e2e -run TestOpenClawTUIInsideKenogram -count=1 -timeout=16m -v

e2e-composition:
	KENOGRAM_ENGRAM_OPENCLAW_E2E=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/e2e -run TestEngramControlsOpenClawInsideKenogram -count=1 -timeout=16m -v

e2e-telegram-canary:
	KENOGRAM_LIVE_TELEGRAM=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/e2e -run TestLiveTelegramOpenClawCanary -count=1 -timeout=19m -v

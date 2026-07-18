PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
BINARY := kenogram
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/idolum-ai/kenogram/internal/version.Version=$(VERSION) -X github.com/idolum-ai/kenogram/internal/version.Commit=$(COMMIT) -X github.com/idolum-ai/kenogram/internal/version.Date=$(DATE)
GOCACHE ?= /tmp/kenogram-go-build
GOMODCACHE ?= /tmp/kenogram-go-mod
GOVULNCHECK_VERSION := v1.6.0

.PHONY: build release-dist release-smoke install install-release uninstall test test-evidence test-race integration proof-readiness e2e e2e-ssh e2e-release e2e-openclaw e2e-composition e2e-hermes e2e-hermes-composition e2e-telegram-canary vet vulncheck check architecture stdlib-only docs-freshness secrets workflow-sanity smoke fmt cross-apple-machine

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
	@GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test -json ./... > artifacts/test.json || { status=$$?; cat artifacts/test.json; exit $$status; }
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test -coverprofile=artifacts/coverage.out ./...

test-race:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test -race ./...

vet:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go vet ./...

vulncheck:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

fmt:
	test -z "$$(gofmt -l cmd internal)"

check: fmt test vet build release-smoke cross-apple-machine architecture stdlib-only docs-freshness secrets workflow-sanity smoke

cross-apple-machine:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) GOOS=darwin GOARCH=arm64 go build -buildvcs=false ./...

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

proof-readiness:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/app -run '^(TestReadinessWrapperSemanticReference|TestReadinessSuccessBeforeCommitRecoversThenExplicitlyReruns)$$' -count=1 -timeout=30s -v

e2e: e2e-ssh e2e-release e2e-openclaw e2e-composition e2e-hermes e2e-hermes-composition

e2e-ssh:
	KENOGRAM_SSH_E2E=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/e2e -run TestSSHComposition -count=1 -timeout=14m -v

e2e-release:
	KENOGRAM_E2E=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/e2e -run TestEngramReleaseInsideKenogram -count=1 -timeout=12m -v

e2e-openclaw:
	KENOGRAM_OPENCLAW_E2E=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/e2e -run TestOpenClawInsideKenogram -count=1 -timeout=17m -v

e2e-composition:
	KENOGRAM_ENGRAM_OPENCLAW_E2E=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/e2e -run TestEngramControlsOpenClawInsideKenogram -count=1 -timeout=18m -v

e2e-hermes:
	KENOGRAM_HERMES_E2E=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/e2e -run TestHermesInsideKenogram -count=1 -timeout=21m -v

e2e-hermes-composition:
	KENOGRAM_ENGRAM_HERMES_E2E=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/e2e -run TestEngramControlsHermesInsideKenogram -count=1 -timeout=21m -v

e2e-telegram-canary:
	KENOGRAM_LIVE_TELEGRAM=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/e2e -run TestLiveTelegramOpenClawCanary -count=1 -timeout=21m -v

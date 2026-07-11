PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
BINARY := kenogram
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/idolum-ai/kenogram/internal/version.Version=$(VERSION) -X github.com/idolum-ai/kenogram/internal/version.Commit=$(COMMIT) -X github.com/idolum-ai/kenogram/internal/version.Date=$(DATE)
GOCACHE ?= /tmp/kenogram-go-build
GOMODCACHE ?= /tmp/kenogram-go-mod

.PHONY: build install uninstall test test-race integration vet check architecture stdlib-only docs-freshness secrets smoke fmt

build:
	mkdir -p bin
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/kenogram

install: build
	mkdir -p $(BINDIR)
	install -m 0755 bin/$(BINARY) $(BINDIR)/$(BINARY)

uninstall:
	rm -f $(BINDIR)/$(BINARY)

test:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./...

test-race:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test -race ./...

vet:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go vet ./...

fmt:
	test -z "$$(gofmt -l cmd internal)"

check: fmt test vet build architecture stdlib-only docs-freshness secrets smoke

architecture:
	bash scripts/check-architecture.sh

stdlib-only:
	bash scripts/check-stdlib-only.sh

docs-freshness:
	bash scripts/check-docs-freshness.sh

secrets:
	bash scripts/check-secrets.sh

smoke: build
	bash scripts/smoke.sh

integration:
	KENOGRAM_INTEGRATION=1 GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./internal/integration -count=1 -v

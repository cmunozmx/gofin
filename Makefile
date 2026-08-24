# gofin — build automation
#
# Run `make` or `make help` for the list of targets.

SHELL      := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
MAKEFLAGS  += --no-print-directory
.DEFAULT_GOAL := help

# ------------------------------------------------------------------ config

MODULE  := $(shell go list -m)
BINARY  := gofin
PKG     := ./cmd/$(BINARY)
BIN_DIR := bin
DIST_DIR:= dist
BIN     := $(BIN_DIR)/$(BINARY)

# Version stamping. Falls back cleanly outside a git checkout.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# -s -w strips the symbol table and DWARF; the binary is ~30% smaller and
# nothing here needs a stack trace with local variable names.
#
# To stamp version metadata into the binary, change `const Version` in
# cmd/gofin/main.go to a `var` and add:
#   -X '$(MODULE)/cmd/$(BINARY).Version=$(VERSION)'
# `go build -ldflags -X` cannot write to a const, it fails silently.
LDFLAGS ?= -s -w

# -trimpath keeps absolute build paths out of the binary, which makes builds
# reproducible across machines.
GO_BUILD_FLAGS ?= -trimpath -ldflags "$(LDFLAGS)"

GO_TEST_FLAGS  ?= -race -count=1
COVER_PROFILE  := coverage.out

# The tree is stdlib-only; CGO is off so the result is a static binary that
# runs in a scratch container. Note this is per-recipe: `make test` needs CGO
# for the race detector.
export CGO_ENABLED

GO_FILES := $(shell find . -type f -name '*.go' -not -path './vendor/*')

# Cross-compilation matrix for `make dist`.
#PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
PLATFORMS ?= linux/amd64

# ------------------------------------------------------------------- build

## build: compile the binary into bin/
build: $(BIN)

$(BIN): $(GO_FILES) go.mod
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o $@ $(PKG)
	@echo "built $@ ($(VERSION))"

## install: install the binary into $GOPATH/bin
install:
	CGO_ENABLED=0 go install $(GO_BUILD_FLAGS) $(PKG)

## run: build and run the server (ARGS="serve --log-level debug")
run: $(BIN)
	$(BIN) $(or $(ARGS),serve --log-level debug)

## dist: cross-compile release archives into dist/
dist: clean-dist
	@mkdir -p $(DIST_DIR)
	@for platform in $(PLATFORMS); do \
	  os=$${platform%/*}; arch=$${platform#*/}; \
	  ext=""; [ "$$os" = windows ] && ext=".exe"; \
	  out="$(DIST_DIR)/$(BINARY)_$(VERSION)_$${os}_$${arch}$${ext}"; \
	  echo "  $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build $(GO_BUILD_FLAGS) -o "$$out" $(PKG); \
	done
	@echo "wrote $(DIST_DIR)/"

# -------------------------------------------------------------------- test

## test: run all tests with the race detector
test:
	go test $(GO_TEST_FLAGS) ./...

## test-short: run tests without the race detector (fast)
test-short:
	go test -count=1 -short ./...

## cover: run tests and report coverage per function
cover:
	go test -covermode=atomic -coverprofile=$(COVER_PROFILE) ./...
	go tool cover -func=$(COVER_PROFILE) | tail -n 20

## cover-html: open the coverage report in a browser
cover-html: cover
	go tool cover -html=$(COVER_PROFILE)

## bench: run benchmarks
bench:
	go test -run '^$$' -bench . -benchmem ./...

# ------------------------------------------------------------------ checks

## fmt: format all Go source in place
fmt:
	gofmt -s -w $(GO_FILES)

## fmt-check: fail if any file is not gofmt-clean
fmt-check:
	@out=$$(gofmt -s -l $(GO_FILES)); \
	if [ -n "$$out" ]; then \
	  echo "not gofmt-clean:"; echo "$$out"; exit 1; \
	fi
	@echo "gofmt: clean"

## vet: run go vet
vet:
	go vet ./...

## lint: run golangci-lint, or fall back to staticcheck / vet
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
	  golangci-lint run ./...; \
	elif command -v staticcheck >/dev/null 2>&1; then \
	  echo "golangci-lint not found, using staticcheck"; staticcheck ./...; \
	else \
	  echo "no linter installed, falling back to go vet (make tools)"; go vet ./...; \
	fi

## tidy: tidy and verify go.mod / go.sum
tidy:
	go mod tidy
	go mod verify

## tidy-check: fail if go.mod or go.sum would change
tidy-check:
	@cp go.mod go.mod.bak; [ -f go.sum ] && cp go.sum go.sum.bak || true; \
	go mod tidy; \
	status=0; \
	diff -q go.mod go.mod.bak >/dev/null || { echo "go.mod is not tidy"; status=1; }; \
	if [ -f go.sum.bak ] && ! diff -q go.sum go.sum.bak >/dev/null; then \
	  echo "go.sum is not tidy"; status=1; \
	fi; \
	mv go.mod.bak go.mod; [ -f go.sum.bak ] && mv go.sum.bak go.sum || true; \
	[ $$status -eq 0 ] && echo "go.mod: tidy"; exit $$status

## vuln: scan dependencies and stdlib for known vulnerabilities
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## check: fmt-check, vet, lint and test — what CI runs
check: fmt-check vet lint test

## all: check then build
all: check build

# ------------------------------------------------------------------- tools

## tools: install the optional development tools
tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go install honnef.co/go/tools/cmd/staticcheck@latest

# ------------------------------------------------------------------- clean

## clean: remove build artifacts and test caches
clean: clean-dist
	rm -rf $(BIN_DIR) $(COVER_PROFILE)
	go clean -testcache

clean-dist:
	rm -rf $(DIST_DIR)

# -------------------------------------------------------------------- help

## help: show this message
help:
	@echo "$(MODULE) — $(VERSION)"
	@echo
	@echo "Targets:"
	@sed -n 's/^## \(.*\): \(.*\)/  \1|\2/p' $(MAKEFILE_LIST) | column -t -s '|'
	@echo
	@echo "Variables: VERSION, LDFLAGS, GO_TEST_FLAGS, PLATFORMS, ARGS"

.PHONY: all build install run dist test test-short cover cover-html bench \
        fmt fmt-check vet lint tidy tidy-check vuln check tools \
        clean clean-dist help

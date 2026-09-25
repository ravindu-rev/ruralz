# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Each CI stage calls one target (docs/engineering/02-repository-layout-and-conventions.md).
# Before opening a pull request run: make hygiene lint generate build test supply-chain

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

MODULE := github.com/ravindu-rev/ruralz
BIN := $(CURDIR)/bin
DIST := $(CURDIR)/dist
GOLANGCI_LINT := $(BIN)/golangci-lint
GOVULNCHECK := $(BIN)/govulncheck

BINARIES := ruralzd ruralz-control ruralz
# Production platforms of every binary, and the extra CLI platforms (ADR-0001).
PLATFORMS := linux/amd64 linux/arm64
CLI_PLATFORMS := darwin/arm64 darwin/amd64 windows/amd64

# Build metadata embedded with -ldflags -X and printed by `ruralz version`.
# There is no build date, so rebuilding a tag reproduces identical binaries.
VERSION ?= $(shell v=$$(git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null) && echo "$${v#v}" || echo 0.0.0-dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
FLAVOR ?= default
LDFLAGS := -X $(MODULE)/internal/buildinfo.version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.flavor=$(FLAVOR)

# The floor job runs the latest Go 1.26 patch. CI installs it and keeps
# GOTOOLCHAIN=local; elsewhere pass FLOOR_GOTOOLCHAIN=go1.26.8 (or newer 1.26 patch).
FLOOR_GOTOOLCHAIN ?= local

.PHONY: help tools hygiene lint fmt generate build test floor supply-chain clean

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'

tools: $(GOLANGCI_LINT) $(GOVULNCHECK) ## Install the pinned, checksum-verified CI tools into bin/

$(GOLANGCI_LINT): scripts/install-tools.sh
	bash scripts/install-tools.sh golangci-lint

$(GOVULNCHECK): scripts/install-tools.sh
	bash scripts/install-tools.sh govulncheck

hygiene: ## Stage 1: repocheck and the no-license-check scan; DCO_RANGE and PR_TITLE add the commit checks
	go run ./internal/tool/repocheck
	if [[ -n "$${DCO_RANGE:-}" ]]; then go run ./internal/tool/commitcheck dco -range "$$DCO_RANGE"; fi
	if [[ -n "$${PR_TITLE:-}" ]]; then go run ./internal/tool/commitcheck title; fi

lint: $(GOLANGCI_LINT) ## Stage 2: gofumpt, goimports and golangci-lint
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) fmt --diff ./...
	$(GOLANGCI_LINT) run ./...

fmt: $(GOLANGCI_LINT) ## Rewrite files with gofumpt and goimports
	$(GOLANGCI_LINT) fmt ./...

generate: ## Stage 3: regenerate the JSON Schema and fail on any difference
	go generate ./...
	git diff --exit-code -- api/schema/ruralz || { echo "generated files are stale: commit the output of 'make generate'" >&2; exit 1; }
	untracked=$$(git ls-files --others --exclude-standard -- api/schema/ruralz); \
	if [[ -n "$$untracked" ]]; then echo "untracked generated files: $$untracked" >&2; exit 1; fi

build: ## Stage 4 (G1): cross-build with CGO_ENABLED=0 and -trimpath
	for bin in $(BINARIES); do for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=$(DIST)/$${os}_$${arch}/$$bin; \
		echo "build $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$out ./cmd/$$bin; \
	done; done
	for p in $(CLI_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [[ $$os == windows ]] && ext=.exe; \
		out=$(DIST)/$${os}_$${arch}/ruralz$$ext; \
		echo "build $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$out ./cmd/ruralz; \
	done

test: ## Stage 5: race tests with cgo, then the shipped CGO_ENABLED=0 configuration
	CGO_ENABLED=1 go test -race -shuffle=on -tags netgo,osusergo -cover ./...
	CGO_ENABLED=0 go test -shuffle=on ./...

floor: ## Floor job: build and test on Go 1.26 with GOEXPERIMENT=jsonv2
	export GOTOOLCHAIN=$(FLOOR_GOTOOLCHAIN) GOEXPERIMENT=jsonv2 CGO_ENABLED=0; \
	v=$$(go env GOVERSION); \
	if [[ $$v != go1.26.* ]]; then echo "floor: want Go 1.26, got $$v; set FLOOR_GOTOOLCHAIN" >&2; exit 1; fi; \
	echo "floor: $$v"; \
	go build ./... && go test -shuffle=on ./...

supply-chain: $(GOVULNCHECK) ## Stage 6: go mod verify, govulncheck, G2, G3, Raft denylist, advisory floors
	go mod verify
	$(GOVULNCHECK) ./...
	go run ./internal/tool/depgate

clean: ## Remove build output
	rm -rf $(DIST)

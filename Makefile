# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# The PR pipeline (.github/workflows/pr.yml,
# docs/engineering/02-repository-layout-and-conventions.md, "CI stages")
# runs these targets, and `make help` lists them. Before opening a pull
# request run what it runs:
#   make lint dco supply-chain test integration
# make integration needs a local redis-server, and Docker for the State
# Store container kinds.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

MODULE := github.com/ravindu-rev/ruralz
BIN := $(CURDIR)/bin
DIST := $(CURDIR)/dist
GOLANGCI_LINT := $(BIN)/golangci-lint
GOVULNCHECK := $(BIN)/govulncheck
ACTIONLINT := $(BIN)/actionlint
SHELLCHECK := $(BIN)/shellcheck
PROMTOOL := $(BIN)/promtool
COSIGN := $(BIN)/cosign
SYFT := $(BIN)/syft

BINARIES := ruralzd ruralz-control ruralz
# Production platforms of every binary, and the extra CLI platforms (ADR-0001).
PLATFORMS := linux/amd64 linux/arm64
CLI_PLATFORMS := darwin/arm64 darwin/amd64 windows/amd64
# Development-only ruralzd platforms: release-dry-run builds them for
# the darwin server archives; make build does not.
SERVER_DEV_PLATFORMS := darwin/arm64 darwin/amd64

# Build metadata embedded with -ldflags -X and printed by `ruralz version`.
# There is no build date, so rebuilding a tag reproduces identical binaries.
# No "#" inside a function call: GNU Make 3.81, the make of macOS, reads it
# as the start of a comment.
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null || echo v0.0.0-dev))
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
FLAVOR ?= default
# The release flags of make build and the release targets. -s -w drop the
# symbol table and DWARF; stack traces and pprof keep working through
# pclntab, and rebuilds stay reproducible.
LDFLAGS := -s -w \
	-X $(MODULE)/internal/buildinfo.version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.flavor=$(FLAVOR)

# make dco checks the non-merge commits of DCO_RANGE, by default the
# branch's commits that origin/main lacks. The PR pipeline passes the
# change's range; recipes read it from the environment and never splice
# it into the shell command line.
DCO_RANGE ?= origin/main..HEAD
export DCO_RANGE

# make integration. RURALZ_TEST_STATESTORE=process|container and
# RURALZ_TEST_REDIS_SERVER select the State Store flavors
# (docs/engineering/03-testing-and-quality-strategy.md, "Integration
# tests"); INTEGRATION_TIMEOUT bounds each package.
INTEGRATION_TIMEOUT ?= 25m

# The release. NOTICES_DIR receives depgate's per-artifact NOTICE and
# THIRD_PARTY_LICENSES, which the archives and the image ship; IMAGE_DIR
# the image build's OCI layout and SBOM_DIR Syft's scan of each platform
# image. RELEASE_PART=image runs steps 1 and 2 (binaries, notices, image)
# and RELEASE_PART=package steps 3 to 5 on what an image part left in
# dist/, as the image and package jobs of release-build.yml do; empty runs
# every step. An empty NOTES_FROM starts the notes at the newest vX.Y.Z tag
# before NOTES_TO, never a release candidate's, or at the repository root
# for the first release (root names it too). SKIPPED_GATES names the
# ruralz.skippedgates.v1 file the notes read (empty: no gate skipped), and
# BENCH_RECORDS the benchmark records the package step archives. These
# inputs can come from workflow inputs, so recipes read them from the
# environment.
RELEASE_DIR ?= $(DIST)/release
NOTICES_DIR ?= $(DIST)/notices
IMAGE_DIR ?= $(DIST)/image
SBOM_DIR ?= $(DIST)/sbom
BENCH_RECORDS ?= $(DIST)/bench
IMAGE_REPOSITORY := ghcr.io/ravindu-rev/ruralzd
IMAGE_PLATFORMS := linux/amd64 linux/arm64
RELEASE_PART ?=
NOTES_FROM ?=
NOTES_TO ?= HEAD
SKIPPED_GATES ?=
export NOTES_FROM NOTES_TO SKIPPED_GATES

# TEST_FLAGS adds go test flags to the integration suites, such as -run X
# -v.
TEST_FLAGS ?=

.PHONY: help tools lint fmt dco generate build test supply-chain integration \
	release-dry-run release release-image release-package clean

help: ## List targets
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

tools: $(GOLANGCI_LINT) $(GOVULNCHECK) $(ACTIONLINT) $(SHELLCHECK) $(PROMTOOL) ## Install the pinned, checksum-verified developer tools into bin/

$(GOLANGCI_LINT): scripts/install-tools.sh
	bash scripts/install-tools.sh golangci-lint

$(GOVULNCHECK): scripts/install-tools.sh
	bash scripts/install-tools.sh govulncheck

$(ACTIONLINT): scripts/install-tools.sh
	bash scripts/install-tools.sh actionlint

$(SHELLCHECK): scripts/install-tools.sh
	bash scripts/install-tools.sh shellcheck

$(PROMTOOL): scripts/install-tools.sh
	bash scripts/install-tools.sh promtool

lint: $(GOLANGCI_LINT) $(ACTIONLINT) $(SHELLCHECK) ## golangci-lint with .golangci.yml (config check, gofumpt and goimports diff, linters), then actionlint with the pinned shellcheck on the workflows
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) fmt --diff ./...
	$(GOLANGCI_LINT) run ./...
	$(ACTIONLINT) -shellcheck=$(SHELLCHECK)

fmt: $(GOLANGCI_LINT) ## Rewrite files with gofumpt and goimports
	$(GOLANGCI_LINT) fmt ./...

dco: ## DCO sign-off of every non-merge commit of DCO_RANGE (default origin/main..HEAD), Dependabot's commits aside
	go run ./internal/tool/commitcheck dco -range "$$DCO_RANGE"

# Generated trees: the JSON Schema and the served-feature table
# (schemagen), and the alert rules and dashboards generated from the
# telemetry catalog (telemetrygen). The unit tests fail on a stale copy.
GENERATED := api/schema/ruralz internal/config/registry/planned_gen.go deploy/grafana

generate: ## Regenerate the JSON Schema, served-feature table, alert rules and dashboards, and fail where they differ from the index
	go generate ./...
	git diff --exit-code -- $(GENERATED) || { echo "generated files are stale: commit the output of 'make generate'" >&2; exit 1; }
	untracked=$$(git ls-files --others --exclude-standard -- $(GENERATED)); \
	if [[ -n "$$untracked" ]]; then echo "untracked generated files: $$untracked" >&2; exit 1; fi

build: ## Cross-build every binary for linux/amd64 and linux/arm64 and the CLI for darwin and windows, with CGO_ENABLED=0, -trimpath and the release -ldflags (G1)
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

test: ## Unit tests: the race tests with cgo, then the shipped CGO_ENABLED=0 configuration
	CGO_ENABLED=1 go test -race -shuffle=on -tags netgo,osusergo -cover ./...
	CGO_ENABLED=0 go test -shuffle=on ./...

supply-chain: $(GOVULNCHECK) ## go mod verify, govulncheck, then depgate: licenses (G2), crypto (G3), the ruralzd Raft and ruralz jwx denylists, no test support in a binary, advisory floors
	go mod verify
	$(GOVULNCHECK) ./...
	go run ./internal/tool/depgate

# The promtool of bin/ comes first on PATH for the alert rule tests.
integration: $(PROMTOOL) ## Integration tests (tag integration): the conformance suites, the State Store, CLI and Node suites and the alert rule tests
	PATH="$(BIN):$$PATH" CGO_ENABLED=1 go test -race -shuffle=on -tags integration,netgo,osusergo -timeout $(INTEGRATION_TIMEOUT) $(TEST_FLAGS) ./...

# Stage 12 (docs/engineering/04-release-versioning-and-compatibility.md,
# "Packaging and audit"), in two parts that release-dry-run and release
# run in order, or one of them with RELEASE_PART:
#   image    the binaries of every release platform (ruralzd also for
#            darwin, development only); depgate -notices DIR, the stage 6
#            gates, then each artifact's NOTICE and THIRD_PARTY_LICENSES in
#            DIR/<binary>_<os>_<arch>/; then, when Docker is present, the
#            ruralzd image for linux/amd64 and linux/arm64 from
#            deploy/container/Dockerfile with SOURCE_DATE_EPOCH at the commit
#            time, timestamps rewritten and BuildKit's provenance and SBOM
#            off, into the OCI layout IMAGE_DIR, which releasekit image
#            checks. release also pushes it untagged by digest, checks that
#            the registry holds the layout's index digest and signs that
#            digest with cosign (keyless, the OIDC identity of
#            release-build.yml). Without Docker the dry run records no image.
#   package  releasekit package (archives, schema files, the benchmark
#            records archive of BENCH_RECORDS, release-manifest.json with
#            the image digests); Syft's SBOM of each archive and scan of
#            each platform image, which releasekit sbom merges into the
#            image SBOM; releasekit notes, releasekit checksums, releasekit
#            verify of the directory and the image, and sha256sum -c.
#            release then signs SHA256SUMS with cosign and audits again.
# Before package, $(DIST)/<os>_<arch>/ holds ruralzd for linux/amd64,
# linux/arm64, darwin/arm64 and darwin/amd64 (development only), ruralz
# for those and windows/amd64 (ruralz.exe), and ruralz-control for linux,
# which is the M2 stub: package archives no ruralz-control until M2.
release_parts = $(if $(RELEASE_PART),release-$(RELEASE_PART),release-image release-package)

release-dry-run: ## Stage 12 without signing: binaries, notices, image (with Docker), archives, SBOMs, notes, checksums, audit; RELEASE_PART=image|package runs one part
	case "$(RELEASE_PART)" in "" | image | package) ;; *) echo "release: RELEASE_PART=$(RELEASE_PART) is not image or package" >&2; exit 2 ;; esac
	$(MAKE) --no-print-directory -j1 $(release_parts) RELEASE_MODE=dry-run

release: ## Stage 12: the dry run's steps with the image pushed untagged by digest, cosign signing the image digest and SHA256SUMS, and a second audit; RELEASE_PART as for release-dry-run
	case "$(RELEASE_PART)" in "" | image | package) ;; *) echo "release: RELEASE_PART=$(RELEASE_PART) is not image or package" >&2; exit 2 ;; esac
	$(MAKE) --no-print-directory -j1 $(release_parts) RELEASE_MODE=release

# Steps 1 and 2 of stage 12; run through release-dry-run or release.
release-image: build
	for p in $(SERVER_DEV_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=$(DIST)/$${os}_$${arch}/ruralzd; \
		echo "build $$out (development only)"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$out ./cmd/ruralzd; \
	done
	rm -rf $(NOTICES_DIR) $(IMAGE_DIR) $(DIST)/image-metadata.json
	go run ./internal/tool/depgate -notices $(NOTICES_DIR)
	if ! docker info >/dev/null 2>&1 || ! docker buildx version >/dev/null 2>&1; then \
		if [[ "$(RELEASE_MODE)" == release ]]; then echo "release: no Docker daemon or buildx: the release pushes its image" >&2; exit 1; fi; \
		echo "release-dry-run: skip the image: no Docker daemon or buildx, so no image is recorded, which package refuses for a release or candidate"; \
		exit 0; \
	fi; \
	epoch=$$(git log -1 --format=%ct $(COMMIT)); \
	builder=; \
	if [[ $$(docker buildx inspect 2>/dev/null | awk '/^Driver:/ { print $$2; exit }') == docker ]]; then \
		docker buildx inspect ruralz-release >/dev/null 2>&1 || docker buildx create --name ruralz-release --driver docker-container >/dev/null; \
		builder="--builder ruralz-release"; \
	fi; \
	outputs=(--output "type=oci,dest=$(IMAGE_DIR),tar=false,rewrite-timestamp=true"); \
	if [[ "$(RELEASE_MODE)" == release ]]; then \
		outputs+=(--output "type=image,name=$(IMAGE_REPOSITORY),push-by-digest=true,name-canonical=true,push=true,oci-mediatypes=true,rewrite-timestamp=true"); \
	fi; \
	platforms=$$(tr ' ' , <<<"$(IMAGE_PLATFORMS)"); \
	echo "image: building $(IMAGE_REPOSITORY) $(VERSION) for $$platforms at SOURCE_DATE_EPOCH=$$epoch"; \
	docker buildx build $$builder --file deploy/container/Dockerfile --target ruralzd \
		--platform "$$platforms" --provenance=false --sbom=false \
		--build-arg SOURCE_DATE_EPOCH=$$epoch --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		--metadata-file $(DIST)/image-metadata.json "$${outputs[@]}" .; \
	if [[ "$(RELEASE_MODE)" != release ]]; then \
		echo "image: $(IMAGE_REPOSITORY)@$$(go run ./internal/tool/releasekit image -dir $(IMAGE_DIR))"; \
		exit 0; \
	fi; \
	digest=$$(go run ./internal/tool/releasekit image -dir $(IMAGE_DIR) -metadata $(DIST)/image-metadata.json); \
	docker buildx imagetools inspect "$(IMAGE_REPOSITORY)@$$digest" >/dev/null; \
	echo "image: pushed $(IMAGE_REPOSITORY)@$$digest untagged"; \
	bash scripts/install-tools.sh cosign; \
	$(COSIGN) sign --yes "$(IMAGE_REPOSITORY)@$$digest"

# Steps 3 to 5 of stage 12; run through release-dry-run or release.
release-package:
	bash scripts/install-tools.sh syft
	rm -rf $(RELEASE_DIR) $(SBOM_DIR)
	image=; if [[ -f $(IMAGE_DIR)/index.json ]]; then image=$(IMAGE_DIR); fi; \
	go run ./internal/tool/releasekit package -dist $(DIST) -notices $(NOTICES_DIR) -out $(RELEASE_DIR) \
		-version $(VERSION) -commit $(COMMIT) -bench $(BENCH_RECORDS) $${image:+-image "$$image"}
	export SYFT_CHECK_FOR_APP_UPDATE=false; \
	for a in $(RELEASE_DIR)/*.tar.gz $(RELEASE_DIR)/*.zip; do \
		[[ -e $$a && $$a != *_bench.tar.gz ]] || continue; \
		$(SYFT) scan "file:$$a" -q -o "cyclonedx-json@1.7=$$a.cdx.json"; \
	done; \
	if [[ -f $(IMAGE_DIR)/index.json ]]; then \
		mkdir -p $(SBOM_DIR); \
		for p in $(IMAGE_PLATFORMS); do \
			$(SYFT) scan "oci-dir:$(IMAGE_DIR)" --platform "$$p" -q -o "cyclonedx-json@1.7=$(SBOM_DIR)/$${p/\//_}.cdx.json"; \
		done; \
		go run ./internal/tool/releasekit sbom -manifest $(RELEASE_DIR)/release-manifest.json -scans $(SBOM_DIR); \
	fi
	go run ./internal/tool/releasekit notes -version $(VERSION) -from "$$NOTES_FROM" -to "$$NOTES_TO" -budgets $(BENCH_RECORDS) \
		-skipped "$$SKIPPED_GATES" -manifest $(RELEASE_DIR)/release-manifest.json -out $(RELEASE_DIR)/NOTES.md
	go run ./internal/tool/releasekit checksums -dir $(RELEASE_DIR)
	image=; if [[ -f $(IMAGE_DIR)/index.json ]]; then image=$(IMAGE_DIR); fi; \
	go run ./internal/tool/releasekit verify -manifest $(RELEASE_DIR)/release-manifest.json $${image:+-image "$$image"}
	cd $(RELEASE_DIR) && sha256sum -c --quiet SHA256SUMS
	if [[ "$(RELEASE_MODE)" == release ]]; then \
		bash scripts/install-tools.sh cosign; \
		$(COSIGN) sign-blob --yes --bundle $(RELEASE_DIR)/SHA256SUMS.sigstore.json $(RELEASE_DIR)/SHA256SUMS; \
		image=; if [[ -f $(IMAGE_DIR)/index.json ]]; then image=$(IMAGE_DIR); fi; \
		go run ./internal/tool/releasekit verify -manifest $(RELEASE_DIR)/release-manifest.json $${image:+-image "$$image"}; \
	fi

clean: ## Remove build output
	rm -rf $(DIST)

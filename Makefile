# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Each CI stage calls one target (docs/engineering/02-repository-layout-and-conventions.md,
# "CI stages"); `make help` lists them. Before opening a pull request run:
#   make hygiene lint generate build test supply-chain
# plus `make integration` (the State Store suites need a local redis-server).
# A target whose suite or tool does not exist yet prints a "skip" line and
# exits 0, so every stage is wired before its content lands.

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
OHA := $(BIN)/oha
VEGETA := $(BIN)/vegeta
COSIGN := $(BIN)/cosign
SYFT := $(BIN)/syft

BINARIES := ruralzd ruralz-control ruralz
# Production platforms of every binary, and the extra CLI platforms (ADR-0001).
PLATFORMS := linux/amd64 linux/arm64
CLI_PLATFORMS := darwin/arm64 darwin/amd64 windows/amd64
# Development-only ruralzd platforms: release-dry-run builds them for
# the darwin server archives; stage 4 does not.
SERVER_DEV_PLATFORMS := darwin/arm64 darwin/amd64

# Host platform without Go, for the clean-runner quickstart.
UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)
HOST_OS := $(if $(filter Darwin,$(UNAME_S)),darwin,linux)
HOST_ARCH := $(if $(filter aarch64 arm64,$(UNAME_M)),arm64,amd64)

# Build metadata embedded with -ldflags -X and printed by `ruralz version`.
# There is no build date, so rebuilding a tag reproduces identical binaries.
VERSION ?= $(shell v=$$(git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null) && echo "$${v#v}" || echo 0.0.0-dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
FLAVOR ?= default
# The release flags of stage 4 and stage 12. -s -w drop the symbol table
# and DWARF, so the size gate measures the shipped binary; stack traces and
# pprof keep working through pclntab, and rebuilds stay reproducible.
LDFLAGS := -s -w \
	-X $(MODULE)/internal/buildinfo.version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.flavor=$(FLAVOR)

# The floor job runs the latest Go 1.26 patch. CI installs it and keeps
# GOTOOLCHAIN=local; elsewhere pass FLOOR_GOTOOLCHAIN=go1.26.8 (or newer 1.26 patch).
FLOOR_GOTOOLCHAIN ?= local

# Stage 8. RURALZ_TEST_STATESTORE=process|container and
# RURALZ_TEST_REDIS_SERVER select the State Store flavors. A non-empty
# INTEGRATION_FLOOR runs the floor leg instead: the protocol conformance
# suite alone, built and run on Go 1.26 (FLOOR_GOTOOLCHAIN, as for floor)
# with GOEXPERIMENT=jsonv2, since floor and release builds run different
# HTTP/2 code (docs/engineering/03-testing-and-quality-strategy.md,
# "Conformance suites").
INTEGRATION_TIMEOUT ?= 25m
# The stage 8 packages that run Nodes on the fixed loopback ports their
# Bundles name: the benchmark smoke run and the configuration conformance
# suite's ruralzd cases (integration).
FIXED_PORT_PKGS := $(MODULE)/test/bench/scenarios $(MODULE)/test/conformance/config
INTEGRATION_FLOOR ?=
# Stage 9. BENCH_BASE is the ref whose merge base with HEAD is the alloc/op
# baseline (empty: absolute caps only); GATES selects alloc and/or size; a
# non-empty GATES_OVERRIDE records a maintainer's perf-override. BENCH_BASE
# and FUZZTIME can come from workflow inputs, so recipes read them from the
# environment ("$$BENCH_BASE") and never splice them into the shell command
# line.
BENCH_BASE ?= origin/main
export BENCH_BASE
GATES ?= alloc size
GATES_OVERRIDE ?=
SIZE_SETTLE ?= 120s
# Stage 10.
E2E_TIMEOUT ?= 40m
# Stage 11. NIGHTLY_JOB picks the job; FUZZ_SHARD=-1 runs every target.
NIGHTLY_JOB ?=
FUZZTIME ?= 15m
export FUZZTIME
FUZZ_SHARD ?= -1
FUZZ_SHARDS ?= 0
FUZZ_REPORT ?= $(DIST)/fuzz/report.json
CHAOS_SCALE ?= reduced
# The nightly Chaos job's 180 minutes bound the nightly profile; the
# reduced profile's three Nodes take V-3's 1,000 handovers in more rounds.
CHAOS_TIMEOUT ?= $(if $(filter reduced,$(CHAOS_SCALE)),240m,170m)
# The Functional job: the benchmark functional variants on the loopback
# profile, whose fixed ports allow one package at a time (-p 1).
FUNCTIONAL_TIMEOUT ?= 110m
# The Latency and soak jobs: BENCH_PROFILE names the host profile
# inventory of deploy/benchhost (empty: the loopback profile),
# BENCH_STATE the rotation state read from the bench-records branch,
# BENCH_ROTATION_ALL=true runs every slot (release), BENCH_ACCEPT a
# maintainer's <scenario>=<commit> acceptance, and BENCH_BASELINE_RURALZD
# with BENCH_BASELINE_VERSION the previous release's verified ruralzd. They
# can come from workflow inputs, so recipes read them from the environment.
BENCH_RECORDS ?= $(DIST)/bench
BENCH_RUN ?= ^TestLatency
BENCH_TIMEOUT ?= 175m
SOAK_TIMEOUT ?= 150m
BENCH_PROFILE ?=
BENCH_STATE ?=
BENCH_ROTATION_ALL ?= false
BENCH_ACCEPT ?=
BENCH_BASELINE_RURALZD ?=
BENCH_BASELINE_VERSION ?=
export BENCH_PROFILE BENCH_STATE BENCH_ROTATION_ALL BENCH_ACCEPT BENCH_BASELINE_RURALZD BENCH_BASELINE_VERSION
# image-checks.yml. IMAGE=<ref>@<digest> is the image under test; empty,
# image-checks builds one for the runner's platform from
# deploy/container/Dockerfile and the linux binary and notices a
# `make release-dry-run` left. PREVIOUS_IMAGE is the image the upgrade
# starts from, the previous release by digest (empty: the image under
# test). Both can come from workflow inputs, so the recipe reads them from
# the environment.
IMAGE ?=
PREVIOUS_IMAGE ?=
export IMAGE PREVIOUS_IMAGE
IMAGE_CHECKS_TIMEOUT ?= 60m
# Stage 12. NOTICES_DIR receives depgate's per-artifact NOTICE and
# THIRD_PARTY_LICENSES, which the archives and the image ship; IMAGE_DIR
# the image build's OCI layout and SBOM_DIR Syft's scan of each platform
# image. RELEASE_PART=image runs steps 1 and 2 (binaries, notices, image)
# and RELEASE_PART=package steps 3 to 5 on what an image part left in
# dist/, as the image and package jobs of release-build.yml do; empty runs
# every step. An empty NOTES_FROM starts the notes at the newest vX.Y.Z tag
# before NOTES_TO, never a release candidate's, or at the repository root
# for the first release (root names it too). SKIPPED_GATES names the
# ruralz.skippedgates.v1 file that release-gates writes and the notes read
# (empty: no gate skipped). release-gates decides the gates of RELEASE_TAG
# (default $GITHUB_REF_NAME) from the jobs of the release run RELEASE_RUN
# (default $GITHUB_RUN_ID), and checks the commits of RELEASE_RANGE
# (empty: since the newest vX.Y.Z tag before the commit, or after
# fc3a100 for the first release). These inputs can come from workflow
# inputs, so recipes read them from the environment.
RELEASE_DIR ?= $(DIST)/release
NOTICES_DIR ?= $(DIST)/notices
IMAGE_DIR ?= $(DIST)/image
SBOM_DIR ?= $(DIST)/sbom
IMAGE_REPOSITORY := ghcr.io/ravindu-rev/ruralzd
IMAGE_PLATFORMS := linux/amd64 linux/arm64
# The newest commit without a sign-off: the first release's DCO and
# Deprecated footer checks start after it.
FIRST_SIGNED_BASE := fc3a1009d6c59cf5f78c18ee8b05901451003383
RELEASE_PART ?=
NOTES_FROM ?=
NOTES_TO ?= HEAD
SKIPPED_GATES ?=
RELEASE_TAG ?= $(GITHUB_REF_NAME)
RELEASE_RUN ?= $(GITHUB_RUN_ID)
RELEASE_RANGE ?=
export NOTES_FROM NOTES_TO SKIPPED_GATES RELEASE_TAG RELEASE_RUN RELEASE_RANGE
RELEASEGATE_FLAGS ?=
QUICKSTART_MAX_SECONDS ?= 600

# Extra flags: TEST_FLAGS for the go test suites (for example -run X -v),
# BENCHGATE_FLAGS and SIZEGATE_FLAGS for the stage 9 tools.
TEST_FLAGS ?=
BENCHGATE_FLAGS ?=
SIZEGATE_FLAGS ?=

SUMMARY_FLAG = $(if $(GITHUB_STEP_SUMMARY),-summary "$(GITHUB_STEP_SUMMARY)")
OVERRIDE_FLAG = $(if $(GATES_OVERRIDE),-override)

# has_tests expands to a test file under $(1), or to nothing when the suite
# does not exist yet.
has_tests = $(shell find $(1) -name '*_test.go' -print -quit 2>/dev/null)

.PHONY: help tools hygiene lint fmt generate build test floor supply-chain \
	integration gates e2e quickstart nightly fuzz chaos chaos-scale bench-functional bench-latency soak \
	t5 image-checks release-dry-run release release-image release-package release-gates clean

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

hygiene: ## Stage 1: repocheck and the no-license-check scan; DCO_RANGE adds the sign-off, Deprecated footer and golden-corpus checks, PR_TITLE the title check
	go run ./internal/tool/repocheck
	if [[ -n "$${DCO_RANGE:-}" ]]; then go run ./internal/tool/commitcheck dco -range "$$DCO_RANGE"; fi
	if [[ -n "$${DCO_RANGE:-}" ]]; then go run ./internal/tool/commitcheck deprecations -range "$$DCO_RANGE"; fi
	if [[ -n "$${DCO_RANGE:-}" ]]; then go run ./internal/tool/commitcheck golden -range "$$DCO_RANGE"; fi
	if [[ -n "$${PR_TITLE:-}" ]]; then go run ./internal/tool/commitcheck title; fi

lint: $(GOLANGCI_LINT) $(ACTIONLINT) $(SHELLCHECK) ## Stage 2: gofumpt, goimports, golangci-lint and actionlint, with the pinned shellcheck on the workflows' run steps
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) fmt --diff ./...
	$(GOLANGCI_LINT) run ./...
	$(ACTIONLINT) -shellcheck=$(SHELLCHECK)

fmt: $(GOLANGCI_LINT) ## Rewrite files with gofumpt and goimports
	$(GOLANGCI_LINT) fmt ./...

# Generated trees diffed by stage 3: the JSON Schema and the
# served-feature table (schemagen), the alert rules and dashboards
# generated from the telemetry catalog (telemetrygen) and the benchmark
# record schema (internal/testkit/benchrecord).
GENERATED := api/schema/ruralz internal/config/registry/planned_gen.go deploy/grafana test/bench/record.schema.json

generate: ## Stage 3: regenerate the JSON Schema, alert rules and dashboards and fail on any difference
	go generate ./...
	git diff --exit-code -- $(GENERATED) || { echo "generated files are stale: commit the output of 'make generate'" >&2; exit 1; }
	untracked=$$(git ls-files --others --exclude-standard -- $(GENERATED)); \
	if [[ -n "$$untracked" ]]; then echo "untracked generated files: $$untracked" >&2; exit 1; fi

build: ## Stage 4 (G1): cross-build with CGO_ENABLED=0, -trimpath and the release -ldflags (-s -w -X)
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

test: ## Stage 5: race tests with cgo, then the shipped CGO_ENABLED=0 configuration (includes test/conformance/config)
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

# Stage 8's go test includes test/t5's unit check: systemd-analyze verify of
# the systemd unit and systemd-sysusers with its sysusers.d entry.
integration: $(if $(INTEGRATION_FLOOR),,$(PROMTOOL)) ## Stage 8: integration and conformance suites (tag integration, test/t5's systemd unit check included), State Store flavors, promtool rules, Dockerfile check; INTEGRATION_FLOOR=1 runs the protocol suite on Go 1.26
ifneq ($(INTEGRATION_FLOOR),)
	export GOTOOLCHAIN=$(FLOOR_GOTOOLCHAIN) GOEXPERIMENT=jsonv2 CGO_ENABLED=1; \
	v=$$(go env GOVERSION); \
	if [[ $$v != go1.26.* ]]; then echo "integration: the floor leg wants Go 1.26, got $$v; set FLOOR_GOTOOLCHAIN" >&2; exit 1; fi; \
	echo "integration: floor leg on $$v"; \
	go test -race -shuffle=on -tags integration,netgo,osusergo -timeout $(INTEGRATION_TIMEOUT) $(TEST_FLAGS) ./test/conformance/protocol/...
else
	@# The packages of FIXED_PORT_PKGS run Nodes on the loopback ports their
	@# Bundles name, which the Bundles' pinned digests fix: they run one
	@# package at a time after the others, so no two Nodes share a port.
	pkgs=$$(go list -tags integration,netgo,osusergo ./... | grep -v -x -F $(addprefix -e ,$(FIXED_PORT_PKGS))); \
	rc=0; \
	PATH="$(BIN):$$PATH" CGO_ENABLED=1 go test -race -shuffle=on -tags integration,netgo,osusergo -timeout $(INTEGRATION_TIMEOUT) $(TEST_FLAGS) $$pkgs || rc=1; \
	PATH="$(BIN):$$PATH" CGO_ENABLED=1 go test -p 1 -race -shuffle=on -tags integration,netgo,osusergo -timeout $(INTEGRATION_TIMEOUT) $(TEST_FLAGS) $(FIXED_PORT_PKGS) || rc=1; \
	exit $$rc
	if [[ ! -f deploy/container/Dockerfile ]]; then \
		echo "integration: skip Dockerfile check: deploy/container/Dockerfile is not present yet"; \
	elif ! docker info >/dev/null 2>&1; then \
		echo "integration: skip Dockerfile check: no Docker daemon"; \
	else \
		docker buildx build --check -f deploy/container/Dockerfile .; \
	fi
endif

# Every selected gate runs even after another fails, so the job summary
# always reports both the alloc/op and the size and idle RSS values; the
# recipe exits with the highest status: 1 gate failure, 2 tool error. The
# gate tools are built first so their 1 and 2 survive (go run turns every
# failure into 1); a gate tool that does not build exits 2.
gates: ## Stage 9: alloc/op A/B gate (benchgate) and stripped size and idle RSS gate (sizegate); GATES=alloc or GATES=size runs one
	for g in $(GATES); do case $$g in alloc | size) ;; *) echo "gates: unknown gate $$g (want alloc or size)" >&2; exit 2 ;; esac; done
	tools=$$(mktemp -d); trap 'rm -rf "$$tools"' EXIT; \
	go build -o "$$tools/" ./internal/tool/benchgate ./internal/tool/sizegate || exit 2; \
	rc=0; \
	worst() { if (( $$1 > rc )); then rc=$$1; fi; }; \
	if [[ " $(GATES) " == *" alloc "* ]]; then \
		"$$tools/benchgate" -base "$$BENCH_BASE" -config test/bench/allocgate.json $(SUMMARY_FLAG) $(OVERRIDE_FLAG) $(BENCHGATE_FLAGS) || worst $$?; \
	fi; \
	if [[ " $(GATES) " == *" size "* ]]; then \
		built=true; \
		for arch in amd64 arm64; do \
			echo "build $(DIST)/linux_$$arch/ruralzd"; \
			CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/linux_$$arch/ruralzd ./cmd/ruralzd || { built=false; worst 2; }; \
		done; \
		if [[ $$built == true ]]; then \
			"$$tools/sizegate" -binary $(DIST)/linux_amd64/ruralzd -binary $(DIST)/linux_arm64/ruralzd \
				$(if $(filter Linux,$(UNAME_S)),-idle-rss -settle $(SIZE_SETTLE)) $(SUMMARY_FLAG) $(OVERRIDE_FLAG) $(SIZEGATE_FLAGS) || worst $$?; \
		fi; \
	fi; \
	exit $$rc

e2e: ## Stage 10: end-to-end suite with the secret leak assertions (tag e2e, test/e2e)
ifeq ($(call has_tests,test/e2e),)
	@echo "e2e: skip: test/e2e has no suite yet"
else
	go test -tags e2e -timeout $(E2E_TIMEOUT) $(TEST_FLAGS) ./test/e2e/...
endif

# The macOS runners' make is GNU Make 3.81, which ignores .SHELLFLAGS, so
# the recipe takes the script's status from PIPESTATUS instead of pipefail.
quickstart: ## Stage 10 SM-7: examples/quickstart/quickstart.sh from an archive to a first proxied request within QUICKSTART_MAX_SECONDS
	archive="$${RURALZ_QUICKSTART_ARCHIVE:-}"; \
	if [[ -z "$$archive" ]]; then \
		archive=$$(compgen -G '$(RELEASE_DIR)/ruralzd_*_$(HOST_OS)_$(HOST_ARCH).tar.gz' | head -n 1 || true); \
	fi; \
	if [[ -n "$$archive" ]]; then echo "quickstart: archive $$archive"; else echo "quickstart: no local archive; the script downloads the release"; fi; \
	log=$$(mktemp); trap 'rm -f "$$log"' EXIT; \
	set +e; RURALZ_QUICKSTART_ARCHIVE="$$archive" bash examples/quickstart/quickstart.sh 2>&1 | tee "$$log"; rc=$${PIPESTATUS[0]}; set -e; \
	if [[ $$rc -ne 0 ]]; then echo "quickstart: examples/quickstart/quickstart.sh exited $$rc" >&2; exit "$$rc"; fi; \
	secs=$$(sed -nE 's/.*first proxied request after ([0-9]+([.][0-9]+)?) s.*/\1/p' "$$log" | tail -n 1); \
	if [[ -z "$$secs" ]]; then echo "quickstart: the script did not report 'first proxied request after <seconds> s'" >&2; exit 1; fi; \
	if awk -v s="$$secs" -v m=$(QUICKSTART_MAX_SECONDS) 'BEGIN { exit !(s > m) }'; then \
		echo "quickstart: first proxied request after $$secs s, above $(QUICKSTART_MAX_SECONDS) s (SM-7)" >&2; exit 1; \
	fi; \
	echo "quickstart: first proxied request after $$secs s (limit $(QUICKSTART_MAX_SECONDS) s)"

nightly: ## Stage 11: one nightly job, NIGHTLY_JOB=fuzz|chaos|functional|latency
	@case "$(NIGHTLY_JOB)" in \
		fuzz) $(MAKE) --no-print-directory fuzz ;; \
		chaos) $(MAKE) --no-print-directory chaos CHAOS_SCALE=nightly ;; \
		functional) $(MAKE) --no-print-directory bench-functional ;; \
		latency) $(MAKE) --no-print-directory bench-latency ;; \
		*) echo "nightly: set NIGHTLY_JOB to fuzz, chaos, functional or latency" >&2; exit 2 ;; \
	esac

fuzz: ## Stage 11 Fuzz job: each Fuzz target for FUZZTIME (15m); FUZZ_SHARD=I runs shard I of eight targets
	if [[ ! "$$FUZZTIME" =~ ^([0-9]+(ms|s|m|h))+$$|^[1-9][0-9]*x$$ ]]; then echo "fuzz: FUZZTIME=$$FUZZTIME is not a duration such as 15m or a count such as 500x" >&2; exit 2; fi
	mkdir -p $(dir $(FUZZ_REPORT))
	go run ./internal/tool/fuzzplan run -index $(FUZZ_SHARD) -shards $(FUZZ_SHARDS) -fuzztime "$$FUZZTIME" -report $(FUZZ_REPORT)

chaos: ## Stage 11 Chaos job: CE experiments, failure rows and drills (tag e2e, test/chaos) at CHAOS_SCALE=reduced|nightly|target
ifeq ($(call has_tests,test/chaos),)
	@echo "chaos: skip: test/chaos has no suite yet"
else
	@# Each run starts dist/chaos empty, so the records it uploads, and the
	@# saturation record a later package reads, are this run's alone.
	rm -rf $(DIST)/chaos
	RURALZ_CHAOS_SCALE=$(CHAOS_SCALE) RURALZ_CHAOS_RECORDS=$(DIST)/chaos RURALZ_BENCH_PROFILE="$$BENCH_PROFILE" \
		go test -tags e2e -p 1 -timeout $(CHAOS_TIMEOUT) $(TEST_FLAGS) ./test/chaos/...
endif

# The target profile runs on the hosts of the inventory BENCH_PROFILE names,
# deploy/benchhost/rh-1.json in chaos-scale.yml.
chaos-scale: ## Chaos at target scale on the RH-1 hosts (CE-3, CE-5 at 100 Nodes, CE-12, CE-15, GD-2), run by chaos-scale.yml
	$(MAKE) --no-print-directory chaos CHAOS_SCALE=target

bench-functional: ## Stage 11 Functional job: every M1 benchmark scenario and constants run at reduced scale on loopback (tag e2e, test/bench/functional)
ifeq ($(call has_tests,test/bench/functional),)
	@echo "bench-functional: skip: test/bench/functional has no suite yet"
else
	go test -tags e2e -p 1 -timeout $(FUNCTIONAL_TIMEOUT) $(TEST_FLAGS) ./test/bench/functional/...
endif

bench-latency: ## Stage 11 Latency job on RH-1: macro scenarios, budgets, gates and records (tag e2e, test/bench/harness)
ifeq ($(call has_tests,test/bench/harness),)
	@echo "bench-latency: skip: test/bench/harness has no suite yet"
else
	bash scripts/install-tools.sh oha vegeta
	mkdir -p $(BENCH_RECORDS)
	RURALZ_BENCH_OHA=$(OHA) RURALZ_BENCH_VEGETA=$(VEGETA) RURALZ_BENCH_RECORDS=$(BENCH_RECORDS) \
		RURALZ_BENCH_PROFILE="$$BENCH_PROFILE" RURALZ_BENCH_STATE="$$BENCH_STATE" \
		RURALZ_BENCH_ROTATION_ALL="$$BENCH_ROTATION_ALL" RURALZ_BENCH_ACCEPT="$$BENCH_ACCEPT" \
		RURALZ_BENCH_BASELINE_RURALZD="$$BENCH_BASELINE_RURALZD" RURALZ_BENCH_BASELINE_VERSION="$$BENCH_BASELINE_VERSION" \
		go test -tags e2e -p 1 -run '$(BENCH_RUN)' -timeout $(BENCH_TIMEOUT) $(TEST_FLAGS) ./test/bench/harness/...
endif

soak: ## Release soak on RH-1: S2 at half saturation for 2 hours (tag e2e, test/bench/harness)
	$(MAKE) --no-print-directory bench-latency BENCH_RUN=^TestSoak BENCH_TIMEOUT=$(SOAK_TIMEOUT)

# The helper checks run anywhere. The live suite runs only with
# RURALZ_TEST_T5=1, as root on a host whose PID 1 is systemd, and installs
# the linux ruralzd archive `make release-dry-run` left in dist/release/.
t5: ## T5 systemd verification (tag t5, test/t5): helper checks; with RURALZ_TEST_T5=1, as root under systemd, the live suite on the release-dry-run archive; run by t5-systemd.yml
	go test -tags t5 -p 1 -timeout 60m $(TEST_FLAGS) ./test/t5/...

# image-checks tests IMAGE, or builds the image for the runner's platform
# from what `make release-dry-run` left: dist/linux_<arch>/ruralzd and its
# notices. The suite runs the CLI that RURALZ_TEST_BIN_DIR names, else
# dist/linux_<arch>/ruralz when present, else one it builds from the
# commit.
image-checks: ## Compose end-to-end suite and air-gapped image start (tag compose, test/compose; Docker) on IMAGE=<ref>@<digest> or the dry run's image, run by image-checks.yml
ifeq ($(call has_tests,test/compose),)
	@echo "image-checks: skip: test/compose has no suite yet"
else
	image="$${IMAGE:-}"; \
	if [[ -z "$$image" ]]; then \
		image=ruralzd:image-checks; \
		for f in dist/linux_$(HOST_ARCH)/ruralzd dist/notices/ruralzd_linux_$(HOST_ARCH)/NOTICE dist/notices/ruralzd_linux_$(HOST_ARCH)/THIRD_PARTY_LICENSES; do \
			if [[ ! -f $$f ]]; then echo "image-checks: $$f is missing: run make release-dry-run first, or set IMAGE=<ref>@<digest>" >&2; exit 2; fi; \
		done; \
		echo "image-checks: building $$image for linux/$(HOST_ARCH) from deploy/container/Dockerfile"; \
		docker buildx build --load --platform linux/$(HOST_ARCH) --file deploy/container/Dockerfile \
			--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --tag "$$image" .; \
	fi; \
	bindir="$${RURALZ_TEST_BIN_DIR:-}"; \
	if [[ -z "$$bindir" && -x dist/linux_$(HOST_ARCH)/ruralz ]]; then bindir="$(CURDIR)/dist/linux_$(HOST_ARCH)"; fi; \
	echo "image-checks: image $$image, upgrade from $${PREVIOUS_IMAGE:-the same image}, CLI $${bindir:-built from the commit}"; \
	RURALZ_TEST_IMAGE="$$image" RURALZ_TEST_PREVIOUS_IMAGE="$${PREVIOUS_IMAGE:-}" RURALZ_TEST_BIN_DIR="$$bindir" \
		go test -tags compose -p 1 -timeout $(IMAGE_CHECKS_TIMEOUT) $(TEST_FLAGS) ./test/compose/...
endif

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

# Every check runs even after another fails, and the recipe exits with
# the highest status: 1 findings, 2 a tool or API error. releasegate runs
# first, so the tag's tracking issue records every gate's outcome.
release-gates: ## Stage 12 gates: releasegate on the release run (tracking issue, skips, SKIPPED_GATES), the no-license-check scan, depgate G2 and G3, commitcheck dco and deprecations since the previous release
	tools=$$(mktemp -d); trap 'rm -rf "$$tools"' EXIT; \
	go build -o "$$tools/" ./internal/tool/releasegate ./internal/tool/repocheck ./internal/tool/depgate ./internal/tool/commitcheck || exit 2; \
	rc=0; \
	worst() { if (( $$1 > rc )); then rc=$$1; fi; }; \
	"$$tools/releasegate" gates -tag "$$RELEASE_TAG" -commit "$(COMMIT)" -run "$$RELEASE_RUN" -records $(BENCH_RECORDS) \
		-skipped "$$SKIPPED_GATES" $(SUMMARY_FLAG) $(RELEASEGATE_FLAGS) || worst $$?; \
	"$$tools/repocheck" || worst $$?; \
	"$$tools/depgate" || worst $$?; \
	range="$$RELEASE_RANGE"; \
	if [[ -z "$$range" ]]; then \
		prev=$$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude 'v*-*' "$(COMMIT)^" 2>/dev/null || true); \
		range="$${prev:-$(FIRST_SIGNED_BASE)}..$(COMMIT)"; \
	fi; \
	echo "release-gates: commit checks over $$range"; \
	"$$tools/commitcheck" dco -range "$$range" || worst $$?; \
	"$$tools/commitcheck" deprecations -range "$$range" || worst $$?; \
	exit $$rc

clean: ## Remove build output
	rm -rf $(DIST)

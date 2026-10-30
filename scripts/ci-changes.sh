#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Computes the path filters of pr-fast and pr-full from the event's commit
# range and writes them to $GITHUB_OUTPUT
# (docs/engineering/02-repository-layout-and-conventions.md, CI stages):
#
#   go=           Go changes (stages 2, 4, 9): *.go, go.mod, go.sum,
#                 .golangci.yml, Makefile, scripts/
#   generate=     stage 3 inputs, including the alert rules and dashboards
#                 generated from the telemetry catalog and the benchmark
#                 record schema test/bench/record.schema.json, which
#                 internal/testkit/benchrecord writes
#   test=         stage 5 and the floor job: Go changes, examples/, the
#                 configuration conformance fixtures under
#                 test/conformance/config/ (a golden regeneration touches only
#                 fixtures, whose digests are checked on the floor, darwin
#                 and windows jobs), test/bench/ (the scenario Bundles and
#                 their pinned digests), or the documents the examples tests
#                 read: the configuration model (example Bundles, Worked
#                 example, diagnostics) and Deployment topologies (the
#                 quickstart Bundle)
#   integration=  stage 8: the pr-full filter and every merge queue run
#   gates=        stage 9: Go changes, or the alloc/op gate list
#                 test/bench/allocgate.json
#
# A change under .github/workflows/, a push, a manual or scheduled run, and
# ALL=true (the workflow_call input `all` that release.yml passes) select
# every stage.
set -euo pipefail

out=${GITHUB_OUTPUT:-/dev/stdout}
zero=0000000000000000000000000000000000000000
every() { printf 'go=true\ngenerate=true\ntest=true\nintegration=true\ngates=true\n' >>"$out"; }

if [[ "${ALL:-false}" == true ]]; then
  every
  exit 0
fi

case "${EVENT:?}" in
  pull_request) range="${PR_BASE:?}...${PR_HEAD:?}" ;;
  merge_group) range="${MG_BASE:?}..${MG_HEAD:?}" ;;
  *) range="" ;;
esac

if [[ -z "$range" || "${PUSH_BEFORE:-}" == "$zero" ]]; then
  every
  exit 0
fi

files=$(git diff --name-only "$range")
printf 'changed files:\n%s\n' "$files" >&2

any() { grep -Eq "$1" <<<"$files"; }

if any '^\.github/workflows/'; then
  every
  exit 0
fi

go=false; generate=false; test=false; integration=false
any '\.go$|^go\.(mod|sum)$|^\.golangci\.yml$|^Makefile$|^scripts/' && go=true
any '^(pkg|api|internal/gen|internal/tool|internal/telemetry/catalog|internal/testkit/benchrecord|sdk|deploy/crds|deploy/grafana)/|^test/bench/record\.schema\.json$|^buf(\.gen)?\.yaml$|^go\.mod$' && generate=true
{ [[ $go == true ]] || any '^examples/|^test/conformance/config/|^test/bench/|^docs/architecture/02-configuration-model\.md$|^docs/operations/01-deployment-topologies\.md$'; } && test=true
gates=$go
any '^test/bench/allocgate\.json$' && gates=true
any '\.go$|^go\.(mod|sum)$|^\.golangci\.yml$|^Makefile$|^scripts/|^(api|examples|test|deploy)/|^internal/tool/' && integration=true
[[ "$EVENT" == merge_group ]] && integration=true
printf 'go=%s\ngenerate=%s\ntest=%s\nintegration=%s\ngates=%s\n' "$go" "$generate" "$test" "$integration" "$gates" >>"$out"

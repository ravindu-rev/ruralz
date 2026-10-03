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
#                 generated from the telemetry catalog
#   test=         stage 5 and the floor job: Go changes, examples/ or the
#                 configuration conformance fixtures under
#                 test/conformance/config/ (a golden regeneration touches only
#                 fixtures, and 11 req 9 needs their digests checked on the
#                 floor, darwin and windows jobs)
#   integration=  stage 8: the pr-full filter (11 req 4) and every merge
#                 queue run (11 req 1)
#   gates=        stage 9: Go changes (11 req 3)
#
# A change under .github/workflows/, a push, a manual or scheduled run, and
# ALL=true (the workflow_call input `all` that release.yml passes, 11 req 11)
# select every stage.
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
any '^(pkg|api|internal/gen|internal/tool|internal/telemetry/catalog|sdk|deploy/crds|deploy/grafana)/|^buf(\.gen)?\.yaml$|^go\.mod$' && generate=true
{ [[ $go == true ]] || any '^examples/|^test/conformance/config/'; } && test=true
any '\.go$|^go\.(mod|sum)$|^\.golangci\.yml$|^Makefile$|^scripts/|^(api|examples|test|deploy)/|^internal/tool/' && integration=true
[[ "$EVENT" == merge_group ]] && integration=true
printf 'go=%s\ngenerate=%s\ntest=%s\nintegration=%s\ngates=%s\n' "$go" "$generate" "$test" "$integration" "$go" >>"$out"

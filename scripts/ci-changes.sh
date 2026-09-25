#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Computes the path filters of pr-fast from the event's commit range and
# writes go=, generate= and test= to $GITHUB_OUTPUT. A change under
# .github/workflows/, a push to main and a manual run select every stage
# (docs/engineering/02-repository-layout-and-conventions.md, CI stages).
set -euo pipefail

out=${GITHUB_OUTPUT:-/dev/stdout}
zero=0000000000000000000000000000000000000000
case "${EVENT:?}" in
  pull_request) range="${PR_BASE:?}...${PR_HEAD:?}" ;;
  merge_group) range="${MG_BASE:?}..${MG_HEAD:?}" ;;
  *) range="" ;;
esac

if [[ -z "$range" || "${PUSH_BEFORE:-}" == "$zero" ]]; then
  printf 'go=true\ngenerate=true\ntest=true\n' >>"$out"
  exit 0
fi

files=$(git diff --name-only "$range")
printf 'changed files:\n%s\n' "$files" >&2

any() { grep -Eq "$1" <<<"$files"; }

if any '^\.github/workflows/'; then
  printf 'go=true\ngenerate=true\ntest=true\n' >>"$out"
  exit 0
fi

go=false; generate=false; test=false
any '\.go$|^go\.(mod|sum)$|^\.golangci\.yml$|^Makefile$|^scripts/' && go=true
any '^(pkg|api|internal/gen|internal/tool|sdk|deploy/crds)/|^buf(\.gen)?\.yaml$|^go\.mod$' && generate=true
{ [[ $go == true ]] || any '^examples/'; } && test=true
printf 'go=%s\ngenerate=%s\ntest=%s\n' "$go" "$generate" "$test" >>"$out"

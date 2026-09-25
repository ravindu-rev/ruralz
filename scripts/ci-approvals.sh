#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Stage 1 approval count: a pull request that changes api/, pkg/, docs/_meta/,
# go.mod, a security-sensitive package, the no-license-check allowlist or a
# workflow needs two approvals from people other than its author. Branch
# protection enforces the one CODEOWNERS approval every pull request needs.
# Reads REPO and PR from the environment; needs GH_TOKEN with pull-requests: read.
set -euo pipefail

sensitive='^(api/|pkg/|docs/_meta/|go\.mod$|internal/(filter/auth|filter/authz|signing|pluginhost)/|internal/tool/repocheck/nolicensecheck\.allow$|\.github/)'

files=$(gh api --paginate "repos/${REPO:?}/pulls/${PR:?}/files" --jq '.[].filename')
matches=$(grep -E "$sensitive" <<<"$files" || true)
if [[ -z "$matches" ]]; then
  echo "no path needs two approvals"
  exit 0
fi
printf 'paths needing two approvals:\n%s\n' "$matches"

author=$(gh api "repos/$REPO/pulls/$PR" --jq '.user.login')
# The latest APPROVED, CHANGES_REQUESTED or DISMISSED review of each reviewer counts.
approvals=$(gh api --paginate "repos/$REPO/pulls/$PR/reviews" --jq '.[] | select(.state != "COMMENTED" and .state != "PENDING") | [.user.login, .state] | @tsv' |
  awk -F'\t' -v author="$author" '$1 != author { state[$1] = $2 } END { n = 0; for (u in state) if (state[u] == "APPROVED") n++; print n }')
echo "approvals: $approvals of 2"
if (( approvals < 2 )); then
  echo "this pull request needs two approvals" >&2
  exit 1
fi

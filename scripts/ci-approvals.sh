#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Pull request approval checks. Both read REPO and PR from the environment
# and need GH_TOKEN with pull-requests: read (and issues: read for the
# label history).
#
#   ci-approvals.sh [count]
#     Stage 1 approval count: a pull request that changes api/, pkg/,
#     docs/_meta/, go.mod, a security-sensitive package, the no-license-check
#     allowlist, a workflow or the alloc/op gate list test/bench/allocgate.json
#     needs two approvals from people other than its author. The gate list
#     is on it because benchgate reads the head's copy, so raising a cap or
#     the threshold, or dropping an entry, loosens the stage 9 gate without
#     the perf-override label (11 req 66). Branch protection enforces the one CODEOWNERS approval every
#     pull request needs. It gates only merge and never other stages.
#
#   ci-approvals.sh perf-override
#     Stage 9 override (docs/architecture/12-performance-budgets-and-benchmarking.md,
#     "Regression policy and gates"; 11 req 66): exits 0 when the pull
#     request carries the perf-override label and its latest application
#     was by a maintainer (repository role admin or maintain) other than the
#     author, which records the approval; exits 1 otherwise. The gates job
#     then reports failures without failing.
set -euo pipefail

sensitive='^(api/|pkg/|docs/_meta/|go\.mod$|internal/(filter/auth|filter/authz|signing|pluginhost)/|internal/tool/repocheck/nolicensecheck\.allow$|\.github/|test/bench/allocgate\.json$)'
override_label=perf-override

count() {
  local files matches author approvals
  files=$(gh api --paginate "repos/${REPO:?}/pulls/${PR:?}/files" --jq '.[].filename')
  matches=$(grep -E "$sensitive" <<<"$files" || true)
  if [[ -z "$matches" ]]; then
    echo "no path needs two approvals"
    return 0
  fi
  printf 'paths needing two approvals:\n%s\n' "$matches"

  author=$(gh api "repos/$REPO/pulls/$PR" --jq '.user.login')
  # The latest APPROVED, CHANGES_REQUESTED or DISMISSED review of each reviewer counts.
  approvals=$(gh api --paginate "repos/$REPO/pulls/$PR/reviews" --jq '.[] | select(.state != "COMMENTED" and .state != "PENDING") | [.user.login, .state] | @tsv' |
    awk -F'\t' -v author="$author" '$1 != author { state[$1] = $2 } END { n = 0; for (u in state) if (state[u] == "APPROVED") n++; print n }')
  echo "approvals: $approvals of 2"
  if ((approvals < 2)); then
    echo "this pull request needs two approvals" >&2
    return 1
  fi
}

perf_override() {
  local labels actor author role
  labels=$(gh api "repos/${REPO:?}/issues/${PR:?}/labels" --jq '.[].name')
  if ! grep -qxF "$override_label" <<<"$labels"; then
    echo "no $override_label label"
    return 1
  fi
  actor=$(gh api --paginate "repos/$REPO/issues/$PR/events" \
    --jq ".[] | select(.event == \"labeled\" and .label.name == \"$override_label\") | .actor.login" | tail -n 1)
  author=$(gh api "repos/$REPO/pulls/$PR" --jq '.user.login')
  if [[ -z "$actor" ]]; then
    echo "$override_label: no labeled event found" >&2
    return 1
  fi
  if [[ "$actor" == "$author" ]]; then
    echo "$override_label was applied by the author $actor; a maintainer other than the author must apply it" >&2
    return 1
  fi
  role=$(gh api "repos/$REPO/collaborators/$actor/permission" --jq '.role_name // .permission')
  case "$role" in
    admin | maintain)
      echo "$override_label applied by maintainer $actor ($role): gate failures are reported, not blocking"
      ;;
    *)
      echo "$override_label applied by $actor with role $role; only a maintainer (admin or maintain) can approve an override" >&2
      return 1
      ;;
  esac
}

case "${1:-count}" in
  count) count ;;
  perf-override) perf_override ;;
  *)
    echo "usage: ci-approvals.sh [count|perf-override]" >&2
    exit 2
    ;;
esac

#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Stage 9 perf-override check (docs/architecture/12-performance-budgets-and-benchmarking.md,
# "Regression policy and gates"). It reads REPO and PR from the environment
# and needs GH_TOKEN with pull-requests: read and issues: read.
#
#   ci-approvals.sh perf-override
#     Exits 0 when the pull request carries the perf-override label and its
#     latest application was by a maintainer (repository role admin or
#     maintain); exits 1 otherwise. The gates job then reports failures
#     without failing.
set -euo pipefail

override_label=perf-override

perf_override() {
  local labels actor role
  labels=$(gh api "repos/${REPO:?}/issues/${PR:?}/labels" --jq '.[].name')
  if ! grep -qxF "$override_label" <<<"$labels"; then
    echo "no $override_label label"
    return 1
  fi
  actor=$(gh api --paginate "repos/$REPO/issues/$PR/events" \
    --jq ".[] | select(.event == \"labeled\" and .label.name == \"$override_label\") | .actor.login" | tail -n 1)
  if [[ -z "$actor" ]]; then
    echo "$override_label: no labeled event found" >&2
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

case "${1:-}" in
  perf-override) perf_override ;;
  *)
    echo "usage: ci-approvals.sh perf-override" >&2
    exit 2
    ;;
esac

#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# The repository's GitHub settings, through the GitHub CLI
# (docs/engineering/02-repository-layout-and-conventions.md, "Repository
# settings"). It reads each area of the settings table and compares it
# with the table, printing a "drift:" line per setting that differs; it
# leaves out the RH1_PROVISIONED variable, which the owner sets once the
# rh-1 runner is registered.
#
#   scripts/repo-settings.sh          writes each area that differs, reads
#                                     it back and fails unless it then
#                                     matches; an area that matches is not
#                                     written, so a second run writes
#                                     nothing. A repository admin runs it,
#                                     with gh logged in.
#   scripts/repo-settings.sh --check  writes nothing; the weekly
#                                     repo-settings.yml workflow runs it.
#
# Both modes end with the steps no API covers: the public visibility and
# the repository link of the ghcr.io/ravindu-rev/ruralzd package, set by
# hand after the first candidate's image push
# (docs/engineering/04-release-versioning-and-compatibility.md, "Release
# runbook", step 8). The script neither reads nor writes them.
#
# Exit status: 0 when every setting matches the table (or was written to
# match it), 1 when --check found drift, 2 on an error: a usage error, gh
# missing, a failed API call, a setting the response leaves out because
# the token may not read it while applying, or a setting that still
# differs after its write. --check, run with the weekly workflow's
# read-only token, prints a "notice:" line for each setting the response
# leaves out and checks the rest. The drift lines are printed even when
# the run ends in an error. Errors go to standard error, everything else
# to standard output.
#
# It needs only gh, whose --jq filters the responses, and keeps to bash
# 3.2, the bash of macOS.
set -euo pipefail

readonly prog=repo-settings
readonly repo=ravindu-rev/ruralz
readonly package=ruralzd
# Required status checks come from GitHub Actions, the GitHub App with
# this ID.
readonly actions_app=15368
# The repository role ID of admin, the only role that bypasses the v* tag
# creation ruleset.
readonly admin_role=5
# Ruleset names, from the Area cells of the settings table.
readonly ruleset_branches='main and release-*'
readonly ruleset_tag_creation='v* tag creation'
readonly ruleset_tag_changes='v* tag updates and deletion'
readonly ruleset_develop='develop'
readonly environment=release

# Areas, in the order they are compared and written: Dependabot alerts
# before security updates, which need them, and the release Environment
# before its deployment branch policies, which need its custom branch
# policies on.
readonly area_merging='Merging'
readonly area_secret_scanning='Security: secret scanning'
readonly area_pvr='Security: private vulnerability reporting'
readonly area_alerts='Security: Dependabot alerts'
readonly area_security_updates='Security: Dependabot security updates'
readonly area_actions='Actions: permissions'
readonly area_workflow='Actions: workflow token'
readonly area_fork='Actions: fork pull requests'
readonly area_immutable='Releases: immutable releases'
readonly area_ruleset_branches="Ruleset \"$ruleset_branches\""
readonly area_ruleset_tag_creation="Ruleset \"$ruleset_tag_creation\""
readonly area_ruleset_tag_changes="Ruleset \"$ruleset_tag_changes\""
readonly area_ruleset_develop="Ruleset \"$ruleset_develop\""
readonly area_environment="Environment \"$environment\""
readonly area_branches="Environment \"$environment\" deployment branches"
areas=(
  "$area_merging" "$area_secret_scanning" "$area_pvr" "$area_alerts" "$area_security_updates"
  "$area_actions" "$area_workflow" "$area_fork" "$area_immutable"
  "$area_ruleset_branches" "$area_ruleset_tag_creation" "$area_ruleset_tag_changes" "$area_ruleset_develop"
  "$area_environment" "$area_branches"
)

usage() {
  echo "usage: $prog [--check]" >&2
  exit 2
}

mode=apply
case $# in
  0) ;;
  1) if [[ $1 == --check ]]; then mode=check; else usage; fi ;;
  *) usage ;;
esac

if ! command -v gh >/dev/null 2>&1; then
  echo "$prog: gh not found: install the GitHub CLI, https://cli.github.com" >&2
  exit 2
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# The sources' files, $tmp/src.*, which meta and compare read, always
# match the glob.
: >"$tmp/src.none"

# api METHOD ENDPOINT [FLAG...]: one REST API call through gh; a write
# reads its JSON body from standard input (--input -). It prints the
# response and returns 0, or prints the error and returns 1.
api() {
  local rc=0
  api_or_absent "$@" || rc=$?
  if [[ $rc -eq 4 ]]; then
    echo "$prog: $1 $2: Not Found (HTTP 404)" >&2
    return 1
  fi
  return "$rc"
}

# api_or_absent METHOD ENDPOINT [FLAG...]: as api, but returns 4 without a
# message when the API answers 404 Not Found, which several endpoints use
# for a setting that is off or an object that does not exist.
api_or_absent() {
  local method=$1 endpoint=$2 err
  shift 2
  if gh api --method "$method" "$endpoint" "$@" >"$tmp/out" 2>"$tmp/err"; then
    cat "$tmp/out"
    return 0
  fi
  # gh prints the error body on standard output, which is dropped, and
  # "gh: <message> (HTTP <status>)" or "gh: HTTP <status>" on standard
  # error.
  err=$(cat "$tmp/err")
  case $err in
    *"HTTP 404"*) return 4 ;;
  esac
  echo "$prog: $method $endpoint: ${err#gh: }" >&2
  return 1
}

# jq_string S: S as a jq string literal.
jq_string() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  printf '"%s"' "$s"
}

# jq_prelude AREA: the jq definitions every source program starts with.
# row prints one line of the live table, "AREA<TAB>NAME<TAB>VALUE", and
# item one whose value is the JSON of its second argument.
jq_prelude() {
  printf 'def row($n; $v): [%s, $n, $v] | join("\\t"); def item($n; $v): row($n; $v | tojson);\n' "$(jq_string "$1")"
}

# fields_jq AREA FIELD...: a jq program printing each top-level FIELD of
# the response as an item of AREA.
fields_jq() {
  local area=$1 field program
  shift
  program=$(jq_prelude "$area")
  for field in "$@"; do
    program="$program item($(jq_string "$field"); .$field),"
  done
  printf '%s' "${program%,}"
}

# row AREA NAME VALUE: prints one line of the live table.
row() { printf '%s\t%s\t%s\n' "$1" "$2" "$3"; }

# Sources: each prints the live table lines of its areas, plus "meta"
# lines that the writes need, and returns 1 after printing an error.

# The repository: its owner and the Merging and secret scanning settings.
# GitHub leaves the merge settings out of the response unless the token
# may write the repository's contents, and security_and_analysis unless
# it is an admin's, so a missing field reads "unreadable", never false.
read_repo() {
  api GET "repos/$repo" --jq '
    def row($a; $n; $v): [$a, $n, $v] | join("\t");
    def field($a; $n): if has($n) then row($a; $n; .[$n] | tojson) else row($a; $n; "unreadable") end;
    row("meta"; "owner"; "\(.owner.login) \(.owner.id) \(.owner.type)"),
    (("allow_squash_merge", "allow_merge_commit", "allow_rebase_merge", "squash_merge_commit_title",
      "squash_merge_commit_message", "delete_branch_on_merge") as $n | field('"$(jq_string "$area_merging")"'; $n)),
    (("secret_scanning", "secret_scanning_push_protection") as $n
      | if (.security_and_analysis | type) == "object" and (.security_and_analysis | has($n))
        then row('"$(jq_string "$area_secret_scanning")"'; $n; .security_and_analysis[$n].status | tojson)
        else row('"$(jq_string "$area_secret_scanning")"'; $n; "unreadable") end)'
}

read_pvr() {
  api GET "repos/$repo/private-vulnerability-reporting" --jq "$(fields_jq "$area_pvr" enabled)"
}

# Dependabot alerts answer 204 when on and 404 when off.
read_alerts() {
  local rc=0
  api_or_absent GET "repos/$repo/vulnerability-alerts" --silent || rc=$?
  case $rc in
    0) row "$area_alerts" enabled true ;;
    4) row "$area_alerts" enabled false ;;
    *) return 1 ;;
  esac
}

# Dependabot security updates answer 404 when off.
read_security_updates() {
  local rc=0
  api_or_absent GET "repos/$repo/automated-security-fixes" --jq "$(fields_jq "$area_security_updates" enabled paused)" || rc=$?
  case $rc in
    0) ;;
    4) row "$area_security_updates" enabled false && row "$area_security_updates" paused false ;;
    *) return 1 ;;
  esac
}

read_actions() {
  api GET "repos/$repo/actions/permissions" --jq "$(fields_jq "$area_actions" enabled sha_pinning_required),
    ([\"meta\", \"allowed_actions\", (.allowed_actions // \"\" | tostring)] | join(\"\\t\"))"
}

read_workflow() {
  api GET "repos/$repo/actions/permissions/workflow" --jq "$(fields_jq "$area_workflow" default_workflow_permissions can_approve_pull_request_reviews)"
}

read_fork() {
  api GET "repos/$repo/actions/permissions/fork-pr-contributor-approval" --jq "$(fields_jq "$area_fork" approval_policy)"
}

# Immutable releases answer 404 when off.
read_immutable() {
  local rc=0
  api_or_absent GET "repos/$repo/immutable-releases" --jq "$(fields_jq "$area_immutable" enabled)" || rc=$?
  case $rc in
    0) ;;
    4) row "$area_immutable" enabled false ;;
    *) return 1 ;;
  esac
}

# The three rulesets, found by name among the repository's own rulesets.
# Rule types compare as a set; a rule's parameters compare one by one, so
# a parameter the table does not name, such as one GitHub adds later,
# never reads as drift. GitHub can leave bypass_actors out of the
# response, depending on the token, so a missing list reads "unreadable".
read_rulesets() {
  local list name area id
  list=$(api GET "repos/$repo/rulesets?includes_parents=false" --paginate --jq '.[] | [(.id | tostring), .name] | join("\t")') || return 1
  for name in "$ruleset_branches" "$ruleset_tag_creation" "$ruleset_tag_changes" "$ruleset_develop"; do
    area="Ruleset \"$name\""
    id=$(printf '%s\n' "$list" | awk -F'\t' -v n="$name" '$2 == n && id == "" { id = $1 } END { print id }')
    if [[ -z $id ]]; then
      row "$area" exists false
      continue
    fi
    row meta "ruleset:$name" "$id"
    api GET "repos/$repo/rulesets/$id" --jq "$(jq_prelude "$area")"'
      item("exists"; true),
      item("target"; .target),
      item("enforcement"; .enforcement),
      item("include"; (.conditions.ref_name.include // []) | sort),
      item("exclude"; (.conditions.ref_name.exclude // []) | sort),
      (if has("bypass_actors")
        then item("bypass_actors"; [(.bypass_actors // [])[] | {actor_id, actor_type, bypass_mode}] | sort)
        else row("bypass_actors"; "unreadable") end),
      item("rules"; [(.rules // [])[].type] | sort),
      ((.rules // [])[] | .type as $t | (.parameters // {}) | to_entries[]
        | item("\($t).\(.key)"; .value | if type == "array" then sort else . end))' || return 1
  done
}

# The release Environment answers 404 when it does not exist.
read_environment() {
  local rc=0
  api_or_absent GET "repos/$repo/environments/$environment" --jq "$(jq_prelude "$area_environment")"'
    item("exists"; true),
    item("deployment_branch_policy"; .deployment_branch_policy
      | if type == "object" then {custom_branch_policies, protected_branches} else . end),
    item("protection_rules"; [(.protection_rules // [])[].type] | sort),
    item("reviewers"; [(.protection_rules // [])[] | select(.type == "required_reviewers")
      | (.reviewers // [])[] | "\(.type):\(.reviewer.login // .reviewer.slug)"] | sort),
    item("prevent_self_review"; [(.protection_rules // [])[] | select(.type == "required_reviewers")
      | .prevent_self_review] | first)' || rc=$?
  case $rc in
    0) ;;
    4) row "$area_environment" exists false ;;
    *) return 1 ;;
  esac
}

# The release Environment's deployment branch policies; 404 while the
# Environment or its custom branch policies do not exist.
read_branch_policies() {
  local rc=0
  api_or_absent GET "repos/$repo/environments/$environment/deployment-branch-policies" --jq "$(jq_prelude "$area_branches")"'
    item("branch_policies"; [(.branch_policies // [])[] | {name, type}] | sort),
    ((.branch_policies // [])[] | ["meta", "branch_policy", "\(.id) \(.type) \(.name)"] | join("\t"))' || rc=$?
  case $rc in
    0) ;;
    4) row "$area_branches" branch_policies '[]' ;;
    *) return 1 ;;
  esac
}

# source_of AREA: the source that reads AREA.
source_of() {
  case $1 in
    "$area_merging" | "$area_secret_scanning") echo repo ;;
    "$area_pvr") echo pvr ;;
    "$area_alerts") echo alerts ;;
    "$area_security_updates") echo security_updates ;;
    "$area_actions") echo actions ;;
    "$area_workflow") echo workflow ;;
    "$area_fork") echo fork ;;
    "$area_immutable") echo immutable ;;
    "$area_ruleset_branches" | "$area_ruleset_tag_creation" | "$area_ruleset_tag_changes" | "$area_ruleset_develop") echo rulesets ;;
    "$area_environment") echo environment ;;
    "$area_branches") echo branch_policies ;;
  esac
}

# fetch SOURCE: reads SOURCE into $tmp/src.SOURCE, or marks it failed and
# returns 1.
fetch() {
  rm -f "$tmp/src.$1" "$tmp/failed.$1"
  if "read_$1" >"$tmp/new"; then
    mv "$tmp/new" "$tmp/src.$1"
    return 0
  fi
  : >"$tmp/failed.$1"
  return 1
}

# meta KEY: the values of the "meta" lines named KEY, one per line.
meta() {
  awk -F'\t' -v k="$1" '$1 == "meta" && $2 == k { print $3 }' "$tmp"/src.*
}

# meta1 KEY: the first value of the "meta" lines named KEY.
meta1() {
  awk -F'\t' -v k="$1" '$1 == "meta" && $2 == k && !found { print $3; found = 1 }' "$tmp"/src.*
}

# want AREA NAME JSON: one setting of the settings table, as the sources
# print it.
want() { printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$tmp/want"; }

# wants REVIEWER: the settings table
# (docs/engineering/02-repository-layout-and-conventions.md, "Repository
# settings"); REVIEWER is the owner's login, the release Environment's
# required reviewer.
wants() {
  local a
  # Squash merging only, titled by the pull request title with the commit
  # details as its message; head branches deleted after merge.
  a=$area_merging
  want "$a" allow_squash_merge true
  want "$a" allow_merge_commit false
  want "$a" allow_rebase_merge false
  want "$a" squash_merge_commit_title '"PR_TITLE"'
  want "$a" squash_merge_commit_message '"COMMIT_MESSAGES"'
  want "$a" delete_branch_on_merge true
  a=$area_secret_scanning
  want "$a" secret_scanning '"enabled"'
  want "$a" secret_scanning_push_protection '"enabled"'
  want "$area_pvr" enabled true
  want "$area_alerts" enabled true
  a=$area_security_updates
  want "$a" enabled true
  want "$a" paused false
  # Only actions pinned to a full commit SHA; a read-only GITHUB_TOKEN that
  # cannot create or approve pull requests; every outside contributor's
  # fork pull-request workflows wait for approval.
  a=$area_actions
  want "$a" enabled true
  want "$a" sha_pinning_required true
  a=$area_workflow
  want "$a" default_workflow_permissions '"read"'
  want "$a" can_approve_pull_request_reviews false
  want "$area_fork" approval_policy '"all_external_contributors"'
  want "$area_immutable" enabled true
  # main and release-*: the required checks from GitHub Actions on an
  # up-to-date branch, not required when a branch is created, so
  # release-X.Y can be cut from a candidate commit on main, where
  # pr-title never runs; squash only; no review requirement; no bypass
  # actors; deletion and force pushes blocked.
  a=$area_ruleset_branches
  want "$a" exists true
  want "$a" target '"branch"'
  want "$a" enforcement '"active"'
  want "$a" include '["refs/heads/main","refs/heads/release-*"]'
  want "$a" exclude '[]'
  want "$a" bypass_actors '[]'
  want "$a" rules '["deletion","non_fast_forward","pull_request","required_status_checks"]'
  want "$a" pull_request.allowed_merge_methods '["squash"]'
  want "$a" pull_request.dismiss_stale_reviews_on_push false
  want "$a" pull_request.require_code_owner_review false
  want "$a" pull_request.require_last_push_approval false
  want "$a" pull_request.required_approving_review_count 0
  want "$a" pull_request.required_review_thread_resolution false
  want "$a" required_status_checks.do_not_enforce_on_create true
  want "$a" required_status_checks.required_status_checks \
    "[{\"context\":\"pr-fast\",\"integration_id\":$actions_app},{\"context\":\"pr-full\",\"integration_id\":$actions_app},{\"context\":\"pr-title\",\"integration_id\":$actions_app}]"
  want "$a" required_status_checks.strict_required_status_checks_policy true
  # v* tags: a ruleset's bypass list covers all its rules, so one ruleset
  # lets only admins create them and a second, without bypass actors,
  # blocks every update and deletion.
  a=$area_ruleset_tag_creation
  want "$a" exists true
  want "$a" target '"tag"'
  want "$a" enforcement '"active"'
  want "$a" include '["refs/tags/v*"]'
  want "$a" exclude '[]'
  want "$a" bypass_actors "[{\"actor_id\":$admin_role,\"actor_type\":\"RepositoryRole\",\"bypass_mode\":\"always\"}]"
  want "$a" rules '["creation"]'
  a=$area_ruleset_tag_changes
  want "$a" exists true
  want "$a" target '"tag"'
  want "$a" enforcement '"active"'
  want "$a" include '["refs/tags/v*"]'
  want "$a" exclude '[]'
  want "$a" bypass_actors '[]'
  # The update rule of a tag ruleset has no parameters: GitHub keeps
  # update_allows_fetch_and_merge, which lets a branch sync its fork, only
  # on branch rulesets.
  want "$a" rules '["deletion","update"]'
  # develop: nobody deletes it, so the head branch of the develop to main
  # pull requests outlives their merge, which deletes head branches.
  a=$area_ruleset_develop
  want "$a" exists true
  want "$a" target '"branch"'
  want "$a" enforcement '"active"'
  want "$a" include '["refs/heads/develop"]'
  want "$a" exclude '[]'
  want "$a" bypass_actors '[]'
  want "$a" rules '["deletion"]'
  # The release Environment: deployments from v* tags only, the owner as
  # its one required reviewer, who approves the runs of the tags they
  # push, so self-review stays allowed; no wait timer.
  a=$area_environment
  want "$a" exists true
  want "$a" deployment_branch_policy '{"custom_branch_policies":true,"protected_branches":false}'
  want "$a" protection_rules '["branch_policy","required_reviewers"]'
  want "$a" reviewers "[\"User:$1\"]"
  want "$a" prevent_self_review false
  want "$area_branches" branch_policies '[{"name":"v*","type":"tag"}]'
}

# compare AREA: prints how AREA's live settings differ from the table, one
# line each: "missing" when the object does not exist, "drift<TAB>NAME<TAB>
# LIVE<TAB>WANT" for a setting that differs or is absent from the live
# table, and "unreadable<TAB>NAME" for one the response left out.
compare() {
  cat "$tmp"/src.* >"$tmp/live"
  awk -F'\t' -v area="$1" '
    FILENAME == ARGV[1] {
      if ($1 == area) { live[$2] = $3; seen[$2] = 1 }
      next
    }
    $1 != area || missing { next }
    $2 == "exists" && live["exists"] == "false" { print "missing"; missing = 1; next }
    !($2 in seen) { printf "drift\t%s\tabsent\t%s\n", $2, $3; next }
    live[$2] == "unreadable" { printf "unreadable\t%s\n", $2; next }
    live[$2] != $3 { printf "drift\t%s\t%s\t%s\n", $2, live[$2], $3 }
  ' "$tmp/live" "$tmp/want"
}

# describe AREA LINES: prints compare's lines for people, one "drift:" line
# per setting, and returns the number of unreadable settings, each of them
# reported as an error.
describe() {
  local area=$1 kind name live wanted unreadable=0
  while IFS=$'\t' read -r kind name live wanted; do
    case $kind in
      missing) echo "drift: $area: missing" ;;
      drift) echo "drift: $area: $name is $live, want $wanted" ;;
      unreadable)
        # The weekly check's read-only token cannot read every setting
        # (docs/engineering/02-repository-layout-and-conventions.md,
        # "Repository settings"): --check notes each one it skips; an
        # admin's apply reads them all.
        if [[ $mode == check ]]; then
          echo "notice: $area: $name not checked: GitHub leaves it out of the response to this token"
        else
          echo "$prog: $area: cannot read $name: the response leaves it out, so the token may not read it" >&2
          unreadable=$((unreadable + 1))
        fi
        ;;
    esac
  done <<<"$2"
  return "$unreadable"
}

# Writes: each makes one area match the table.

write_merging() {
  api PATCH "repos/$repo" --silent --input - <<'EOF'
{"allow_squash_merge":true,"allow_merge_commit":false,"allow_rebase_merge":false,"squash_merge_commit_title":"PR_TITLE","squash_merge_commit_message":"COMMIT_MESSAGES","delete_branch_on_merge":true}
EOF
}

write_secret_scanning() {
  api PATCH "repos/$repo" --silent --input - <<'EOF'
{"security_and_analysis":{"secret_scanning":{"status":"enabled"},"secret_scanning_push_protection":{"status":"enabled"}}}
EOF
}

write_pvr() { api PUT "repos/$repo/private-vulnerability-reporting" --silent; }

write_alerts() { api PUT "repos/$repo/vulnerability-alerts" --silent; }

write_security_updates() { api PUT "repos/$repo/automated-security-fixes" --silent; }

# The allowed actions policy is not in the table and keeps its value.
write_actions() {
  local allowed
  allowed=$(meta1 allowed_actions)
  case $allowed in
    all | local_only | selected) allowed=",\"allowed_actions\":\"$allowed\"" ;;
    *) allowed= ;;
  esac
  api PUT "repos/$repo/actions/permissions" --silent --input - <<EOF
{"enabled":true$allowed,"sha_pinning_required":true}
EOF
}

write_workflow() {
  api PUT "repos/$repo/actions/permissions/workflow" --silent --input - <<'EOF'
{"default_workflow_permissions":"read","can_approve_pull_request_reviews":false}
EOF
}

write_fork() {
  api PUT "repos/$repo/actions/permissions/fork-pr-contributor-approval" --silent --input - <<'EOF'
{"approval_policy":"all_external_contributors"}
EOF
}

write_immutable() { api PUT "repos/$repo/immutable-releases" --silent; }

# ruleset_body NAME: the ruleset NAME as the table defines it.
ruleset_body() {
  case $1 in
    "$ruleset_branches")
      cat <<EOF
{"name":"$ruleset_branches","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":["refs/heads/main","refs/heads/release-*"],"exclude":[]}},
 "bypass_actors":[],
 "rules":[{"type":"deletion"},{"type":"non_fast_forward"},
  {"type":"pull_request","parameters":{"allowed_merge_methods":["squash"],"dismiss_stale_reviews_on_push":false,
   "require_code_owner_review":false,"require_last_push_approval":false,"required_approving_review_count":0,
   "required_review_thread_resolution":false}},
  {"type":"required_status_checks","parameters":{"do_not_enforce_on_create":true,"strict_required_status_checks_policy":true,
   "required_status_checks":[{"context":"pr-fast","integration_id":$actions_app},{"context":"pr-full","integration_id":$actions_app},
    {"context":"pr-title","integration_id":$actions_app}]}}]}
EOF
      ;;
    "$ruleset_tag_creation")
      cat <<EOF
{"name":"$ruleset_tag_creation","target":"tag","enforcement":"active",
 "conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},
 "bypass_actors":[{"actor_id":$admin_role,"actor_type":"RepositoryRole","bypass_mode":"always"}],
 "rules":[{"type":"creation"}]}
EOF
      ;;
    "$ruleset_tag_changes")
      cat <<EOF
{"name":"$ruleset_tag_changes","target":"tag","enforcement":"active",
 "conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},
 "bypass_actors":[],
 "rules":[{"type":"update"},{"type":"deletion"}]}
EOF
      ;;
    "$ruleset_develop")
      cat <<EOF
{"name":"$ruleset_develop","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":["refs/heads/develop"],"exclude":[]}},
 "bypass_actors":[],
 "rules":[{"type":"deletion"}]}
EOF
      ;;
  esac
}

# write_ruleset NAME: creates the ruleset NAME, or replaces the one with
# that name.
write_ruleset() {
  local id
  id=$(meta1 "ruleset:$1")
  if [[ -z $id ]]; then
    ruleset_body "$1" | api POST "repos/$repo/rulesets" --silent --input -
  else
    ruleset_body "$1" | api PUT "repos/$repo/rulesets/$id" --silent --input -
  fi
}

write_environment() {
  api PUT "repos/$repo/environments/$environment" --silent --input - <<EOF
{"wait_timer":0,"prevent_self_review":false,"reviewers":[{"type":"User","id":$owner_id}],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}
EOF
}

# Keeps one v* tag policy, deletes every other policy, and creates the v*
# tag policy when none exists.
write_branch_policies() {
  local id type name keep=
  while read -r id type name; do
    [[ -n $id ]] || continue
    if [[ -z $keep && $type == tag && $name == "v*" ]]; then
      keep=$id
      continue
    fi
    api DELETE "repos/$repo/environments/$environment/deployment-branch-policies/$id" --silent || return 1
  done <<<"$(meta branch_policy)"
  if [[ -z $keep ]]; then
    api POST "repos/$repo/environments/$environment/deployment-branch-policies" --silent --input - <<'EOF'
{"name":"v*","type":"tag"}
EOF
  fi
}

# write_area AREA: makes AREA match the table.
write_area() {
  case $1 in
    "$area_merging") write_merging ;;
    "$area_secret_scanning") write_secret_scanning ;;
    "$area_pvr") write_pvr ;;
    "$area_alerts") write_alerts ;;
    "$area_security_updates") write_security_updates ;;
    "$area_actions") write_actions ;;
    "$area_workflow") write_workflow ;;
    "$area_fork") write_fork ;;
    "$area_immutable") write_immutable ;;
    "$area_ruleset_branches") write_ruleset "$ruleset_branches" ;;
    "$area_ruleset_tag_creation") write_ruleset "$ruleset_tag_creation" ;;
    "$area_ruleset_tag_changes") write_ruleset "$ruleset_tag_changes" ;;
    "$area_ruleset_develop") write_ruleset "$ruleset_develop" ;;
    "$area_environment") write_environment ;;
    "$area_branches") write_branch_policies ;;
  esac
}

# no_api_steps prints the settings no API covers, which the maintainer
# applies by hand.
no_api_steps() {
  local owner=${repo%%/*}
  cat <<EOF
Steps no API covers, applied by hand after the first candidate's image push
(docs/engineering/04-release-versioning-and-compatibility.md, "Release runbook", step 8);
$prog neither reads nor writes them:
  1. Make the package ghcr.io/$owner/$package public: in its settings,
     https://github.com/users/$owner/packages/container/$package/settings,
     change its visibility to public.
  2. Link the package to the repository $repo: on its page,
     https://github.com/users/$owner/packages/container/package/$package,
     connect the repository $repo.
EOF
}

echo "$prog: $mode $repo"

# The owner, from the repository, is the release Environment's reviewer.
if ! fetch repo; then
  echo "$prog: cannot read the repository $repo" >&2
  exit 2
fi
read -r owner_login owner_id owner_type <<<"$(meta owner)"
if [[ ! ${owner_login:-} =~ ^[A-Za-z0-9-]+$ || ! ${owner_id:-} =~ ^[0-9]+$ ]]; then
  echo "$prog: the repository's owner reads \"$(meta owner)\", not a login and an ID" >&2
  exit 2
fi
if [[ ${owner_type:-} != User ]]; then
  echo "$prog: the release Environment's reviewer is the owner, a user; $owner_login is an account of type ${owner_type:-none}" >&2
  exit 2
fi
: >"$tmp/want"
wants "$owner_login"

errors=0
for src in pvr alerts security_updates actions workflow fork immutable rulesets environment branch_policies; do
  fetch "$src" || errors=$((errors + 1))
done

drift=0 updated=0 unchanged=0
for area in "${areas[@]}"; do
  src=$(source_of "$area")
  # A source that failed to read was reported and counted once.
  [[ ! -e $tmp/failed.$src ]] || continue
  result=$(compare "$area")
  if [[ -z $result ]]; then
    if [[ $mode == apply ]]; then
      echo "unchanged: $area"
      unchanged=$((unchanged + 1))
    fi
    continue
  fi
  rc=0
  describe "$area" "$result" || rc=$?
  errors=$((errors + rc))
  if [[ $mode == check ]]; then
    drift=$((drift + $(printf '%s\n' "$result" | awk -F'\t' '$1 == "drift" || $1 == "missing" { n++ } END { print n + 0 }')))
    continue
  fi
  # An area with a setting the token may not read is not written.
  [[ $rc -eq 0 ]] || continue
  if ! write_area "$area"; then
    echo "$prog: $area: not updated" >&2
    errors=$((errors + 1))
    continue
  fi
  if ! fetch "$src"; then
    errors=$((errors + 1))
    continue
  fi
  result=$(compare "$area")
  if [[ -n $result ]]; then
    echo "$prog: $area: still differs after its write:" >&2
    describe "$area" "$result" >&2 || true
    errors=$((errors + 1))
    continue
  fi
  echo "updated: $area"
  updated=$((updated + 1))
done

if [[ $mode == check ]]; then
  if [[ $drift -eq 0 ]]; then
    echo "$prog: no drift from the settings table"
  else
    echo "$prog: $drift setting(s) drift from the settings table; an admin runs scripts/repo-settings.sh to apply it"
  fi
else
  echo "$prog: $updated area(s) updated, $unchanged unchanged"
fi
no_api_steps
if [[ $errors -gt 0 ]]; then
  echo "$prog: $errors error(s)" >&2
  exit 2
fi
if [[ $drift -gt 0 ]]; then
  exit 1
fi
exit 0

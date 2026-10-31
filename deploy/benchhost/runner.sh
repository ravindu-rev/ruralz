#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# runner.sh is the rh-1 runner service
# (docs/architecture/12-performance-budgets-and-benchmarking.md, "Reference
# hardware"; docs/engineering/01-tech-stack-and-libraries.md, "CI tooling"):
# an ephemeral just-in-time GitHub Actions runner on RH-1, outside
# ruralzd's cpuset. As root, ruralz-bench-runner.service holds a GitHub
# App's private key, which never leaves RH-1, and loops: it signs a JWT as
# the App, takes an installation token, generates the just-in-time
# configuration of one runner labelled self-hosted and rh-1 (at repository
# level, or in an organization runner group limited to the repository with
# --runner-group), and runs that runner as the inventory's dedicated user
# until it has taken one job, then registers the next. Jobs run as that
# user, who cannot read the key, with LimitNOFILE 1,048,576 and the user
# manager's bus, so the harness's systemd-run scopes work.
#
#   runner.sh install --inventory rh-1.json --app-id ID --app-key FILE
#                     --repo OWNER/NAME --runner-version X.Y.Z
#                     --runner-sha256 HEX [--runner-group ID]
#   runner.sh run [--once]
#
# install is idempotent; README.md is the runbook. BENCHHOST_ROOT prefixes
# every file path (the tests run it on a fake tree).
set -euo pipefail

# One collation for sort and comm.
export LC_ALL=C
R=${BENCHHOST_ROOT:-}
api_url=${RUNNER_API_URL:-https://api.github.com}
conf=/etc/ruralz-bench
lib=/usr/local/lib/ruralz-bench
unit=/etc/systemd/system/ruralz-bench-runner.service

log() { printf 'runner.sh: %s\n' "$*" >&2; }
die() {
  log "$*"
  exit 1
}
usage() {
  cat >&2 <<'EOF'
usage: runner.sh install --inventory FILE --app-id ID --app-key FILE --repo OWNER/NAME
                         --runner-version X.Y.Z --runner-sha256 HEX [--runner-group ID]
       runner.sh run [--once]
EOF
  exit 2
}

# b64url encodes stdin as unpadded base64url.
b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

# app_jwt prints a JWT signed with the App's key (RUNNER_APP_KEY) for the
# App RUNNER_APP_ID: issued 60 s in the past against clock drift, expiring
# 9 minutes later, within GitHub's 10-minute limit.
app_jwt() {
  local now header payload sig
  now=$(date +%s) || return
  header=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url) || return
  payload=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' $((now - 60)) $((now + 540)) "$RUNNER_APP_ID" | b64url) || return
  sig=$(printf '%s.%s' "$header" "$payload" | openssl dgst -sha256 -sign "$RUNNER_APP_KEY" -binary | b64url) || return
  printf '%s.%s.%s\n' "$header" "$payload" "$sig"
}

# api METHOD PATH TOKEN [BODY] calls the GitHub REST API.
api() {
  local args=(-fsS --retry 3 -X "$1" -H "Accept: application/vnd.github+json"
    -H "X-GitHub-Api-Version: 2022-11-28" -H "Authorization: Bearer $3")
  if (($# > 3)); then
    args+=(-H "Content-Type: application/json" --data "$4")
  fi
  curl "${args[@]}" "$api_url$2"
}

# jit_config prints the encoded just-in-time configuration of one new
# runner: at repository level in the Default group, or, with
# RUNNER_GROUP_ID, in that organization runner group. Each step returns
# on failure: a command substitution does not inherit set -e.
jit_config() {
  local jwt id token owner=${RUNNER_REPO%%/*} name=${RUNNER_REPO#*/} scope tokens body now
  jwt=$(app_jwt) || return
  now=$(date +%s) || return
  if [[ -n ${RUNNER_GROUP_ID:-} ]]; then
    scope=/orgs/$owner
    tokens='{"permissions":{"organization_self_hosted_runners":"write"}}'
    body=$(jq -cn --arg n "rh-1-$now" --argjson g "$RUNNER_GROUP_ID" \
      '{name: $n, runner_group_id: $g, labels: ["self-hosted", "rh-1"], work_folder: "_work"}') || return
  else
    scope=/repos/$RUNNER_REPO
    tokens=$(jq -cn --arg r "$name" '{repositories: [$r], permissions: {administration: "write"}}') || return
    body=$(jq -cn --arg n "rh-1-$now" \
      '{name: $n, runner_group_id: 1, labels: ["self-hosted", "rh-1"], work_folder: "_work"}') || return
  fi
  id=$(api GET "$scope/installation" "$jwt" | jq -er .id) || return
  token=$(api POST "/app/installations/$id/access_tokens" "$jwt" "$tokens" | jq -er .token) || return
  api POST "$scope/actions/runners/generate-jitconfig" "$token" "$body" | jq -er .encoded_jit_config
}

# run registers one runner at a time and runs it as RUNNER_USER until it
# has taken its job; --once stops after the first runner.
run() {
  local once=0 uid jit
  [[ ${1:-} == --once ]] && once=1
  for v in RUNNER_APP_ID RUNNER_APP_KEY RUNNER_REPO RUNNER_USER RUNNER_DIR; do
    [[ -n ${!v:-} ]] || die "$v is not set (EnvironmentFile $conf/runner.env)"
  done
  uid=$(id -u "$RUNNER_USER")
  while :; do
    if jit=$(jit_config); then
      log "starting a just-in-time runner for $RUNNER_REPO"
      runuser -u "$RUNNER_USER" -- env "XDG_RUNTIME_DIR=/run/user/$uid" \
        "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$uid/bus" \
        "$RUNNER_DIR/run.sh" --jitconfig "$jit" || log "the runner exited with status $?"
    else
      log "no just-in-time configuration from GitHub"
      ((once)) && return 1
      sleep 60
    fi
    ((once)) && return 0
  done
}

install_runner() {
  local inventory="" app_id="" app_key="" repo="" version="" sha="" group=""
  while (($#)); do
    case "$1" in
      --inventory) inventory=${2:-}; shift 2 || usage ;;
      --app-id) app_id=${2:-}; shift 2 || usage ;;
      --app-key) app_key=${2:-}; shift 2 || usage ;;
      --repo) repo=${2:-}; shift 2 || usage ;;
      --runner-version) version=${2:-}; shift 2 || usage ;;
      --runner-sha256) sha=${2:-}; shift 2 || usage ;;
      --runner-group) group=${2:-}; shift 2 || usage ;;
      *) usage ;;
    esac
  done
  [[ -f $inventory ]] || die "--inventory names no file"
  [[ $app_id =~ ^[0-9]+$ ]] || die "--app-id is the App's numeric ID"
  [[ -f $app_key ]] || die "--app-key names no file"
  [[ $repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "--repo is OWNER/NAME"
  [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--runner-version is an actions/runner release X.Y.Z"
  [[ $sha =~ ^[0-9a-f]{64}$ ]] || die "--runner-sha256 is the SHA-256 of that release's linux-x64 archive"
  [[ -z $group || $group =~ ^[0-9]+$ ]] || die "--runner-group is a numeric runner group ID"
  [[ $(jq -er .profile "$inventory") == rh-1 ]] || die "the rh-1 runner runs on the rh-1 profile's gateway host"
  local user cpus group_name online allowed
  user=$(jq -er .sshUser "$inventory")
  cpus=$(jq -er '.hosts.gateway.cpus' "$inventory")
  group_name=$(id -gn "$user")

  # The runner and the harness take the cores outside ruralzd's cpuset.
  online=$(<"$R/sys/devices/system/cpu/online")
  allowed=$(comm -23 <(cpu_list "$online" | sort) <(cpu_list "$cpus" | sort) | sort -n | cpu_join)
  [[ -n $allowed ]] || die "no core is left outside ruralzd's cpuset $cpus"

  local dir=/opt/ruralz-bench/actions-runner-$version archive
  if [[ ! -x $R$dir/run.sh ]]; then
    archive=$(mktemp)
    curl -fsSL -o "$archive" "https://github.com/actions/runner/releases/download/v$version/actions-runner-linux-x64-$version.tar.gz"
    printf '%s  %s\n' "$sha" "$archive" | sha256sum --strict -c - >/dev/null ||
      die "the actions/runner $version archive does not match --runner-sha256"
    mkdir -p "$R$dir"
    tar -xzf "$archive" -C "$R$dir"
    rm -f "$archive"
    "$R$dir/bin/installdependencies.sh"
  fi
  chown -R "$user:$group_name" "$R$dir"
  ln -sfn "$dir" "$R/opt/ruralz-bench/actions-runner"

  mkdir -p "$R$conf" "$R$lib"
  install -m 0600 "$app_key" "$R$conf/github-app.pem"
  install -m 0755 "${BASH_SOURCE[0]}" "$R$lib/runner.sh"
  cat >"$R$conf/runner.env" <<EOF
# deploy/benchhost runner.sh install: the rh-1 runner service's settings.
RUNNER_APP_ID=$app_id
RUNNER_APP_KEY=$conf/github-app.pem
RUNNER_REPO=$repo
RUNNER_GROUP_ID=$group
RUNNER_USER=$user
RUNNER_DIR=/opt/ruralz-bench/actions-runner
EOF
  chmod 0600 "$R$conf/runner.env"
  local uid
  uid=$(id -u "$user")
  mkdir -p "$(dirname "$R$unit")"
  cat >"$R$unit" <<EOF
[Unit]
Description=Ruralz rh-1 just-in-time GitHub Actions runner (deploy/benchhost)
Wants=network-online.target user@$uid.service
After=network-online.target user@$uid.service ruralz-bench-tune.service

[Service]
Type=simple
EnvironmentFile=$conf/runner.env
ExecStart=$lib/runner.sh run
Restart=always
RestartSec=30
AllowedCPUs=$allowed
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
  # The dependencies the runner installed are held like every package.
  comm -23 <(dpkg-query -W -f='${binary:Package}\n' | sort -u) <(apt-mark showhold | sort -u) |
    xargs -r apt-mark hold >/dev/null
  systemctl daemon-reload
  systemctl enable ruralz-bench-runner.service
  systemctl restart ruralz-bench-runner.service
  log "ruralz-bench-runner.service runs actions/runner $version as $user on cores $allowed"
}

# cpu_list LIST prints the CPUs of a cpu list such as 0-3,8 one per line.
cpu_list() {
  local part
  for part in ${1//,/ }; do
    seq "${part%-*}" "${part#*-}"
  done
}

# cpu_join prints the sorted CPUs on stdin as a cpu list such as 0-3,8.
cpu_join() {
  awk 'NR == 1 { lo = hi = $1; next }
       $1 == hi + 1 { hi = $1; next }
       { printf "%s%s,", lo, (hi > lo ? "-" hi : ""); lo = hi = $1 }
       END { if (NR) printf "%s%s\n", lo, (hi > lo ? "-" hi : "") }'
}

main() {
  case "${1:-}" in
    install) shift; install_runner "$@" ;;
    run) shift; run "$@" ;;
    *) usage ;;
  esac
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main "$@"
fi

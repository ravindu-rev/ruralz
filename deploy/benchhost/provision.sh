#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# provision.sh provisions the four lab hosts of a profile inventory from a
# workstation, over ssh (docs/architecture/12-performance-budgets-and-benchmarking.md,
# "Reference hardware"; README.md is the runbook). It needs only ssh
# access as root, or as a user with passwordless sudo, to each host, so it
# works with any provider:
#
#   provision.sh --inventory rh-1.json
#                --admin gateway=DEST --admin load=DEST --admin upstream=DEST --admin statestore=DEST
#                --lab gateway=IPV4 --lab load=IPV4 --lab upstream=IPV4 --lab statestore=IPV4
#                [--ssh-option OPTION]...
#
# DEST is an ssh destination such as root@203.0.113.7; --lab gives each
# host's address on the lab network, which /etc/hosts gives the
# inventory's bench.example names on every host. It runs host.sh on each
# host: first on the gateway host (RH-1) with the other hosts' ssh host keys
# as the harness's known hosts, then on the others with the harness's public
# key, the State Store host also pulling the Valkey image that
# internal/statestore/statestoretest/redisserver pins. It ends by reaching
# every other host from RH-1 as the harness does. A re-run is safe.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

log() { printf 'provision.sh: %s\n' "$*"; }
die() {
  printf 'provision.sh: %s\n' "$*" >&2
  exit 1
}
usage() {
  cat >&2 <<'EOF'
usage: provision.sh --inventory FILE
                    --admin gateway=DEST --admin load=DEST --admin upstream=DEST --admin statestore=DEST
                    --lab gateway=IPV4 --lab load=IPV4 --lab upstream=IPV4 --lab statestore=IPV4
                    [--ssh-option OPTION]...
EOF
  exit 2
}

inventory=""
ssh_opts=(-o BatchMode=yes)
declare -A admin=() lab=()
while (($#)); do
  case "$1" in
    --inventory) inventory=${2:-}; shift 2 || usage ;;
    --admin | --lab)
      [[ ${2:-} == *=* ]] || usage
      if [[ $1 == --admin ]]; then admin[${2%%=*}]=${2#*=}; else lab[${2%%=*}]=${2#*=}; fi
      shift 2
      ;;
    --ssh-option) ssh_opts+=(-o "${2:-}"); shift 2 || usage ;;
    *) usage ;;
  esac
done

roles=(gateway load upstream statestore)
remote=(load upstream statestore)
for tool in jq ssh scp; do
  command -v "$tool" >/dev/null || die "$tool is required on this workstation"
done
[[ -f $inventory ]] || die "--inventory names no file"
for r in "${roles[@]}"; do
  [[ -n ${admin[$r]:-} ]] || die "--admin $r=DEST is missing"
  [[ ${lab[$r]:-} =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "--lab $r=IPV4 is missing"
done
profile=$(jq -er .profile "$inventory")
case "$profile" in rh-1 | rh-3) ;; *) die "inventory profile $profile: only rh-1 and rh-3 are provisioned" ;; esac
user=$(jq -er .sshUser "$inventory")
ssh_key=$(jq -er .sshKey "$inventory")
known_hosts=$(jq -er .knownHosts "$inventory")
valkey_image=$(sed -n 's/^[[:space:]]*ImageValkey9 = "\(.*\)"$/\1/p' "$repo/internal/statestore/statestoretest/redisserver/images.go")
[[ -n $valkey_image ]] || die "no ImageValkey9 in internal/statestore/statestoretest/redisserver/images.go"

lab_args=()
for r in "${roles[@]}"; do lab_args+=(--lab "$r=${lab[$r]}"); done

# on ROLE ARGS... runs a command on the role's host, as root.
on() {
  local dest=${admin[$1]} cmd
  shift
  printf -v cmd '%q ' "$@"
  if [[ $(ssh "${ssh_opts[@]}" "$dest" id -u) != 0 ]]; then
    cmd="sudo -n $cmd"
  fi
  ssh "${ssh_opts[@]}" "$dest" "$cmd"
}

# run_host ROLE [FILE...] -- [HOST.SH ARGS...] copies host.sh, tune.sh, the
# inventory and FILEs to a temporary directory of the role's host and runs
# host.sh there; a FILE argument of host.sh names the copy by base name.
run_host() {
  local r=$1 dir files=() args=()
  shift
  while (($#)) && [[ $1 != -- ]]; do files+=("$1"); shift; done
  shift
  dir=$(ssh "${ssh_opts[@]}" "${admin[$r]}" mktemp -d)
  scp -q "${ssh_opts[@]}" "$here/host.sh" "$here/tune.sh" "$inventory" "${files[@]}" "${admin[$r]}:$dir/"
  for a in "$@"; do
    if [[ $a == @* ]]; then args+=("$dir/${a#@}"); else args+=("$a"); fi
  done
  log "provisioning the $r host ${admin[$r]}"
  on "$r" bash "$dir/host.sh" --inventory "$dir/$(basename "$inventory")" --role "$r" "${lab_args[@]}" "${args[@]}"
  ssh "${ssh_opts[@]}" "${admin[$r]}" rm -rf "$dir"
}

# The harness reaches each other host by its inventory ssh name and checks
# its ed25519 host key.
: >"$work/known_hosts"
for r in "${remote[@]}"; do
  name=$(jq -er --arg r "$r" '.hosts[$r].ssh' "$inventory")
  key=$(on "$r" cat /etc/ssh/ssh_host_ed25519_key.pub)
  [[ $key =~ ^ssh-ed25519\ [A-Za-z0-9+/=]+ ]] || die "the $r host has no ed25519 host key"
  printf '%s %s\n' "$name" "${BASH_REMATCH[0]}" >>"$work/known_hosts"
done

run_host gateway "$work/known_hosts" -- --known-hosts @known_hosts
# The user reads its own key: root would follow a link the user planted.
on gateway runuser -u "$user" -- cat "$ssh_key.pub" >"$work/harness.pub"
for r in "${remote[@]}"; do
  extra=()
  if [[ $r == statestore ]]; then extra=(--valkey-image "$valkey_image"); fi
  run_host "$r" "$work/harness.pub" -- --authorized-key @harness.pub "${extra[@]}"
done

for r in "${remote[@]}"; do
  name=$(jq -er --arg r "$r" '.hosts[$r].ssh' "$inventory")
  on gateway runuser -u "$user" -- ssh -o BatchMode=yes -o StrictHostKeyChecking=yes \
    -o "UserKnownHostsFile=$known_hosts" -i "$ssh_key" -l "$user" "$name" -- true ||
    die "the harness on RH-1 cannot reach $name as $user"
  log "the harness reaches $name as $user"
done
log "$profile provisioned; next: runner.sh install on the gateway host (README.md)"

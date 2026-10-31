#!/usr/bin/env bash
# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# The scripted quickstart (docs/operations/01-deployment-topologies.md,
# "Quickstart"): from a ruralzd release archive to a first proxied request,
# which measures SM-7, 10 minutes or less (target). It runs topology T1's
# single Node without the ruralz CLI and without Go: one file-mode ruralzd
# with the memory State Store in front of a python3 Upstream, on loopback.
#
#   1. Preflight: bash, curl, tar, sha256sum or shasum, and a python3 that
#      runs; linux or darwin on amd64 or arm64; ports 8080, 9000 and 9901
#      free on 127.0.0.1; valid RURALZ_QUICKSTART_* inputs. Exit 2.
#   2. Archive: RURALZ_QUICKSTART_ARCHIVE and the SHA256SUMS beside it, or
#      the host's archive, SHA256SUMS and SHA256SUMS.sigstore.json from the
#      GitHub release RURALZ_QUICKSTART_VERSION names (0.1.0, no v), else
#      from the latest release. cosign 3.1.3 or newer on 3.x, or 2.6.5 or
#      newer on 2.x, verifies SHA256SUMS first when both are present; then
#      the archive is checked against SHA256SUMS and ruralzd is unpacked. A
#      failed download, signature or checksum check exits 1.
#   3. Upstream: python3 -m http.server on 127.0.0.1:9000, serving one
#      known line.
#   4. Node: ruralzd on the quickstart Bundle beside this script, or on the
#      copy below when the script runs alone or piped (curl ... | bash).
#   5. Readiness: GET /readyz on 127.0.0.1:9901 every 0.5 s until 200.
#   6. Success: GET http://127.0.0.1:8080/ until it answers 200 with the
#      known line and x-quickstart: ruralz, which only the Route's headers
#      Policy adds; then it prints "first proxied request after <seconds>
#      s", stops both processes and exits 0. With RURALZ_QUICKSTART_KEEP=1
#      both keep running until SIGINT or SIGTERM, which stops both and
#      exits 0.
#   7. Failure: a process that exits early, or 600 s from the start without
#      success, prints the last /readyz answer and the ruralzd log, stops
#      both processes and exits 1.
#
# It tests a shipped archive, so it runs in real time and waits on
# conditions, never on fixed sleeps. It keeps to bash 3.2, the bash of
# macOS: no associative arrays, no empty-array expansion under set -u.

# The rest of the script is bash; a POSIX shell running it stops here.
if [ -z "${BASH_VERSION:-}" ]; then
  echo "quickstart: run this script with bash" >&2
  exit 2
fi
set -euo pipefail

start=$(date +%s)

readonly prog=quickstart
readonly repo=ravindu-rev/ruralz
readonly listener_port=8080 upstream_port=9000 admin_port=9901
readonly timeout_seconds=600 # step 7 (target)
readonly poll_interval=0.5   # step 5 (target)
readonly upstream_stop_bound=5
readonly drain_start_bound=5
readonly drain_exit_bound=30 # seconds from the first SIGTERM to SIGKILL (target)
readonly known_line="Hello from the Ruralz quickstart Upstream"
# Releases are signed keylessly by the reusable release-build.yml workflow
# at the release tag (docs/engineering/04-release-versioning-and-compatibility.md,
# "Release pipeline").
readonly signer="https://github.com/$repo/.github/workflows/release-build.yml@refs/tags/v"
readonly issuer=https://token.actions.githubusercontent.com
readonly version_re='^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'

archive_input=${RURALZ_QUICKSTART_ARCHIVE:-}
version_input=${RURALZ_QUICKSTART_VERSION:-}
keep=${RURALZ_QUICKSTART_KEEP:-}

deadline=$((start + timeout_seconds))
work=
archive=
upstream_pid=
node_pid=
kept=
readyz_status=
readyz_body=
host_os=
host_arch=
sha256_tool=
cosign_major=

say() { printf '%s: %s\n' "$prog" "$*"; }
warn() { printf '%s: %s\n' "$prog" "$*" >&2; }
now() { date +%s; }

# fail prints why the quickstart failed and exits 1; the EXIT trap stops
# whatever runs.
fail() {
  warn "$*"
  exit 1
}

# dir_of prints the directory part of a path, "." for a bare name.
dir_of() {
  case $1 in
    */*) printf '%s\n' "${1%/*}" ;;
    *) printf '.\n' ;;
  esac
}

# script_dir prints the directory of this script when bash runs it from a
# file; nothing when bash reads it from a pipe or a string.
script_dir() {
  local src=${BASH_SOURCE[0]:-} dir
  if [[ -z $src || ! -f $src ]]; then return 0; fi
  dir=$(dir_of "$src")
  (CDPATH='' cd -P -- "${dir:-/}" && pwd -P)
}

# write_bundle writes the quickstart Bundle to $1: a byte-for-byte copy of
# examples/quickstart/ruralz.yaml and of its block in Deployment topologies
# "Quickstart", which a Go test keeps identical.
write_bundle() {
  cat >"$1" <<'BUNDLE'
apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: quickstart
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: hello
spec:
  protocol: http
  endpoints:
    - address: "127.0.0.1:9000"         # python3 -m http.server, started by quickstart.sh
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: quickstart-header
spec:
  type: headers
  config:
    response:
      set: [{name: x-quickstart, value: ruralz}]   # only the Filter Chain adds it
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: hello
spec:
  match:
    path: {prefix: /}
  policies:
    - name: quickstart-header
  upstreams:
    - name: hello
BUNDLE
}

# busy_ports prints each port of its arguments that cannot be bound on
# 127.0.0.1. Like ruralzd and http.server it sets SO_REUSEADDR, so only a
# bound socket counts, never a connection in TIME_WAIT.
busy_ports() {
  python3 - "$@" <<'PY'
import socket
import sys

for port in sys.argv[1:]:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        s.bind(("127.0.0.1", int(port)))
    except OSError:
        print(port)
    finally:
        s.close()
PY
}

# Step 1. Every problem found is reported before the exit.
preflight() {
  local problems='' tool os arch port name re line
  for tool in curl tar python3; do
    command -v "$tool" >/dev/null 2>&1 || problems="${problems}missing tool: $tool"$'\n'
  done
  if command -v sha256sum >/dev/null 2>&1; then
    sha256_tool=sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    sha256_tool=shasum
  else
    problems="${problems}missing tool: sha256sum or shasum"$'\n'
  fi
  os=$(uname -s)
  arch=$(uname -m)
  case $os in
    Linux) host_os=linux ;;
    Darwin) host_os=darwin ;;
  esac
  case $arch in
    x86_64 | amd64) host_arch=amd64 ;;
    aarch64 | arm64) host_arch=arm64 ;;
  esac
  if [[ -z $host_os || -z $host_arch ]]; then
    problems="${problems}unsupported platform $os/$arch: ruralzd archives exist for Linux and Darwin on amd64 and arm64"$'\n'
  fi
  # The python3 stub of macOS that asks for the Command Line Tools exits
  # non-zero without running anything.
  if command -v python3 >/dev/null 2>&1; then
    if python3 -c 'import http.server, socket' >/dev/null 2>&1; then
      for port in $(busy_ports "$listener_port" "$upstream_port" "$admin_port"); do
        problems="${problems}port $port is busy on 127.0.0.1"$'\n'
      done
    else
      problems="${problems}python3 does not run (on macOS, install the Command Line Tools: xcode-select --install)"$'\n'
    fi
  fi
  case $keep in
    '' | 0 | 1) ;;
    *) problems="${problems}RURALZ_QUICKSTART_KEEP=$keep: want 1, or 0 or empty to stop after the first proxied request"$'\n' ;;
  esac
  if [[ -n $archive_input ]]; then
    name=${archive_input##*/}
    re='^ruralzd_[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?_([a-z0-9]+)_([a-z0-9]+)\.tar\.gz$'
    if [[ ! -f $archive_input ]]; then
      problems="${problems}RURALZ_QUICKSTART_ARCHIVE=$archive_input is not a file"$'\n'
    elif [[ ! $name =~ $re ]]; then
      problems="${problems}RURALZ_QUICKSTART_ARCHIVE=$archive_input is not named ruralzd_<version>_<os>_<arch>.tar.gz"$'\n'
    elif [[ -n $host_os && -n $host_arch && ${BASH_REMATCH[2]}/${BASH_REMATCH[3]} != "$host_os/$host_arch" ]]; then
      problems="${problems}RURALZ_QUICKSTART_ARCHIVE=$archive_input is for ${BASH_REMATCH[2]}/${BASH_REMATCH[3]}, not this $host_os/$host_arch host"$'\n'
    fi
  elif [[ -n $version_input && ! $version_input =~ $version_re ]]; then
    problems="${problems}RURALZ_QUICKSTART_VERSION=$version_input: give a release version without its v, such as 0.1.0 or 0.1.0-rc.1"$'\n'
  fi
  if [[ -n $problems ]]; then
    while IFS= read -r line; do
      if [[ -n $line ]]; then warn "$line"; fi
    done <<<"$problems"
    warn "preflight failed"
    exit 2
  fi
  say "preflight: $host_os/$host_arch with curl, tar, $sha256_tool and python3; ports $listener_port, $upstream_port and $admin_port free on 127.0.0.1"
}

# remaining prints the seconds left before the step 7 deadline, at least 1.
remaining() {
  local left=$((deadline - $(now)))
  if ((left < 1)); then left=1; fi
  printf '%s\n' "$left"
}

# download fetches the HTTPS URL $1 into $2 within the time left.
download() {
  local left rc=0
  left=$(remaining)
  curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --connect-timeout 30 \
    --max-time "$left" --retry-max-time "$left" -o "$2" "$1" || rc=$?
  if ((rc != 0)); then fail "download of $1 failed (curl exit $rc)"; fi
}

# latest_version prints the version of the repository's latest release,
# which is never a pre-release, from the redirect of /releases/latest to
# /releases/tag/v<version>.
latest_version() {
  local left url rc=0 tag
  left=$(remaining)
  url=$(curl -fsS --proto '=https' --tlsv1.2 --retry 3 --connect-timeout 30 \
    --max-time "$left" --retry-max-time "$left" -o /dev/null -w '%{redirect_url}' \
    "https://github.com/$repo/releases/latest") || rc=$?
  if ((rc != 0)); then fail "cannot read the latest release of $repo (curl exit $rc)"; fi
  case $url in
    "https://github.com/$repo/releases/tag/v"*) tag=${url##*/tag/v} ;;
    *) fail "$repo has no release yet: set RURALZ_QUICKSTART_ARCHIVE or RURALZ_QUICKSTART_VERSION" ;;
  esac
  [[ $tag =~ $version_re ]] || fail "the latest release tag of $repo, v$tag, is not a release version"
  printf '%s\n' "$tag"
}

# sha256 prints the SHA-256 of the file $1 in lower-case hex; reading
# standard input keeps sha256sum from escaping unusual file names.
sha256() {
  if [[ $sha256_tool == sha256sum ]]; then
    sha256sum <"$1"
  else
    shasum -a 256 <"$1"
  fi | awk '{ print tolower($1) }'
}

# cosign_admitted reports whether cosign version $1 (vX.Y.Z or X.Y.Z, with
# an optional pre-release) is 3.1.3 or newer on 3.x, or 2.6.5 or newer on
# 2.x, which fix a verification bypass (GHSA-fx35-mq7g-6g98), and sets
# cosign_major.
cosign_admitted() {
  local re='^v?([0-9]+)\.([0-9]+)\.([0-9]+)(-[0-9A-Za-z.-]+)?$' major minor patch pre floor_minor floor_patch
  [[ $1 =~ $re ]] || return 1
  major=$((10#${BASH_REMATCH[1]}))
  minor=$((10#${BASH_REMATCH[2]}))
  patch=$((10#${BASH_REMATCH[3]}))
  pre=${BASH_REMATCH[4]}
  case $major in
    3) floor_minor=1 floor_patch=3 ;;
    2) floor_minor=6 floor_patch=5 ;;
    *) return 1 ;;
  esac
  cosign_major=$major
  if ((minor != floor_minor)); then
    ((minor > floor_minor))
    return
  fi
  if ((patch != floor_patch)); then
    ((patch > floor_patch))
    return
  fi
  # X.Y.Z-pre comes before X.Y.Z.
  [[ -z $pre ]]
}

# verify_signature checks SHA256SUMS ($1) against its Sigstore bundle ($2)
# and the certificate identity of release-build.yml at v$3 when an
# admitted cosign is on PATH; otherwise it says that only checksums are
# checked.
verify_signature() {
  local sums=$1 bundle=$2 version=$3 cv rc=0
  if [[ ! -f $bundle ]]; then
    say "no SHA256SUMS.sigstore.json: only checksums are checked"
    return 0
  fi
  if ! command -v cosign >/dev/null 2>&1; then
    say "no cosign on PATH: only checksums are checked"
    return 0
  fi
  cv=$(cosign version 2>&1 | sed -n 's/^GitVersion:[[:space:]]*//p' | head -n 1) || true
  if ! cosign_admitted "$cv"; then
    say "cosign ${cv:-of an unknown version} is not 3.1.3 or newer on 3.x or 2.6.5 or newer on 2.x (GHSA-fx35-mq7g-6g98): only checksums are checked"
    return 0
  fi
  if [[ $cosign_major == 2 ]]; then
    # cosign 2.x reads a Sigstore bundle only when asked to; 3.x does by
    # default.
    cosign verify-blob --new-bundle-format --bundle "$bundle" \
      --certificate-identity "$signer$version" --certificate-oidc-issuer "$issuer" \
      "$sums" >"$work/cosign.log" 2>&1 || rc=$?
  else
    cosign verify-blob --bundle "$bundle" \
      --certificate-identity "$signer$version" --certificate-oidc-issuer "$issuer" \
      "$sums" >"$work/cosign.log" 2>&1 || rc=$?
  fi
  if ((rc != 0)); then
    cat "$work/cosign.log" >&2
    fail "SHA256SUMS: the signature check against $signer$version failed (cosign $cv, exit $rc)"
  fi
  say "SHA256SUMS: signed by $signer$version (cosign $cv)"
}

# verify_checksum checks the archive $1 against SHA256SUMS ($2), which
# must list it once.
verify_checksum() {
  local name want got count
  name=${1##*/}
  want=$(awk -v f="$name" '{ n = $2; sub(/^\*/, "", n) } n == f { print tolower($1) }' "$2")
  count=$(printf '%s' "$want" | awk 'END { print NR }')
  if ((count != 1)); then fail "SHA256SUMS lists $name $count times, want once"; fi
  got=$(sha256 "$1") || fail "cannot compute the SHA-256 of $1"
  [[ $got == "$want" ]] || fail "checksum mismatch for $name: SHA256SUMS has $want, the archive has $got"
  say "SHA256SUMS: $name matches"
}

# Step 2.
fetch_archive() {
  local version dir base name
  if [[ -n $archive_input ]]; then
    archive=$archive_input
    dir=$(dir_of "$archive")
    name=${archive##*/}
    version=${name#ruralzd_}
    version=${version%_"$host_os"_"$host_arch".tar.gz}
    say "archive: $archive"
  else
    if [[ -n $version_input ]]; then
      version=$version_input
    else
      version=$(latest_version) || exit 1
      say "the latest release is $version"
    fi
    dir=$work/download
    mkdir -p "$dir"
    base="https://github.com/$repo/releases/download/v$version"
    archive=$dir/ruralzd_${version}_${host_os}_${host_arch}.tar.gz
    say "downloading ruralzd $version for $host_os/$host_arch from $base"
    download "$base/${archive##*/}" "$archive"
    download "$base/SHA256SUMS" "$dir/SHA256SUMS"
    download "$base/SHA256SUMS.sigstore.json" "$dir/SHA256SUMS.sigstore.json"
  fi
  [[ -f $dir/SHA256SUMS ]] || fail "no SHA256SUMS beside $archive"
  verify_signature "$dir/SHA256SUMS" "$dir/SHA256SUMS.sigstore.json" "$version"
  verify_checksum "$archive" "$dir/SHA256SUMS"
  mkdir -p "$work/bin"
  if ! tar -xzf "$archive" -C "$work/bin" ruralzd >"$work/tar.log" 2>&1; then
    cat "$work/tar.log" >&2
    fail "cannot unpack ruralzd from $archive"
  fi
  [[ -f $work/bin/ruralzd && -x $work/bin/ruralzd ]] || fail "$archive holds no executable ruralzd"
  say "unpacked ruralzd $version"
}

# Step 3.
start_upstream() {
  mkdir -p "$work/upstream"
  printf '%s\n' "$known_line" >"$work/upstream/index.html"
  (cd "$work/upstream" && exec python3 -u -m http.server --bind 127.0.0.1 "$upstream_port") \
    >"$work/upstream.log" 2>&1 </dev/null &
  upstream_pid=$!
  say "Upstream: python3 -m http.server on 127.0.0.1:$upstream_port (pid $upstream_pid)"
}

# Step 4. The Node gets the quickstart's process settings and only them:
# RURALZ_LOG_LEVEL passes through and every other RURALZ_* variable is
# removed, so a State Store URL, admin credential or secret root in the
# caller's environment cannot change it (docs/_meta/foundation-pack.md
# section 2, "Process settings").
start_node() {
  local dir config name
  dir=$(script_dir)
  if [[ -n $dir && -f $dir/ruralz.yaml ]]; then
    config=$dir
  else
    config=$work/bundle
    mkdir -p "$config"
    write_bundle "$config/ruralz.yaml"
  fi
  mkdir -p "$work/data"
  chmod 700 "$work/data"
  (
    for name in ${!RURALZ_@}; do
      if [[ $name != RURALZ_LOG_LEVEL ]]; then unset "$name"; fi
    done
    export RURALZ_CONFIG="$config" RURALZ_DATA_DIR="$work/data"
    # A Node refuses loopback Endpoints unless the allow-list holds them
    # (docs/architecture/08-security-and-identity.md "Secrets", rule 6).
    export RURALZ_FETCH_ALLOW=127.0.0.1
    # 8080 and 9901 listen on loopback only.
    export RURALZ_BIND_LOOPBACK=true
    exec "$work/bin/ruralzd"
  ) >"$work/ruralzd.log" 2>&1 </dev/null &
  node_pid=$!
  say "Node: ruralzd on the quickstart Bundle in $config (pid $node_pid)"
}

# probe GETs the URL $1 and prints its HTTP status, or 000 when nothing
# answered, leaving the body in $2 and the response header fields in $3.
# A proxy of the caller's environment never sees these loopback requests.
probe() {
  rm -f "$2" "$3"
  curl -sS --noproxy '*' --max-time 5 -o "$2" -D "$3" -w '%{http_code}' "$1" 2>/dev/null || true
}

# has_header reports whether the header file $1 holds a field named $2, in
# any case, whose value is exactly $3.
has_header() {
  tr -d '\r' <"$1" | awk -v n="$2" -v v="$3" '
    { i = index($0, ":") }
    i > 0 && tolower(substr($0, 1, i - 1)) == n {
      x = substr($0, i + 1); sub(/^[ \t]+/, "", x); sub(/[ \t]+$/, "", x)
      if (x == v) found = 1
    }
    END { exit !found }'
}

# print_log prints the log $2 of process $1 to standard error.
print_log() {
  warn "$1 log:"
  if [[ -s $2 ]]; then cat "$2" >&2; else warn "  (empty)"; fi
  warn "end of the $1 log"
}

# report prints what step 7 prints: the reason $1, the last /readyz answer
# and the ruralzd log, then the Upstream log when $2 is upstream.
report() {
  warn "$1"
  if [[ -n $readyz_status ]]; then
    warn "last /readyz answer: $readyz_status $readyz_body"
  else
    warn "last /readyz answer: none, nothing answered on 127.0.0.1:$admin_port"
  fi
  print_log ruralzd "$work/ruralzd.log"
  if [[ ${2:-} == upstream ]]; then print_log Upstream "$work/upstream.log"; fi
}

# check_alive ends the run with step 7 when a process exited early.
check_alive() {
  local status=0
  if [[ -n $upstream_pid ]] && ! kill -0 "$upstream_pid" 2>/dev/null; then
    wait "$upstream_pid" 2>/dev/null || status=$?
    upstream_pid=
    report "the Upstream (python3 -m http.server) exited early with status $status" upstream
    exit 1
  fi
  if [[ -n $node_pid ]] && ! kill -0 "$node_pid" 2>/dev/null; then
    wait "$node_pid" 2>/dev/null || status=$?
    node_pid=
    report "the Node (ruralzd) exited early with status $status"
    exit 1
  fi
}

# check_deadline ends the run with step 7 once 600 s have passed since the
# start.
check_deadline() {
  if (($(now) >= deadline)); then
    report "no first proxied request within $timeout_seconds s"
    exit 1
  fi
}

# Step 5.
await_ready() {
  local status
  while :; do
    check_alive
    status=$(probe "http://127.0.0.1:$admin_port/readyz" "$work/readyz.body" "$work/readyz.headers")
    if [[ $status != 000 ]]; then
      readyz_status=$status
      readyz_body=$(cat "$work/readyz.body" 2>/dev/null || true)
    fi
    if [[ $status == 200 ]]; then
      say "Node ready after $(($(now) - start)) s: $readyz_body"
      return 0
    fi
    check_deadline
    sleep "$poll_interval"
  done
}

# Step 6.
await_first_request() {
  local status
  while :; do
    check_alive
    status=$(probe "http://127.0.0.1:$listener_port/" "$work/reply.body" "$work/reply.headers")
    if [[ $status == 200 ]] && [[ "$(cat "$work/reply.body" 2>/dev/null)" == "$known_line" ]] &&
      has_header "$work/reply.headers" x-quickstart ruralz; then
      say "first proxied request after $(($(now) - start)) s"
      return 0
    fi
    check_deadline
    sleep "$poll_interval"
  done
}

# gone_by waits until process $1 has exited, or until the time $2; false
# when it still runs then.
gone_by() {
  while kill -0 "$1" 2>/dev/null; do
    if (($(now) >= $2)); then return 1; fi
    sleep 0.1
  done
}

# stop_upstream stops http.server with SIGTERM, then SIGKILL after 5 s.
stop_upstream() {
  local pid=$upstream_pid
  upstream_pid=
  kill -TERM "$pid" 2>/dev/null || true
  if ! gone_by "$pid" $(($(now) + upstream_stop_bound)); then
    kill -KILL "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
}

# stop_node stops ruralzd as ruralz dev run does: a SIGTERM starts its
# Drain, a second one, once /readyz reports draining, jumps the Drain to
# its deadline, and SIGKILL follows when it outlives the 30 s exit bound
# (docs/reference/01-cli-and-api-surface.md "Local ruralzd launched by the
# CLI"; docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Drain
# timeline defaults").
stop_node() {
  local pid=$node_pid exit_by drain_by
  node_pid=
  exit_by=$(($(now) + drain_exit_bound))
  drain_by=$(($(now) + drain_start_bound))
  kill -TERM "$pid" 2>/dev/null || true
  # Two signals sent at once can arrive as one, so the second waits for the
  # Drain to start.
  while kill -0 "$pid" 2>/dev/null && (($(now) < drain_by)); do
    probe "http://127.0.0.1:$admin_port/readyz" "$work/stop.body" "$work/stop.headers" >/dev/null
    if grep -q '"draining"' "$work/stop.body" 2>/dev/null; then break; fi
    sleep 0.1
  done
  kill -TERM "$pid" 2>/dev/null || true
  if ! gone_by "$pid" "$exit_by"; then
    warn "ruralzd outlived the $drain_exit_bound s exit bound: SIGKILL"
    kill -KILL "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
}

# stop_all stops whatever still runs.
stop_all() {
  local stopped=
  if [[ -n $upstream_pid ]]; then
    stop_upstream
    stopped="the Upstream"
  fi
  if [[ -n $node_pid ]]; then
    stop_node
    stopped="${stopped:+$stopped and }the Node"
  fi
  if [[ -n $stopped ]]; then say "stopped $stopped"; fi
}

# cleanup runs at every exit; a signal during it is ignored, so both
# processes are always stopped and the temporary directory removed.
cleanup() {
  trap - EXIT
  trap '' INT TERM HUP
  stop_all || true
  if [[ -n $work ]]; then rm -rf "$work"; fi
}

# on_signal ends the run on SIGINT, SIGTERM or SIGHUP. With
# RURALZ_QUICKSTART_KEEP=1 after the first proxied request that is the
# expected end: both processes stop and the exit is 0. Before it, the run
# was interrupted: 128 plus the signal number.
on_signal() {
  trap '' INT TERM HUP
  if [[ -n $kept ]]; then
    stop_all
    exit 0
  fi
  warn "interrupted"
  exit "$1"
}

trap cleanup EXIT
trap 'on_signal 129' HUP
trap 'on_signal 130' INT
trap 'on_signal 143' TERM

preflight
tmp=${TMPDIR:-/tmp}
work=$(mktemp -d "${tmp%/}/ruralz-quickstart.XXXXXX") || fail "cannot create a temporary directory in $tmp"
fetch_archive
start_upstream
start_node
await_ready
await_first_request

if [[ $keep != 1 ]]; then
  stop_all
  exit 0
fi
kept=1
say "RURALZ_QUICKSTART_KEEP=1: the Node and the Upstream keep running. Try:"
say "  curl http://127.0.0.1:$listener_port/"
say "Stop both with Ctrl-C, or with: kill -TERM $$"
while :; do
  check_alive
  sleep 1
done

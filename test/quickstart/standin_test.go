// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package quickstart

import (
	"strconv"
	"strings"
)

// The stand-ins: bash scripts a case writes under the names of the tools
// quickstart.sh calls, which bash starts in a few milliseconds, with or
// without the race detector in the test binary. Each reads the case's
// scenario from scenario.sh in the state directory envState names,
// records the calls it answers in calls.log, and keeps its state there:
//
//   - curl answers the loopback probes from what the stub ruralzd and the
//     python3 stand-in recorded, serves release assets from release/, and
//     resolves no other host;
//   - python3 passes or fails the "python3 runs" check, reports the busy
//     ports the scenario names, and stands in for http.server until
//     SIGTERM;
//   - cosign reports the scenario's version and verifies or refuses;
//   - uname reports the scenario's platform;
//   - sleep advances the simulated clock, in milliseconds, by its argument
//     times the scenario's clock scale, once the Upstream and the Node have
//     started, and date prints that clock, so no case waits in real time.

// envState names the state directory of a case's stand-ins.
const envState = "QUICKSTART_STANDIN_STATE"

// epoch is the simulated clock's reading at zero, in seconds.
const epoch = 1_700_000_000

// unexpected is the answer a stand-in records for a call it does not
// model; every case fails on one.
const unexpected = "unexpected"

// Loopback URLs quickstart.sh probes.
const (
	readyzURL = "http://127.0.0.1:9901/readyz"
	proxyURL  = "http://127.0.0.1:8080/"
)

// The release repository's URLs.
const (
	releases     = "https://github.com/ravindu-rev/ruralz/releases"
	downloadBase = releases + "/download/"
)

// scenario is what a case's stand-ins do.
type scenario struct {
	// UnameS and UnameM are what uname -s and uname -m print; Linux and
	// x86_64 when empty.
	UnameS, UnameM string
	// PythonBroken makes python3 fail as the macOS stub does.
	PythonBroken bool
	// BusyPorts are the ports the port check reports busy.
	BusyPorts []string
	// UpstreamExit makes http.server exit 1 at once.
	UpstreamExit bool
	// NotReadyPolls is how many /readyz polls answer 503 before a ready
	// Node answers 200.
	NotReadyPolls int
	// NoHeader leaves x-quickstart out of the proxied answer, and Body,
	// when set, replaces its body.
	NoHeader bool
	Body     string
	// Latest is the tag /releases/latest redirects to; empty redirects to
	// /releases, as GitHub does for a repository without a release.
	Latest string
	// CosignVersion is the GitVersion cosign version prints, none when
	// empty; CosignFail makes verify-blob refuse.
	CosignVersion string
	CosignFail    bool
	// ClockScale multiplies every simulated sleep; 1 when zero.
	ClockScale int
}

// shQuote quotes s for bash.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func flag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// shell returns the scenario as the variable assignments of scenario.sh.
func (sc scenario) shell() string {
	scale := sc.ClockScale
	if scale == 0 {
		scale = 1
	}
	var b strings.Builder
	for _, kv := range [][2]string{
		{"unameS", sc.UnameS},
		{"unameM", sc.UnameM},
		{"pythonBroken", flag(sc.PythonBroken)},
		{"busyPorts", strings.Join(sc.BusyPorts, " ")},
		{"upstreamExit", flag(sc.UpstreamExit)},
		{"notReadyPolls", strconv.Itoa(sc.NotReadyPolls)},
		{"noHeader", flag(sc.NoHeader)},
		{"replyBody", sc.Body},
		{"latest", sc.Latest},
		{"cosignVersion", sc.CosignVersion},
		{"cosignFail", flag(sc.CosignFail)},
		{"clockScale", strconv.Itoa(scale)},
	} {
		b.WriteString(kv[0] + "=" + shQuote(kv[1]) + "\n")
	}
	return b.String()
}

// call is one call a stand-in answered: a line of calls.log, its fields
// separated by the unit separator.
type call struct {
	Tool, Answer string
	Args         []string
}

// parseCall parses a line of calls.log.
func parseCall(line string) call {
	f := strings.Split(line, "\x1f")
	c := call{Tool: f[0]}
	if len(f) > 1 {
		c.Answer = f[1]
	}
	if len(f) > 2 {
		c.Args = f[2:]
	}
	return c
}

// standInPrelude starts every stand-in: the scenario and the helpers.
const standInPrelude = `#!/usr/bin/env bash
state=$` + envState + `
. "$state/scenario.sh"
# record appends one call: the tool, the answer $1, then the arguments.
record() {
  local line=$tool a
  for a in "$@"; do line="$line"$'\037'"$a"; done
  printf '%s\n' "$line" >>"$state/calls.log"
}
refuse() {
  record ` + unexpected + ` "$@"
  echo "$tool stand-in: unexpected arguments: $*" >&2
  exit 2
}
has() { [ -e "$state/$1" ]; }
# alive reports whether the stub process $1 recorded its pid, has not
# recorded its exit and still runs.
alive() {
  has "$1.pid" && ! has "$1.exited" && kill -0 "$(cat "$state/$1.pid")" 2>/dev/null
}
# listening reports whether the stub ruralzd runs with its stop signal
# handler in place, which it sets before it binds anything.
listening() { alive ruralzd && has ruralzd.trapped; }
`

// standIns maps each tool name to its stand-in script.
func standIns() map[string]string {
	return map[string]string{
		"curl": standInPrelude + `tool=curl
args=("$@")
out='' hdr='' fmt='' url='' fail=''
while [ $# -gt 0 ]; do
  case $1 in
    -o | -D | -w | --proto | --retry | --connect-timeout | --max-time | --retry-max-time | --noproxy)
      [ $# -ge 2 ] || refuse "${args[@]}"
      case $1 in -o) out=$2 ;; -D) hdr=$2 ;; -w) fmt=$2 ;; esac
      shift 2 ;;
    --tlsv1.2) shift ;;
    -*)
      case ${1#-} in '' | *[!fsSL]*) refuse "${args[@]}" ;; esac
      case $1 in *f*) fail=1 ;; esac
      shift ;;
    *)
      [ -z "$url" ] || refuse "${args[@]}"
      url=$1
      shift ;;
  esac
done
[ -n "$url" ] || refuse "${args[@]}"
case $fmt in '' | '%{http_code}' | '%{redirect_url}') ;; *) refuse "${args[@]}" ;; esac
nothing=0 status=0 note='' redirect='' headers='' rbody='' newline=0 asset=''
json='Content-Type: application/json'
case $url in
  ` + readyzURL + `)
    if ! listening; then
      nothing=7
    elif has ruralzd.draining; then
      status=503 note=draining headers=$json rbody='{"status":"not_ready","reasons":[{"reason":"draining"}]}'
    else
      polls=$(( $(cat "$state/readyz.polls" 2>/dev/null || echo 0) + 1 ))
      echo "$polls" >"$state/readyz.polls"
      headers=$json
      if [ "$polls" -gt "$notReadyPolls" ] && has ruralzd.ready; then
        status=200 rbody='{"status":"ready","revision":"rev-0123456789ab"}'
      else
        status=503 rbody='{"status":"not_ready","reasons":[{"reason":"no_revision"}]}'
      fi
    fi ;;
  ` + proxyURL + `)
    if ! listening || ! has ruralzd.ready; then
      nothing=7
    elif ! alive upstream; then
      status=502 headers=$json rbody='{"title":"Bad Gateway","status":502}'
    else
      status=200 headers=$'Content-Type: text/html\r\nServer: SimpleHTTP/0.6 Python/3' newline=1
      [ "$noHeader" = 1 ] || headers="$headers"$'\r\nX-Quickstart: ruralz'
      if [ -n "$replyBody" ]; then rbody=$replyBody; else rbody=$(cat "$state/upstream.index"); fi
    fi ;;
  ` + releases + `/latest)
    status=302 redirect=` + releases + `
    [ -z "$latest" ] || redirect="$redirect/tag/$latest" ;;
  ` + downloadBase + `*)
    asset=$state/release/${url#` + downloadBase + `}
    if [ -f "$asset" ]; then status=200; else status=404 rbody='Not Found' asset=''; fi ;;
  *)
    # The cases have no network: no other host resolves.
    nothing=6 ;;
esac
code=$(printf '%03d' "$status")
record "$code${note:+ $note}" "${args[@]}"
format() {
  case $fmt in
    '%{http_code}') printf '%s' "$code" ;;
    '%{redirect_url}') printf '%s' "$redirect" ;;
  esac
}
if [ "$nothing" != 0 ]; then
  format
  echo "curl: ($nothing) stand-in: nothing answers $url" >&2
  exit "$nothing"
fi
if [ -n "$hdr" ]; then
  {
    printf 'HTTP/1.1 %s stand-in\r\n' "$status"
    [ -z "$headers" ] || printf '%s\r\n' "$headers"
    printf '\r\n'
  } >"$hdr"
fi
if [ -n "$fail" ] && [ "$status" -ge 400 ]; then
  echo "curl: (22) The requested URL returned error: $status" >&2
  exit 22
fi
[ -n "$out" ] || out=/dev/stdout
if [ -n "$asset" ]; then
  cat "$asset" >"$out"
elif [ "$newline" = 1 ]; then
  printf '%s\n' "$rbody" >"$out"
else
  printf '%s' "$rbody" >"$out"
fi
format
`,
		"python3": standInPrelude + `tool=python3
if [ $# -eq 2 ] && [ "$1" = -c ]; then
  record ok "$@"
  if [ "$pythonBroken" = 1 ]; then
    echo "xcode-select: note: No developer tools were found, requesting install." >&2
    exit 1
  fi
  exit 0
fi
if [ "${1:-}" = - ]; then
  cat >/dev/null
  record ok "$@"
  shift
  for port in "$@"; do
    case " $busyPorts " in *" $port "*) echo "$port" ;; esac
  done
  exit 0
fi
[ "$*" = "-u -m http.server --bind 127.0.0.1 9000" ] || refuse "$@"
record ok "$@"
if [ "$upstreamExit" = 1 ]; then
  echo "OSError: [Errno 98] Address already in use" >&2
  : >"$state/upstream.exited"
  exit 1
fi
trap ': >"$state/upstream.exited"; exit 143' TERM
cp index.html "$state/upstream.index"
pwd >"$state/upstream.dir"
echo "$$" >"$state/upstream.pid"
echo "Serving HTTP on 127.0.0.1 port 9000 (http://127.0.0.1:9000/) ..."
while :; do
  command -p sleep 1 &
  wait "$!"
done
`,
		"cosign": standInPrelude + `tool=cosign
if [ "$*" = version ]; then
  echo "  ______   ______        _______. __    _______ .__   __."
  echo "cosign: A tool for Container Signing, Verification and Storage in an OCI registry."
  echo
  [ -z "$cosignVersion" ] || echo "GitVersion:    $cosignVersion"
  echo "GitTreeState:  clean"
  exit 0
fi
[ "${1:-}" = verify-blob ] || refuse "$@"
if [ "$cosignFail" = 1 ]; then
  record refused "$@"
  echo "Error: none of the expected identities matched what was in the certificate" >&2
  exit 1
fi
record verified "$@"
echo "Verified OK" >&2
`,
		"uname": standInPrelude + `tool=uname
case "$*" in
  -s) echo "${unameS:-Linux}" ;;
  -m) echo "${unameM:-x86_64}" ;;
  *) refuse "$@" ;;
esac
`,
		"sleep": standInPrelude + `tool=sleep
[ $# -eq 1 ] || refuse "$@"
case $1 in '' | *[!0-9.]* | *.*.*) refuse "$@" ;; esac
# Simulated time never runs ahead of the real start of the Upstream and the
# Node: wait, up to about 30 s, until each has started or exited.
started() {
  { has upstream.pid || has upstream.exited; } && { has ruralzd.trapped || has ruralzd.exited; }
}
i=0
while ! started && [ "$i" -lt 30000 ]; do
  command -p sleep 0.001
  i=$((i + 1))
done
whole=${1%%.*} part=000
case $1 in *.*) part=${1#*.}000 ;; esac
ms=$(( 10#${whole:-0} * 1000 + 10#${part:0:3} ))
clock=$(cat "$state/clock" 2>/dev/null || echo 0)
echo $(( clock + ms * clockScale )) >"$state/clock"
record ok "$@"
`,
		"date": standInPrelude + `tool=date
[ "$*" = "+%s" ] || refuse "$@"
clock=$(cat "$state/clock" 2>/dev/null || echo 0)
echo $(( ` + strconv.Itoa(epoch) + ` + clock / 1000 ))
`,
	}
}

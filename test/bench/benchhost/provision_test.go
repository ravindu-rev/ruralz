// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package benchhost

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
)

// The admin ssh destinations of the four hosts: root on three, a sudo
// user on the load host.
func admins() map[string]string {
	return map[string]string{"gateway": "root@gw.example", "load": "admin@load.example", "upstream": "root@up.example", "statestore": "root@ss.example"}
}

// hostKeys are the remote hosts' ed25519 host keys.
func hostKeys() map[string]string {
	return map[string]string{"load": "AAAAKEYLOAD", "upstream": "AAAAKEYUP", "statestore": "AAAAKEYSS"}
}

// provisionStandins emulate the hosts behind ssh and scp: id, mktemp, the
// host keys, the harness's public key and host.sh, whose invocations they
// record in order, and the copies scp makes, kept under
// $STUB_STATE/remote/<destination>.
func provisionStandins() map[string]string {
	return map[string]string{
		"ssh": `while [[ ${1:-} == -o ]]; do shift 2; done
dest=$1; shift
remote=$STUB_STATE/remote/$dest
mkdir -p "$remote"
eval "set -- $*"
if [[ $1 == sudo ]]; then shift 2; echo "$dest" >>"$STUB_STATE/sudo"; fi
case "$1" in
  id) if [[ $dest == root@* ]]; then echo 0; else echo 1000; fi ;;
  mktemp) n=$(cat "$STUB_STATE/tmps" 2>/dev/null || echo 0); echo $((n + 1)) >"$STUB_STATE/tmps"; mkdir -p "$remote/tmp/rz.$n"; echo "/tmp/rz.$n" ;;
  cat) case "$2" in
         /etc/ssh/ssh_host_ed25519_key.pub) echo "ssh-ed25519 $(cat "$STUB_STATE/hostkey-$dest") root@host" ;;
         *.pub) echo "` + harnessKey + `" ;;
       esac ;;
  bash) echo "$dest $*" >>"$STUB_STATE/hostsh" ;;
  runuser) echo "$dest $*" >>"$STUB_STATE/verify" ;;
esac
`,
		"scp": `while [[ $1 == -q || $1 == -o ]]; do [[ $1 == -o ]] && shift; shift; done
target=${*: -1}
dir=$STUB_STATE/remote/${target%%:*}${target#*:}
mkdir -p "$dir"
for f in "${@:1:$#-1}"; do cp "$f" "$dir"; done
`,
	}
}

// provisionArgs returns provision.sh's arguments for the committed
// inventory profile.
func provisionArgs(t *testing.T, profile string) []string {
	args := []string{"--inventory", script(t, profile+".json")}
	for _, r := range []string{"gateway", "load", "upstream", "statestore"} {
		args = append(args, "--admin", r+"="+admins()[r], "--lab", r+"="+labIPs()[r])
	}
	return args
}

// provisionRig returns a rig whose stand-ins emulate the four hosts.
func provisionRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, provisionStandins())
	for role, key := range hostKeys() {
		r.writeState("hostkey-"+admins()[role], key+"\n")
	}
	return r
}

// stateLines returns a state file's lines.
func (r *rig) stateLines(name string) []string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.state, name)) //nolint:gosec // G304: the stand-ins' own record
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// remoteFile returns a file a host received by scp, given the host.sh
// argument naming it.
func (r *rig) remoteFile(dest, hostPath string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.state, "remote", dest, filepath.FromSlash(hostPath))) //nolint:gosec // G304: a file of the test's own tree
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

// argAfter returns the word after flag in a recorded command line.
func argAfter(line, flag string) string {
	f := strings.Fields(line)
	if i := slices.Index(f, flag); i >= 0 && i+1 < len(f) {
		return f[i+1]
	}
	return ""
}

// TestProvision: provision.sh runs host.sh on the gateway host first,
// with the other hosts' ed25519 host keys under their inventory ssh names
// as the harness's known hosts, then on RH-1L, RH-1U and RH-1S with the
// harness's public key, RH-1S also pulling the Valkey image redisserver
// pins; every host gets the same scripts and inventory and every role's
// lab address, a non-root admin runs them through sudo, and the run ends
// by reaching each other host from RH-1 as the harness does.
func TestProvision(t *testing.T) {
	r := provisionRig(t)
	r.mustRun("provision.sh", nil, provisionArgs(t, "rh-1")...)

	runs := r.stateLines("hostsh")
	if len(runs) != 4 {
		t.Fatalf("host.sh runs %q, want one per host", runs)
	}
	order := []string{"gateway", "load", "upstream", "statestore"}
	for i, line := range runs {
		role := order[i]
		dest := admins()[role]
		if !strings.HasPrefix(line, dest+" bash ") || argAfter(line, "--role") != role {
			t.Errorf("host.sh run %d = %q, want the %s host (%s)", i, line, role, dest)
			continue
		}
		dir := filepath.Dir(filepath.FromSlash(argAfter(line, "--inventory")))
		for _, name := range []string{"host.sh", "tune.sh", "rh-1.json"} {
			if got := r.remoteFile(dest, filepath.ToSlash(filepath.Join(dir, name))); got != readFile(t, script(t, name)) {
				t.Errorf("%s received a different %s", role, name)
			}
		}
		for _, lab := range order {
			if !strings.Contains(line, " --lab "+lab+"="+labIPs()[lab]) {
				t.Errorf("%s's host.sh run lacks --lab %s: %q", role, lab, line)
			}
		}
		switch role {
		case "gateway":
			want := "load.bench.example ssh-ed25519 AAAAKEYLOAD\nupstream-mock.bench.example ssh-ed25519 AAAAKEYUP\nstatestore.bench.example ssh-ed25519 AAAAKEYSS\n"
			if got := r.remoteFile(dest, argAfter(line, "--known-hosts")); got != want {
				t.Errorf("the harness's known hosts = %q, want %q", got, want)
			}
		default:
			if got := r.remoteFile(dest, argAfter(line, "--authorized-key")); got != harnessKey+"\n" {
				t.Errorf("%s authorizes %q, want the harness's key", role, got)
			}
		}
		if got, want := argAfter(line, "--valkey-image"), map[bool]string{true: redisserver.ImageValkey9}[role == "statestore"]; got != want {
			t.Errorf("%s --valkey-image %q, want %q", role, got, want)
		}
	}
	if got := r.stateLines("sudo"); len(got) == 0 || slices.ContainsFunc(got, func(d string) bool { return d != admins()["load"] }) {
		t.Errorf("sudo ran on %q, want only the non-root admin %s", got, admins()["load"])
	}
	verify := r.stateLines("verify")
	for _, name := range []string{"load.bench.example", "upstream-mock.bench.example", "statestore.bench.example"} {
		want := admins()["gateway"] + " runuser -u ruralz-bench -- ssh -o BatchMode=yes -o StrictHostKeyChecking=yes " +
			"-o UserKnownHostsFile=/home/ruralz-bench/.ssh/known_hosts -i /home/ruralz-bench/.ssh/id_ed25519 -l ruralz-bench " + name + " -- true"
		if !slices.Contains(verify, want) {
			t.Errorf("verifications %q lack %q", verify, want)
		}
	}
}

// TestProvisionRefuses: provision.sh changes no host without every
// role's admin destination and lab address, or for the loopback profile.
func TestProvisionRefuses(t *testing.T) {
	drop := func(args []string, flag, role string) []string {
		var out []string
		for i := 0; i < len(args); i++ {
			if args[i] == flag && strings.HasPrefix(args[i+1], role+"=") {
				i++
				continue
			}
			out = append(out, args[i])
		}
		return out
	}
	cases := []struct {
		name, want string
		args       func(t *testing.T) []string
	}{
		{"no admin", "--admin statestore=DEST is missing", func(t *testing.T) []string { return drop(provisionArgs(t, "rh-1"), "--admin", "statestore") }},
		{"no lab", "--lab load=IPV4 is missing", func(t *testing.T) []string { return drop(provisionArgs(t, "rh-1"), "--lab", "load") }},
		{"loopback", "only rh-1 and rh-3 are provisioned", func(t *testing.T) []string { return provisionArgs(t, "loopback") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := provisionRig(t)
			out, err := r.run("provision.sh", nil, c.args(t)...)
			if err == nil || !strings.Contains(out, c.want) {
				t.Errorf("provision.sh = %v:\n%s\nwant a failure naming %q", err, out, c.want)
			}
			if len(r.stateLines("hostsh")) != 0 {
				t.Error("a refused run provisioned a host")
			}
		})
	}
}

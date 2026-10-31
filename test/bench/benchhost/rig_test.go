// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

// Package benchhost tests the scripts of deploy/benchhost, the benchmark
// hosts' setup (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Reference hardware"), without the hosts: each script runs under bash
// against a fake file tree, which BENCHHOST_ROOT names, with stand-ins for
// the tools that change a host or reach the network (apt-get, systemctl,
// useradd, ethtool, docker, ssh, curl, runuser and others), each recording
// its calls, and with the system's coreutils, jq and openssl for the rest.
// The settings baseline each script applies is asserted from the files it
// writes and the calls it makes, and a second run must change nothing. A
// guard fails any command run as root that names a path under a directory
// the bench user owns, where the user could plant a link.
package benchhost

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// scriptTimeout bounds one script run; the scripts never wait.
const scriptTimeout = 2 * time.Minute

// systemTools are the tools the scripts run for real.
func systemTools() []string {
	return []string{
		"bash", "env", "cat", "sed", "awk", "grep", "sort", "comm", "paste", "seq", "tr", "head", "cut",
		"dirname", "basename", "mktemp", "install", "cmp", "mkdir", "chmod", "rm", "cp", "mv", "ln", "ls",
		"date", "jq", "openssl", "sha256sum", "tar", "gzip", "xargs", "wc", "touch", "printf", "test",
	}
}

// guardedTools are the system tools that read, write or own files: the
// rig wraps each in the guard.
func guardedTools() []string {
	return []string{"cat", "cmp", "chmod", "mkdir", "install", "mv", "cp", "ln", "rm", "touch", "tar"}
}

// optionalTools may be missing; a test that needs one skips.
func optionalTools() []string { return []string{"printf", "touch", "wc", "ls", "cp", "mv", "ln"} }

// repoRoot returns the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no repository root at %s: %v", root, err)
	}
	return root
}

// script returns the path of a deploy/benchhost file.
func script(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "deploy", "benchhost", name)
}

// prelude starts every stand-in: it records the call, one line of
// tab-separated words, in the calls file, then guards it.
const prelude = `#!/usr/bin/env bash
set -euo pipefail
{ printf '%s' "$(basename "$0")"; printf '\t%s' "$@"; printf '\n'; } >>"$STUB_STATE/calls"
` + guard

// guard fails a command root runs, with exit status 97, when it names a
// path under a directory the bench user owns, one per line of
// $STUB_STATE/user-dirs (a line DIR/* stands for every directory in DIR):
// the user can plant a link there that root would follow. The runuser
// stand-in marks the commands it runs, the user's, with STUB_RUNUSER.
const guard = `if [[ ${0##*/} != runuser && -z ${STUB_RUNUSER:-} && -f $STUB_STATE/user-dirs ]]; then
  while IFS= read -r d; do
    for a in "$@"; do
      if [[ $d == */\* ]]; then
        [[ $a == "${d%/\*}"/*/* ]] || continue
      else
        [[ $a == "$d"/* ]] || continue
      fi
      printf '%s: root named %s, under a directory the bench user owns\n' "${0##*/}" "$a" >&2
      exit 97
    done
  done <"$STUB_STATE/user-dirs"
fi
`

// runuserStandin runs its command as runuser -u USER -- COMMAND does,
// marking it the user's for the guard.
const runuserStandin = `export STUB_RUNUSER=$2
while (($#)) && [[ $1 != -- ]]; do shift; done
shift
exec "$@"
`

// wrapper returns a guarded system tool: the guard, then the tool at path.
func wrapper(path string) string {
	return "#!/usr/bin/env bash\nset -euo pipefail\n" + guard + "exec '" + path + "' \"$@\"\n"
}

// rig is one script run's fake host: the file tree, the stand-ins'
// state, and the PATH of stand-ins and system tools.
type rig struct {
	t *testing.T
	// root is BENCHHOST_ROOT, state the stand-ins' state directory and tmp
	// the scripts' TMPDIR.
	root, state, tmp string
	bash             string
	env              []string
}

// newRig builds a rig whose stand-ins are standins (name to the body that
// follows the prelude).
func newRig(t *testing.T, standins map[string]string) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, root: filepath.Join(dir, "root"), state: filepath.Join(dir, "state"), tmp: filepath.Join(dir, "tmp")}
	bin, sys, home := filepath.Join(dir, "standins"), filepath.Join(dir, "system"), filepath.Join(dir, "home")
	for _, d := range []string{r.root, r.state, bin, sys, r.tmp, home} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range standins {
		//nolint:gosec // G306: a stand-in is executable.
		if err := os.WriteFile(filepath.Join(bin, name), []byte(prelude+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range systemTools() {
		p, err := exec.LookPath(name)
		switch {
		case err == nil && slices.Contains(guardedTools(), name):
			//nolint:gosec // G306: a wrapper is executable.
			if err := os.WriteFile(filepath.Join(sys, name), []byte(wrapper(p)), 0o700); err != nil {
				t.Fatal(err)
			}
		case err == nil:
			if err := os.Symlink(p, filepath.Join(sys, name)); err != nil {
				t.Fatal(err)
			}
		case !slices.Contains(optionalTools(), name):
			t.Skipf("%s is not installed", name)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	r.bash = bash
	r.env = []string{
		"PATH=" + bin + ":" + sys, "HOME=" + home, "TMPDIR=" + r.tmp, "LC_ALL=C",
		"BENCHHOST_ROOT=" + r.root, "STUB_STATE=" + r.state, "STUB_SYSTEM=" + sys,
	}
	return r
}

// path returns the fake tree's path of an absolute host path.
func (r *rig) path(hostPath string) string {
	return filepath.Join(r.root, filepath.FromSlash(hostPath))
}

// write writes a file of the fake tree, creating its directory.
func (r *rig) write(hostPath, content string) {
	r.t.Helper()
	p := r.path(hostPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// read returns a file of the fake tree, "" when it is missing.
func (r *rig) read(hostPath string) string {
	r.t.Helper()
	b, err := os.ReadFile(r.path(hostPath))
	if err != nil {
		return ""
	}
	return string(b)
}

// writeState writes a file of the stand-ins' state.
func (r *rig) writeState(name, content string) {
	r.t.Helper()
	p := filepath.Join(r.state, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// userDirs lists, for the guard, the directories the bench user owns; a
// path DIR/* stands for every directory in DIR.
func (r *rig) userDirs(dirs ...string) {
	r.t.Helper()
	r.writeState("user-dirs", strings.Join(dirs, "\n")+"\n")
}

// run runs a deploy/benchhost script with args and extra environment, and
// returns its combined output.
func (r *rig) run(name string, env []string, args ...string) (string, error) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.t.Context(), scriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bash, append([]string{script(r.t, name)}, args...)...) //nolint:gosec // G204: bash runs a deploy/benchhost script
	cmd.Env = append(slices.Clone(r.env), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// source sources a deploy/benchhost script in bash and runs cmd after it,
// returning its standard output.
func (r *rig) source(name, cmd string, env []string) (string, error) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.t.Context(), scriptTimeout)
	defer cancel()
	//nolint:gosec // G204: bash sources a deploy/benchhost script
	c := exec.CommandContext(ctx, r.bash, "-c", `source "$1" && `+cmd, "bash", script(r.t, name))
	c.Env = append(slices.Clone(r.env), env...)
	var stderr strings.Builder
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return string(out), nil
}

// shell runs a command on the rig's PATH and environment, as a script
// would, and returns its combined output.
func (r *rig) shell(args ...string) (string, error) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.t.Context(), scriptTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, r.bash, append([]string{"-c", `"$@"`, "bash"}, args...)...) //nolint:gosec // G204: bash runs a test's command
	c.Env = slices.Clone(r.env)
	out, err := c.CombinedOutput()
	return string(out), err
}

// mustRun runs a script that must succeed.
func (r *rig) mustRun(name string, env []string, args ...string) string {
	r.t.Helper()
	out, err := r.run(name, env, args...)
	if err != nil {
		r.t.Fatalf("%s %q: %v\n%s", name, args, err, out)
	}
	return out
}

// calls returns the recorded calls, each its tool and arguments, and
// clears the record.
func (r *rig) calls() [][]string {
	r.t.Helper()
	p := filepath.Join(r.state, "calls")
	f, err := os.Open(p) //nolint:gosec // G304: the stand-ins' own record
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		out = append(out, strings.Split(sc.Text(), "\t"))
	}
	_ = f.Close()
	if err := sc.Err(); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// called reports whether calls holds one whose words start with want.
func called(calls [][]string, want ...string) bool {
	return slices.ContainsFunc(calls, func(c []string) bool {
		return len(c) >= len(want) && slices.Equal(c[:len(want)], want)
	})
}

// snapshot returns every file of the fake tree with its mode and content.
func (r *rig) snapshot() map[string]string {
	r.t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(r.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(r.root, p)
		if d.IsDir() {
			out[rel] = info.Mode().String()
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // G304: a file of the test's own tree
		if err != nil {
			return err
		}
		out[rel] = info.Mode().String() + "\n" + string(b)
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

// mode returns the permission bits of a file of the fake tree.
func (r *rig) mode(hostPath string) fs.FileMode {
	r.t.Helper()
	info, err := os.Stat(r.path(hostPath))
	if err != nil {
		r.t.Fatal(err)
	}
	return info.Mode().Perm()
}

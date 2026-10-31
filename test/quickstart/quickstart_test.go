// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

// Package quickstart tests examples/quickstart/quickstart.sh, the scripted
// quickstart of docs/operations/01-deployment-topologies.md "Quickstart",
// step by step and without the network: stand-in scripts take the place of
// curl, python3, cosign, uname, sleep and date, and each case's archives
// hold a stub ruralzd that records how the script started it. The
// stand-in sleep advances a simulated clock that the stand-in date reads,
// so the 0.5 s polls and the 600 s deadline run without waiting, and no
// case binds a port but the one that holds 127.0.0.1:9000 for the real
// port check. The stage 10 quickstart jobs run the script against each
// real ruralzd archive
// (docs/engineering/02-repository-layout-and-conventions.md "CI stages").
package quickstart

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// What quickstart.sh prints and checks.
const (
	knownLine = "Hello from the Ruralz quickstart Upstream"
	signer    = "https://github.com/ravindu-rev/ruralz/.github/workflows/release-build.yml@refs/tags/v"
	issuer    = "https://token.actions.githubusercontent.com"
)

// runTimeout bounds one run of the script in real time; a run takes well
// under a second, so only a hung script reaches it.
const runTimeout = 2 * time.Minute

// Stub ruralzd modes.
const (
	// stubServe becomes ready and stops on a second SIGTERM, as a Node's
	// Drain jumps to its deadline.
	stubServe = "serve"
	// stubHang never becomes ready.
	stubHang = "hang"
	// stubExit exits 3 at once, after a log line.
	stubExit = "exit"
	// stubStubborn becomes ready and ignores SIGTERM.
	stubStubborn = "stubborn"
)

// systemTools are the tools quickstart.sh and the stub ruralzd use besides
// the stand-ins; tar may call gzip, and the script needs sha256sum or
// shasum.
func systemTools() []string {
	return []string{"bash", "env", "mktemp", "rm", "mkdir", "chmod", "cat", "cp", "ls", "tar", "gzip", "awk", "sed", "head", "tr", "grep", "sha256sum", "shasum"}
}

// optionalTools may be missing from the host.
func optionalTools() []string { return []string{"gzip", "sha256sum", "shasum"} }

// setup is how a case prepares its run.
type setup struct {
	sc scenario
	// without leaves these tools, stand-ins or system tools, off PATH.
	without []string
	// cosign puts the cosign stand-in on PATH.
	cosign bool
	// realPython puts the host's python3 on PATH instead of the stand-in.
	realPython bool
}

// qcase is one case: its directories, its PATH of stand-ins and system
// tools, and the environment the script runs with.
type qcase struct {
	t    *testing.T
	repo string
	// dir is the script's working directory; state is the stand-ins'
	// state directory; tmp is the script's TMPDIR.
	dir, state, tmp string
	bash            string
	env             []string
}

// repoRoot returns the repository root, with symbolic links resolved as
// the script's pwd -P resolves them.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no repository root at %s: %v", root, err)
	}
	return root
}

// script returns the repository's quickstart.sh, beside ruralz.yaml.
func (c *qcase) script() string {
	return filepath.Join(c.repo, "examples", "quickstart", "quickstart.sh")
}

func newCase(t *testing.T, s setup) *qcase {
	t.Helper()
	root := t.TempDir()
	c := &qcase{
		t: t, repo: repoRoot(t),
		dir: filepath.Join(root, "work"), state: filepath.Join(root, "state"), tmp: filepath.Join(root, "tmp"),
	}
	standins, system := filepath.Join(root, "standins"), filepath.Join(root, "system")
	for _, d := range []string{c.dir, c.state, c.tmp, standins, system, filepath.Join(c.state, "release")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	c.writeFile(filepath.Join(c.state, "scenario.sh"), []byte(s.sc.shell()))
	for name, body := range standIns() {
		switch {
		case slices.Contains(s.without, name) || (name == "cosign" && !s.cosign):
		case name == "python3" && s.realPython:
			p, err := exec.LookPath("python3")
			if err != nil {
				t.Skip("python3 not installed")
			}
			link(t, p, filepath.Join(standins, name))
		default:
			writeExecutable(t, filepath.Join(standins, name), []byte(body))
		}
	}
	for _, name := range systemTools() {
		if slices.Contains(s.without, name) {
			continue
		}
		p, err := exec.LookPath(name)
		switch {
		case err == nil:
			link(t, p, filepath.Join(system, name))
		case !slices.Contains(optionalTools(), name):
			t.Skipf("%s not installed", name)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	c.bash = bash
	c.env = []string{
		"PATH=" + standins + ":" + system, "HOME=" + root, "TMPDIR=" + c.tmp, "LC_ALL=C",
		envState + "=" + c.state,
	}
	t.Cleanup(c.stopLeftovers)
	return c
}

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
}

// writeFile writes a file of the case with mode 0600.
func (c *qcase) writeFile(name string, data []byte) {
	c.t.Helper()
	if err := os.WriteFile(name, data, 0o600); err != nil {
		c.t.Fatal(err)
	}
}

// writeExecutable writes an executable with mode 0700 while it holds
// syscall.ForkLock: a child that a parallel test forks while the file is
// open for writing keeps it open until its own exec, and an exec of the
// file in that time fails with "text file busy".
func writeExecutable(t *testing.T, name string, data []byte) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(name, data, 0o700); err != nil { //nolint:gosec // G306: the file is executable.
		t.Fatal(err)
	}
}

// readFile returns a file the case or the script wrote.
func readFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name) //nolint:gosec // G304: a file of the case's directories or the repository.
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// stateFile returns a file of the state directory, "" when absent.
func (c *qcase) stateFile(name string) string {
	data, err := os.ReadFile(filepath.Join(c.state, name)) //nolint:gosec // G304: a file of the case's state directory.
	if err != nil {
		return ""
	}
	return string(data)
}

// pid returns the pid a stub process (ruralzd or upstream) recorded.
func (c *qcase) pid(name string) (int, bool) {
	pid, err := strconv.Atoi(strings.TrimSpace(c.stateFile(name + ".pid")))
	return pid, err == nil && pid > 0
}

// stopLeftovers kills what a failed case may leave running.
func (c *qcase) stopLeftovers() {
	for _, name := range []string{"ruralzd", "upstream"} {
		if pid, ok := c.pid(name); ok {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// stub returns the stub ruralzd of the case in mode: a bash script that
// records its environment, its data directory and its Bundle, and answers
// SIGTERM as a Node's Drain does, the first starting the Drain and the
// second ending the process.
func (c *qcase) stub(mode string) string {
	return `#!/usr/bin/env bash
# Stub ruralzd of the quickstart tests.
state='` + c.state + `'
mode=` + mode + `
echo "$$" >"$state/ruralzd.pid"
env >"$state/ruralzd.env"
ls -ld "$RURALZ_DATA_DIR" >"$state/ruralzd.datadir" 2>&1
cp "$RURALZ_CONFIG/ruralz.yaml" "$state/ruralzd.bundle" 2>/dev/null
echo '{"level":"INFO","msg":"stub ruralzd starting"}'
if [ "$mode" = exit ]; then
  echo '{"level":"ERROR","msg":"listener http: bind 127.0.0.1:8080: address already in use"}'
  : >"$state/ruralzd.exited"
  exit 3
fi
terms=0
on_term() {
  terms=$((terms + 1))
  echo "$terms" >"$state/ruralzd.terms"
  if [ "$terms" -eq 1 ]; then
    : >"$state/ruralzd.draining"
  else
    : >"$state/ruralzd.exited"
    exit 0
  fi
}
if [ "$mode" = stubborn ]; then
  trap 'echo ignored >>"$state/ruralzd.ignored"' TERM
else
  trap on_term TERM
fi
: >"$state/ruralzd.trapped"
if [ "$mode" != hang ]; then : >"$state/ruralzd.ready"; fi
# wait returns as soon as a trapped signal arrives, so each SIGTERM runs
# its trap at once.
while :; do
  command -p sleep 1 &
  wait "$!"
done
`
}

// tarGz returns a gzip-compressed tar holding members by name, ruralzd
// executable.
func tarGz(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		mode := int64(0o644)
		if name == "ruralzd" {
			mode = 0o755
		}
		h := &tar.Header{Name: name, Mode: mode, Size: int64(len(members[name])), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, ModTime: time.Unix(epoch, 0)}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, members[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// release writes into dir the archive ruralzd_<version>_<platform>.tar.gz,
// holding a stub ruralzd in mode (none when mode is empty) beside the
// other archive members, its SHA256SUMS and, when signed,
// SHA256SUMS.sigstore.json. It returns the archive's path.
func (c *qcase) release(dir, version, platform, mode string, signed bool) string {
	c.t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		c.t.Fatal(err)
	}
	members := map[string]string{"LICENSE": "Apache License\n", "README.md": "# Ruralz\n"}
	if mode != "" {
		members["ruralzd"] = c.stub(mode)
	}
	name := "ruralzd_" + version + "_" + platform + ".tar.gz"
	data := tarGz(c.t, members)
	c.writeFile(filepath.Join(dir, name), data)
	// The CLI archive's line comes first, so the script must pick its own.
	sums := sha256Hex([]byte("cli")) + "  ruralz_" + version + "_" + platform + ".tar.gz\n" +
		sha256Hex(data) + "  " + name + "\n"
	c.writeFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums))
	if signed {
		c.writeFile(filepath.Join(dir, "SHA256SUMS.sigstore.json"), []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`+"\n"))
	}
	return filepath.Join(dir, name)
}

// localRelease writes a linux/amd64 release of 0.1.0 into the case's
// release directory and returns the archive's path.
func (c *qcase) localRelease(mode string, signed bool) string {
	return c.release(filepath.Join(c.dir, "release"), "0.1.0", "linux_amd64", mode, signed)
}

// result is how a run ended.
type result struct {
	code           int
	stdout, stderr string
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	default:
		t.Fatalf("quickstart.sh: %v", err)
		return 0
	}
}

// command returns bash running args with the case's environment plus env.
func (c *qcase) command(ctx context.Context, args []string, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.bash, args...) //nolint:gosec // G204: bash runs quickstart.sh, from the repository or copied by the case.
	cmd.Dir = c.dir
	cmd.Env = append(slices.Clone(c.env), env...)
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

// run runs script to its end with env added, then checks what every run
// leaves (after).
func (c *qcase) run(script string, env ...string) result {
	c.t.Helper()
	return c.runWith(nil, []string{script}, env)
}

// runWith runs bash with args and stdin to its end.
func (c *qcase) runWith(stdin io.Reader, args, env []string) result {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), runTimeout)
	defer cancel()
	cmd := c.command(ctx, args, env)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r := result{code: exitCode(c.t, cmd.Run()), stdout: stdout.String(), stderr: stderr.String()}
	c.after(r)
	return r
}

// running is a run in progress.
type running struct {
	c      *qcase
	cmd    *exec.Cmd
	cancel context.CancelFunc
	lines  chan string
	done   chan struct{}
	// mu guards stdout, which the reader writes until done.
	mu     sync.Mutex
	stdout strings.Builder
	stderr bytes.Buffer
}

// start starts script with env added.
func (c *qcase) start(script string, env ...string) *running {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), runTimeout)
	r := &running{c: c, cmd: c.command(ctx, []string{script}, env), cancel: cancel, lines: make(chan string, 1024), done: make(chan struct{})}
	r.cmd.Stderr = &r.stderr
	out, err := r.cmd.StdoutPipe()
	if err != nil {
		cancel()
		c.t.Fatal(err)
	}
	if err := r.cmd.Start(); err != nil {
		cancel()
		c.t.Fatal(err)
	}
	go func() {
		defer close(r.done)
		defer close(r.lines)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			r.mu.Lock()
			r.stdout.WriteString(sc.Text() + "\n")
			r.mu.Unlock()
			select {
			case r.lines <- sc.Text():
			default:
			}
		}
	}()
	return r
}

// await reads standard output up to a line holding want.
func (r *running) await(want string) {
	r.c.t.Helper()
	for line := range r.lines {
		if strings.Contains(line, want) {
			return
		}
	}
	r.c.t.Fatalf("quickstart.sh ended without printing %q", want)
}

// awaitState waits until the state files names exist, as a condition
// poll under runTimeout.
func (r *running) awaitState(names ...string) {
	r.c.t.Helper()
	deadline := time.Now().Add(runTimeout)
	for _, name := range names {
		for !r.c.exists(name) {
			if time.Now().After(deadline) {
				r.c.t.Fatalf("the state file %s never appeared", name)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func (r *running) signal(sig syscall.Signal) {
	r.c.t.Helper()
	if err := r.cmd.Process.Signal(sig); err != nil {
		r.c.t.Fatal(err)
	}
}

// wait waits for the end of the run, then checks what every run leaves.
func (r *running) wait() result {
	r.c.t.Helper()
	<-r.done
	err := r.cmd.Wait()
	r.cancel()
	r.mu.Lock()
	res := result{code: exitCode(r.c.t, err), stdout: r.stdout.String(), stderr: r.stderr.String()}
	r.mu.Unlock()
	r.c.after(res)
	return res
}

// exists reports whether the state file name exists.
func (c *qcase) exists(name string) bool {
	_, err := os.Stat(filepath.Join(c.state, name))
	return err == nil
}

// calls returns every call the stand-ins answered, in order.
func (c *qcase) calls() []call {
	c.t.Helper()
	var out []call
	for _, line := range strings.Split(c.stateFile("calls.log"), "\n") {
		if line != "" {
			out = append(out, parseCall(line))
		}
	}
	return out
}

// callsOf returns the calls the stand-in tool answered.
func (c *qcase) callsOf(tool string) []call {
	var out []call
	for _, cl := range c.calls() {
		if cl.Tool == tool {
			out = append(out, cl)
		}
	}
	return out
}

// after checks what every run leaves: no stand-in met a call it does not
// model, neither stub process still runs, and the script's temporary
// directory is gone.
func (c *qcase) after(r result) {
	t := c.t
	t.Helper()
	for _, cl := range c.calls() {
		if cl.Answer == unexpected {
			t.Errorf("quickstart.sh called %s %q, which the stand-ins do not model", cl.Tool, cl.Args)
		}
	}
	for _, name := range []string{"ruralzd", "upstream"} {
		if pid, ok := c.pid(name); ok && syscall.Kill(pid, 0) == nil {
			t.Errorf("the stub %s (pid %d) still runs after quickstart.sh exited", name, pid)
		}
	}
	if ents, err := os.ReadDir(c.tmp); err != nil || len(ents) > 0 {
		t.Errorf("quickstart.sh left %d entries in TMPDIR (%v)", len(ents), err)
	}
	if t.Failed() {
		t.Logf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
}

// expect checks a run's exit status and that its standard output and
// standard error hold the given lines or fragments.
func expect(t *testing.T, r result, code int, stdout, stderr []string) {
	t.Helper()
	if r.code != code {
		t.Errorf("exit %d, want %d", r.code, code)
	}
	for _, w := range stdout {
		if !strings.Contains(r.stdout, w) {
			t.Errorf("standard output lacks %q", w)
		}
	}
	for _, w := range stderr {
		if !strings.Contains(r.stderr, w) {
			t.Errorf("standard error lacks %q", w)
		}
	}
	if t.Failed() {
		t.Logf("stdout:\n%s\nstderr:\n%s", r.stdout, r.stderr)
	}
}

// nodeEnv returns the environment the stub ruralzd started with.
func (c *qcase) nodeEnv() map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(c.stateFile("ruralzd.env"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return env
}

// startedNothing checks that the run started neither the Upstream nor the
// Node.
func (c *qcase) startedNothing() {
	c.t.Helper()
	for _, f := range []string{"ruralzd.pid", "ruralzd.exited", "upstream.pid", "upstream.exited"} {
		if c.exists(f) {
			c.t.Errorf("the run started a process (%s), want none", f)
		}
	}
}

// stoppedNode checks that the stub ruralzd got two SIGTERMs and exited on
// the second, as a Node's Drain jumps to its deadline.
func (c *qcase) stoppedNode() {
	c.t.Helper()
	if got := strings.TrimSpace(c.stateFile("ruralzd.terms")); got != "2" {
		c.t.Errorf("the stub ruralzd got %q SIGTERMs, want 2", got)
	}
	if !c.exists("ruralzd.exited") {
		c.t.Error("the stub ruralzd did not exit on its second SIGTERM")
	}
}

// drainedNode checks stoppedNode and that the second SIGTERM followed a
// /readyz answer that reported the Drain, which the first one started.
func (c *qcase) drainedNode() {
	c.t.Helper()
	c.stoppedNode()
	for _, cl := range c.callsOf("curl") {
		if slices.Contains(cl.Args, readyzURL) && cl.Answer == "503 draining" {
			return
		}
	}
	c.t.Error("quickstart.sh sent the second SIGTERM before /readyz reported the Drain")
}

// stoppedUpstream checks that the Upstream stand-in exited on SIGTERM.
func (c *qcase) stoppedUpstream() {
	c.t.Helper()
	if !c.exists("upstream.exited") {
		c.t.Error("the Upstream did not exit")
	}
}

// proxiedLine matches the line step 6 prints, which make quickstart holds
// against QUICKSTART_MAX_SECONDS.
func proxiedLine() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^quickstart: first proxied request after [0-9]+ s$`)
}

// sha256Tool is the tool the script uses on this host.
func sha256Tool() string {
	if _, err := exec.LookPath("sha256sum"); err == nil {
		return "sha256sum"
	}
	return "shasum"
}

// checkStarted checks how the run started the Upstream and the Node, and
// returns the Node's RURALZ_CONFIG.
func (c *qcase) checkStarted(r result) string {
	t := c.t
	t.Helper()
	ups := c.callsOf("python3")
	if len(ups) == 0 || !slices.Equal(ups[len(ups)-1].Args, []string{"-u", "-m", "http.server", "--bind", "127.0.0.1", "9000"}) {
		t.Errorf("the Upstream started as %v, want python3 -u -m http.server --bind 127.0.0.1 9000", ups)
	}
	if got := c.stateFile("upstream.index"); got != knownLine+"\n" {
		t.Errorf("the Upstream served index.html %q, want the known line", got)
	}
	env := c.nodeEnv()
	for k, want := range map[string]string{"RURALZ_FETCH_ALLOW": "127.0.0.1", "RURALZ_BIND_LOOPBACK": "true", "RURALZ_LOG_LEVEL": "debug"} {
		if env[k] != want {
			t.Errorf("the Node's %s = %q, want %q", k, env[k], want)
		}
	}
	for _, k := range []string{"RURALZ_STATE_STORE_URL", "RURALZ_ADMIN_TLS_DIR", "RURALZ_ADMIN_TOKEN_FILE", "RURALZ_SECRET_ROOT", "RURALZ_QUICKSTART_ARCHIVE", "RURALZ_QUICKSTART_VERSION"} {
		if v, ok := env[k]; ok {
			t.Errorf("the Node inherited %s=%s", k, v)
		}
	}
	data := env["RURALZ_DATA_DIR"]
	if !strings.HasPrefix(data, c.tmp+"/ruralz-quickstart.") || filepath.Base(data) != "data" {
		t.Errorf("the Node's RURALZ_DATA_DIR = %q, want a data directory in a temporary directory of %s", data, c.tmp)
	}
	if ls := c.stateFile("ruralzd.datadir"); !strings.HasPrefix(ls, "drwx------") {
		t.Errorf("the Node's data directory is %q, want mode 0700", ls)
	}
	for _, cl := range c.callsOf("curl") {
		if slices.Contains(cl.Args, readyzURL) || slices.Contains(cl.Args, proxyURL) {
			if !containsSeq(cl.Args, "--noproxy", "*") || !containsSeq(cl.Args, "--max-time", "5") {
				t.Errorf("a loopback probe ran as curl %q, want --noproxy '*' and --max-time 5", cl.Args)
			}
		}
	}
	if !proxiedLine().MatchString(r.stdout) {
		t.Errorf("standard output lacks the line %q", "quickstart: first proxied request after <seconds> s")
	}
	return env["RURALZ_CONFIG"]
}

// containsSeq reports whether args holds a then b.
func containsSeq(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

// TestFirstProxiedRequest runs the success path of steps 2 to 6 from a
// local archive, with the script beside ruralz.yaml, alone, and piped to
// bash: the Upstream serves the known line on 127.0.0.1:9000; the Node
// runs on the Bundle beside the script or on the copy the script writes,
// which equals examples/quickstart/ruralz.yaml, with a temporary
// RURALZ_DATA_DIR of mode 0700, RURALZ_FETCH_ALLOW=127.0.0.1,
// RURALZ_BIND_LOOPBACK=true and RURALZ_LOG_LEVEL kept, every other
// RURALZ_* setting removed; the script prints the first proxied request,
// stops the Upstream and the Node, whose Drain the second SIGTERM jumps
// to its deadline, removes its temporary directory and exits 0.
func TestFirstProxiedRequest(t *testing.T) {
	t.Parallel()
	inherited := []string{"RURALZ_LOG_LEVEL=debug", "RURALZ_STATE_STORE_URL=redis://cache.example:6379", "RURALZ_ADMIN_TLS_DIR=/etc/ruralz/admin-tls", "RURALZ_ADMIN_TOKEN_FILE=/etc/ruralz/admin-token", "RURALZ_SECRET_ROOT=/"}
	for _, tc := range []struct {
		name string
		// how runs the script: beside ruralz.yaml, alone, or piped.
		how string
	}{
		{"beside ruralz.yaml", "beside"},
		{"alone", "alone"},
		{"piped to bash", "piped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCase(t, setup{})
			env := append([]string{"RURALZ_QUICKSTART_ARCHIVE=" + c.localRelease(stubServe, false)}, inherited...)
			var r result
			switch tc.how {
			case "beside":
				r = c.run(c.script(), env...)
			case "alone":
				alone := filepath.Join(c.dir, "alone", "quickstart.sh")
				if err := os.MkdirAll(filepath.Dir(alone), 0o750); err != nil {
					t.Fatal(err)
				}
				writeExecutable(t, alone, []byte(readFile(t, c.script())))
				r = c.run(alone, env...)
			default:
				// A ruralz.yaml in the working directory is not the Bundle.
				c.writeFile(filepath.Join(c.dir, "ruralz.yaml"), []byte("kind: NotTheQuickstart\n"))
				r = c.runWith(strings.NewReader(readFile(t, c.script())), nil, env)
			}
			expect(t, r, 0, []string{
				"quickstart: preflight: linux/amd64 with curl, tar, " + sha256Tool() + " and python3; ports 8080, 9000 and 9901 free on 127.0.0.1",
				"quickstart: SHA256SUMS: ruralzd_0.1.0_linux_amd64.tar.gz matches",
				"quickstart: Upstream: python3 -m http.server on 127.0.0.1:9000",
				"quickstart: Node ready after ",
				"quickstart: stopped the Upstream and the Node",
			}, nil)
			config := c.checkStarted(r)
			wantBundle := readFile(t, filepath.Join(c.repo, "examples", "quickstart", "ruralz.yaml"))
			if got := c.stateFile("ruralzd.bundle"); got != wantBundle {
				t.Errorf("the Node's Bundle differs from examples/quickstart/ruralz.yaml:\n%s", got)
			}
			if tc.how == "beside" {
				if want := filepath.Join(c.repo, "examples", "quickstart"); config != want {
					t.Errorf("RURALZ_CONFIG = %q, want the script's own directory %s", config, want)
				}
			} else if !strings.HasPrefix(config, c.tmp+"/ruralz-quickstart.") || filepath.Base(config) != "bundle" {
				t.Errorf("RURALZ_CONFIG = %q, want the Bundle the script writes in its temporary directory", config)
			}
			c.drainedNode()
			c.stoppedUpstream()
		})
	}
}

// TestReadinessPolls checks step 5: /readyz is polled every 0.5 s until
// it answers 200, and only then does the script send the first request.
func TestReadinessPolls(t *testing.T) {
	t.Parallel()
	c := newCase(t, setup{sc: scenario{NotReadyPolls: 3}})
	r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+c.localRelease(stubServe, false))
	expect(t, r, 0, []string{`s: {"status":"ready","revision":"rev-0123456789ab"}`}, nil)
	polls, slept := 0, true
	for _, cl := range c.calls() {
		switch {
		case cl.Tool == "curl" && slices.Contains(cl.Args, proxyURL):
			t.Fatalf("the first request came before /readyz answered 200")
		case cl.Tool == "curl" && slices.Contains(cl.Args, readyzURL):
			if !slept {
				t.Errorf("a /readyz poll followed the previous one without sleep 0.5")
			}
			if cl.Answer == "200" {
				if polls < 3 {
					t.Errorf("/readyz answered 200 after %d polls, want at least 3 that answered 503", polls)
				}
				return
			}
			polls, slept = polls+1, false
		case cl.Tool == "sleep" && !slept:
			if !slices.Equal(cl.Args, []string{"0.5"}) {
				t.Errorf("the readiness poll interval is %q s, want 0.5", cl.Args)
			}
			slept = true
		}
	}
	t.Error("/readyz never answered 200")
}

// TestPreflight checks step 1: each missing tool, a python3 that does not
// run, an unsupported platform, busy ports and invalid inputs are all
// reported, and the script exits 2 before it starts or fetches anything.
func TestPreflight(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		s     setup
		env   func(c *qcase) []string
		wants []string
	}{
		{
			"missing curl and tar",
			setup{without: []string{"curl", "tar"}},
			nil,
			[]string{"quickstart: missing tool: curl", "quickstart: missing tool: tar"},
		},
		{
			"missing python3",
			setup{without: []string{"python3"}},
			nil,
			[]string{"quickstart: missing tool: python3"},
		},
		{
			"missing sha256sum and shasum",
			setup{without: []string{"sha256sum", "shasum"}},
			nil,
			[]string{"quickstart: missing tool: sha256sum or shasum"},
		},
		{
			"python3 that does not run",
			setup{sc: scenario{PythonBroken: true}},
			nil,
			[]string{"quickstart: python3 does not run (on macOS, install the Command Line Tools: xcode-select --install)"},
		},
		{
			"unsupported operating system",
			setup{sc: scenario{UnameS: "FreeBSD"}},
			nil,
			[]string{"quickstart: unsupported platform FreeBSD/x86_64: ruralzd archives exist for Linux and Darwin on amd64 and arm64"},
		},
		{
			"unsupported architecture",
			setup{sc: scenario{UnameM: "riscv64"}},
			nil,
			[]string{"quickstart: unsupported platform Linux/riscv64"},
		},
		{
			"busy ports",
			setup{sc: scenario{BusyPorts: []string{"8080", "9901"}}},
			nil,
			[]string{"quickstart: port 8080 is busy on 127.0.0.1", "quickstart: port 9901 is busy on 127.0.0.1"},
		},
		{
			"keep value",
			setup{},
			func(*qcase) []string { return []string{"RURALZ_QUICKSTART_KEEP=yes"} },
			[]string{"quickstart: RURALZ_QUICKSTART_KEEP=yes: want 1, or 0 or empty to stop after the first proxied request"},
		},
		{
			"version with its v",
			setup{},
			func(*qcase) []string { return []string{"RURALZ_QUICKSTART_VERSION=v0.1.0"} },
			[]string{"quickstart: RURALZ_QUICKSTART_VERSION=v0.1.0: give a release version without its v, such as 0.1.0 or 0.1.0-rc.1"},
		},
		{
			"partial version",
			setup{},
			func(*qcase) []string { return []string{"RURALZ_QUICKSTART_VERSION=0.1"} },
			[]string{"quickstart: RURALZ_QUICKSTART_VERSION=0.1: give a release version"},
		},
		{"archive that is no file", setup{}, func(c *qcase) []string {
			return []string{"RURALZ_QUICKSTART_ARCHIVE=" + filepath.Join(c.dir, "ruralzd_0.1.0_linux_amd64.tar.gz")}
		}, []string{"ruralzd_0.1.0_linux_amd64.tar.gz is not a file"}},
		{"archive with another name", setup{}, func(c *qcase) []string {
			p := filepath.Join(c.dir, "ruralzd.tar.gz")
			c.writeFile(p, tarGz(c.t, map[string]string{"ruralzd": "#!/bin/sh\n"}))
			return []string{"RURALZ_QUICKSTART_ARCHIVE=" + p}
		}, []string{"ruralzd.tar.gz is not named ruralzd_<version>_<os>_<arch>.tar.gz"}},
		{"archive of another platform", setup{}, func(c *qcase) []string {
			return []string{"RURALZ_QUICKSTART_ARCHIVE=" + c.release(filepath.Join(c.dir, "release"), "0.1.0", "darwin_arm64", stubServe, false)}
		}, []string{"ruralzd_0.1.0_darwin_arm64.tar.gz is for darwin/arm64, not this linux/amd64 host"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCase(t, tc.s)
			var env []string
			if tc.env != nil {
				env = tc.env(c)
			}
			r := c.run(c.script(), env...)
			expect(t, r, 2, nil, append(tc.wants, "quickstart: preflight failed"))
			if strings.Contains(r.stdout, "quickstart: preflight:") {
				t.Error("the preflight passed")
			}
			if got := c.callsOf("curl"); len(got) > 0 {
				t.Errorf("the failed preflight was followed by curl %q", got[0].Args)
			}
			c.startedNothing()
		})
	}
}

// TestPreflightPortCheck runs the preflight's port check with the host's
// python3 while the test holds 127.0.0.1:9000, or something else does.
func TestPreflightPortCheck(t *testing.T) {
	t.Parallel()
	c := newCase(t, setup{realPython: true})
	var lc net.ListenConfig
	if ln, err := lc.Listen(t.Context(), "tcp4", "127.0.0.1:9000"); err == nil {
		t.Cleanup(func() { _ = ln.Close() })
	}
	r := c.run(c.script())
	expect(t, r, 2, nil, []string{"quickstart: port 9000 is busy on 127.0.0.1", "quickstart: preflight failed"})
	c.startedNothing()
}

// TestNotBash checks that a POSIX shell running the script stops at once
// with exit 2.
func TestNotBash(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	out, err := exec.CommandContext(t.Context(), sh, "-c", `printf %s "${BASH_VERSION:-}"`).Output() //nolint:gosec // G204: the host's sh.
	if err != nil || len(out) > 0 {
		t.Skipf("sh is bash here (%q, %v)", out, err)
	}
	c := newCase(t, setup{})
	c.bash = sh
	r := c.run(c.script())
	expect(t, r, 2, nil, []string{"quickstart: run this script with bash"})
	if strings.Contains(r.stdout, "preflight") || strings.Contains(r.stderr, "preflight") {
		t.Error("the script went on under sh")
	}
}

// TestPlatforms checks that the host's uname picks its archive: Linux
// aarch64 linux/arm64, Darwin arm64 darwin/arm64 and Darwin x86_64
// darwin/amd64; shasum stands in for a missing sha256sum.
func TestPlatforms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		unameS, unameM, platform, want string
	}{
		{"Linux", "aarch64", "linux_arm64", "linux/arm64 with curl, tar, shasum and python3"},
		{"Darwin", "arm64", "darwin_arm64", "darwin/arm64 with curl, tar, shasum and python3"},
		{"Darwin", "x86_64", "darwin_amd64", "darwin/amd64 with curl, tar, shasum and python3"},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			t.Parallel()
			if _, err := exec.LookPath("shasum"); err != nil {
				t.Skip("shasum not installed")
			}
			c := newCase(t, setup{sc: scenario{UnameS: tc.unameS, UnameM: tc.unameM}, without: []string{"sha256sum"}})
			archive := c.release(filepath.Join(c.dir, "release"), "0.2.0-rc.1", tc.platform, stubServe, false)
			r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+archive)
			expect(t, r, 0, []string{"quickstart: preflight: " + tc.want, "quickstart: unpacked ruralzd 0.2.0-rc.1", "quickstart: first proxied request after "}, nil)
		})
	}
}

// TestKeepZero checks that RURALZ_QUICKSTART_KEEP=0 stops both processes
// after the first proxied request, as an empty value does.
func TestKeepZero(t *testing.T) {
	t.Parallel()
	c := newCase(t, setup{})
	r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+c.localRelease(stubServe, false), "RURALZ_QUICKSTART_KEEP=0")
	expect(t, r, 0, []string{"quickstart: stopped the Upstream and the Node"}, nil)
	if strings.Contains(r.stdout, "keep running") {
		t.Error("RURALZ_QUICKSTART_KEEP=0 kept the processes running")
	}
}

// TestChecksums checks step 2's checksum check: the archive must match its
// one SHA256SUMS line, in text or binary form and any case, or the script
// exits 1 before it starts anything.
func TestChecksums(t *testing.T) {
	t.Parallel()
	name := "ruralzd_0.1.0_linux_amd64.tar.gz"
	for _, tc := range []struct {
		desc string
		// edit changes the release directory written for the case.
		edit  func(c *qcase, dir, archive string)
		code  int
		wants []string
	}{
		{"mismatch", func(c *qcase, dir, _ string) {
			c.writeFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Repeat("ab", 32)+"  "+name+"\n"))
		}, 1, []string{"quickstart: checksum mismatch for " + name + ": SHA256SUMS has " + strings.Repeat("ab", 32) + ", the archive has "}},
		{"not listed", func(c *qcase, dir, _ string) {
			c.writeFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Repeat("ab", 32)+"  ruralz_0.1.0_linux_amd64.tar.gz\n"))
		}, 1, []string{"quickstart: SHA256SUMS lists " + name + " 0 times, want once"}},
		{"listed twice", func(c *qcase, dir, archive string) {
			line := sha256Hex([]byte(readFile(c.t, archive))) + "  " + name + "\n"
			c.writeFile(filepath.Join(dir, "SHA256SUMS"), []byte(line+line))
		}, 1, []string{"quickstart: SHA256SUMS lists " + name + " 2 times, want once"}},
		{"no SHA256SUMS", func(c *qcase, dir, _ string) {
			if err := os.Remove(filepath.Join(dir, "SHA256SUMS")); err != nil {
				c.t.Fatal(err)
			}
		}, 1, []string{"quickstart: no SHA256SUMS beside "}},
		{"binary mode and upper case", func(c *qcase, dir, archive string) {
			c.writeFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.ToUpper(sha256Hex([]byte(readFile(c.t, archive))))+" *"+name+"\n"))
		}, 0, nil},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			c := newCase(t, setup{})
			dir := filepath.Join(c.dir, "release")
			archive := c.localRelease(stubServe, false)
			tc.edit(c, dir, archive)
			r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+archive)
			expect(t, r, tc.code, nil, tc.wants)
			if tc.code != 0 {
				c.startedNothing()
			}
		})
	}
}

// TestArchiveWithoutRuralzd checks that an archive matching its checksum
// but holding no ruralzd exits 1.
func TestArchiveWithoutRuralzd(t *testing.T) {
	t.Parallel()
	c := newCase(t, setup{})
	r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+c.localRelease("", false))
	expect(t, r, 1, nil, []string{"quickstart: cannot unpack ruralzd from "})
	c.startedNothing()
}

// verifyArgs returns the cosign verify-blob arguments for SHA256SUMS in
// dir, signed for 0.1.0, with --new-bundle-format for cosign 2.x.
func verifyArgs(dir string, two bool) []string {
	args := []string{"verify-blob"}
	if two {
		args = append(args, "--new-bundle-format")
	}
	return append(args, "--bundle", filepath.Join(dir, "SHA256SUMS.sigstore.json"),
		"--certificate-identity", signer+"0.1.0", "--certificate-oidc-issuer", issuer, filepath.Join(dir, "SHA256SUMS"))
}

// TestSignature checks step 2's signature check on a local archive with
// SHA256SUMS.sigstore.json beside it: cosign 3.1.3 or newer on 3.x, or
// 2.6.5 or newer on 2.x, verifies SHA256SUMS against release-build.yml at
// the archive's version first; any other cosign, no cosign or no bundle
// leaves only the checksum check, which the script says.
func TestSignature(t *testing.T) {
	t.Parallel()
	only := "only checksums are checked"
	for _, tc := range []struct {
		name, version string
		admit         bool
	}{
		{"3.1.3", "v3.1.3", true},
		{"3.2.0", "v3.2.0", true},
		{"3.10.1", "v3.10.1", true},
		{"3.1.3 without v", "3.1.3", true},
		{"2.6.5", "v2.6.5", true},
		{"2.7.0", "v2.7.0", true},
		{"3.1.2", "v3.1.2", false},
		{"3.0.9", "v3.0.9", false},
		{"2.6.4", "v2.6.4", false},
		{"2.5.12", "v2.5.12", false},
		{"1.13.1", "v1.13.1", false},
		{"4.0.0", "v4.0.0", false},
		{"3.1.3-rc.1", "v3.1.3-rc.1", false},
		{"devel", "devel", false},
		{"without a version", "", false},
	} {
		t.Run("cosign "+tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCase(t, setup{sc: scenario{CosignVersion: tc.version}, cosign: true})
			archive := c.localRelease(stubServe, true)
			r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+archive)
			got := c.callsOf("cosign")
			if !tc.admit {
				expect(t, r, 0, []string{"is not 3.1.3 or newer on 3.x or 2.6.5 or newer on 2.x (GHSA-fx35-mq7g-6g98): " + only}, nil)
				if len(got) > 0 {
					t.Errorf("cosign %s verified: %q", tc.version, got[0].Args)
				}
				return
			}
			expect(t, r, 0, []string{"quickstart: SHA256SUMS: signed by " + signer + "0.1.0 (cosign " + tc.version + ")", "quickstart: SHA256SUMS: ruralzd_0.1.0_linux_amd64.tar.gz matches"}, nil)
			want := verifyArgs(filepath.Dir(archive), strings.HasPrefix(strings.TrimPrefix(tc.version, "v"), "2."))
			if len(got) != 1 || !slices.Equal(got[0].Args, want) {
				t.Errorf("cosign calls = %v, want one verify-blob %q", got, want)
			}
		})
	}
	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		c := newCase(t, setup{sc: scenario{CosignVersion: "v3.1.3", CosignFail: true}, cosign: true})
		r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+c.localRelease(stubServe, true))
		expect(t, r, 1, nil, []string{"Error: none of the expected identities matched", "quickstart: SHA256SUMS: the signature check against " + signer + "0.1.0 failed (cosign v3.1.3, exit 1)"})
		c.startedNothing()
	})
	t.Run("no cosign", func(t *testing.T) {
		t.Parallel()
		c := newCase(t, setup{})
		r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+c.localRelease(stubServe, true))
		expect(t, r, 0, []string{"quickstart: no cosign on PATH: " + only}, nil)
	})
	t.Run("no bundle", func(t *testing.T) {
		t.Parallel()
		c := newCase(t, setup{sc: scenario{CosignVersion: "v3.1.3"}, cosign: true})
		r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+c.localRelease(stubServe, false))
		expect(t, r, 0, []string{"quickstart: no SHA256SUMS.sigstore.json: " + only}, nil)
		if got := c.callsOf("cosign"); len(got) > 0 {
			t.Errorf("cosign verified without a bundle: %q", got[0].Args)
		}
	})
}

// TestDownload checks step 2's download path: the host's archive,
// SHA256SUMS and SHA256SUMS.sigstore.json of the release
// RURALZ_QUICKSTART_VERSION names, or of the latest release, fetched over
// HTTPS only and verified; no release, a latest tag that is no version
// and a missing asset each exit 1.
func TestDownload(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sc   scenario
		// served are the release tags the stand-in curl serves, signed
		// unless unsigned is set.
		served   []string
		unsigned bool
		env      []string
		code     int
		version  string
		stdout   []string
		stderr   []string
	}{
		{
			name: "release version", served: []string{"0.1.0"}, env: []string{"RURALZ_QUICKSTART_VERSION=0.1.0"}, version: "0.1.0",
			stdout: []string{"quickstart: downloading ruralzd 0.1.0 for linux/amd64 from " + releases + "/download/v0.1.0"},
		},
		{name: "release candidate", served: []string{"0.1.0-rc.1"}, env: []string{"RURALZ_QUICKSTART_VERSION=0.1.0-rc.1"}, version: "0.1.0-rc.1"},
		{
			name: "latest release", sc: scenario{Latest: "v0.2.0"}, served: []string{"0.1.0", "0.2.0"}, version: "0.2.0",
			stdout: []string{"quickstart: the latest release is 0.2.0"},
		},
		{
			name: "no release", code: 1,
			stderr: []string{"quickstart: ravindu-rev/ruralz has no release yet: set RURALZ_QUICKSTART_ARCHIVE or RURALZ_QUICKSTART_VERSION"},
		},
		{
			name: "latest tag that is no version", sc: scenario{Latest: "v1"}, code: 1,
			stderr: []string{"quickstart: the latest release tag of ravindu-rev/ruralz, v1, is not a release version"},
		},
		{
			name: "missing archive", env: []string{"RURALZ_QUICKSTART_VERSION=0.3.0"}, code: 1,
			stderr: []string{"quickstart: download of " + releases + "/download/v0.3.0/ruralzd_0.3.0_linux_amd64.tar.gz failed (curl exit 22)"},
		},
		{
			name: "missing signature", served: []string{"0.1.0"}, unsigned: true, env: []string{"RURALZ_QUICKSTART_VERSION=0.1.0"}, code: 1,
			stderr: []string{"quickstart: download of " + releases + "/download/v0.1.0/SHA256SUMS.sigstore.json failed (curl exit 22)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.sc.CosignVersion = "v3.1.3"
			c := newCase(t, setup{sc: tc.sc, cosign: true})
			for _, v := range tc.served {
				c.release(filepath.Join(c.state, "release", "v"+v), v, "linux_amd64", stubServe, !tc.unsigned)
			}
			r := c.run(c.script(), tc.env...)
			expect(t, r, tc.code, tc.stdout, tc.stderr)
			if tc.code != 0 {
				c.startedNothing()
				return
			}
			var fetched []string
			for _, cl := range c.callsOf("curl") {
				if u := cl.Args[len(cl.Args)-1]; strings.HasPrefix(u, "https://") {
					fetched = append(fetched, u)
					if !containsSeq(cl.Args, "--proto", "=https") || !slices.Contains(cl.Args, "--tlsv1.2") {
						t.Errorf("curl %q fetched without --proto =https --tlsv1.2", cl.Args)
					}
				}
			}
			base := releases + "/download/v" + tc.version + "/"
			want := []string{base + "ruralzd_" + tc.version + "_linux_amd64.tar.gz", base + "SHA256SUMS", base + "SHA256SUMS.sigstore.json"}
			if tc.sc.Latest != "" {
				want = append([]string{releases + "/latest"}, want...)
			}
			if !slices.Equal(fetched, want) {
				t.Errorf("fetched %q, want %q", fetched, want)
			}
			got := c.callsOf("cosign")
			if len(got) != 1 || !slices.Contains(got[0].Args, signer+tc.version) {
				t.Errorf("cosign calls = %v, want verify-blob against %s%s", got, signer, tc.version)
			}
			if !strings.Contains(r.stdout, "quickstart: first proxied request after ") {
				t.Error("no first proxied request")
			}
		})
	}
}

// TestFailure checks step 7: a Node or an Upstream that exits early, or
// 600 s without a first proxied request, prints the reason, the last
// /readyz answer and the ruralzd log (and the Upstream's log when it
// failed), stops what still runs and exits 1.
func TestFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		sc     scenario
		mode   string
		stdout []string
		stderr []string
	}{
		{
			name: "Node exits early", mode: stubExit,
			stdout: []string{"quickstart: stopped the Upstream"},
			stderr: []string{
				"quickstart: the Node (ruralzd) exited early with status 3",
				"quickstart: last /readyz answer: none, nothing answered on 127.0.0.1:9901",
				"quickstart: ruralzd log:", `"msg":"listener http: bind 127.0.0.1:8080: address already in use"`, "quickstart: end of the ruralzd log",
			},
		},
		{
			name: "Upstream exits early", sc: scenario{UpstreamExit: true}, mode: stubServe,
			stdout: []string{"quickstart: stopped the Node"},
			stderr: []string{
				"quickstart: the Upstream (python3 -m http.server) exited early with status 1",
				"quickstart: ruralzd log:", "quickstart: Upstream log:", "OSError: [Errno 98] Address already in use",
			},
		},
		{
			name: "never ready", sc: scenario{ClockScale: 100}, mode: stubHang,
			stdout: []string{"quickstart: stopped the Upstream and the Node"},
			stderr: []string{
				"quickstart: no first proxied request within 600 s",
				`quickstart: last /readyz answer: 503 {"status":"not_ready","reasons":[{"reason":"no_revision"}]}`,
				`"msg":"stub ruralzd starting"`,
			},
		},
		{
			name: "answer without x-quickstart", sc: scenario{ClockScale: 100, NoHeader: true}, mode: stubServe,
			stderr: []string{"quickstart: no first proxied request within 600 s", `quickstart: last /readyz answer: 200 {"status":"ready","revision":"rev-0123456789ab"}`},
		},
		{
			name: "answer with another body", sc: scenario{ClockScale: 100, Body: "Hello from somewhere else"}, mode: stubServe,
			stderr: []string{"quickstart: no first proxied request within 600 s"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCase(t, setup{sc: tc.sc})
			r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+c.localRelease(tc.mode, false))
			expect(t, r, 1, tc.stdout, tc.stderr)
			if proxiedLine().MatchString(r.stdout) {
				t.Error("the run reported a first proxied request")
			}
		})
	}
}

// TestStubbornNode checks that a Node that ignores SIGTERM is killed once
// it outlives the 30 s exit bound.
func TestStubbornNode(t *testing.T) {
	t.Parallel()
	c := newCase(t, setup{sc: scenario{ClockScale: 10}})
	r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+c.localRelease(stubStubborn, false))
	expect(t, r, 0, []string{"quickstart: stopped the Upstream and the Node"}, []string{"quickstart: ruralzd outlived the 30 s exit bound: SIGKILL"})
	if n := strings.Count(c.stateFile("ruralzd.ignored"), "ignored"); n < 2 {
		t.Errorf("the stub got %d SIGTERMs before SIGKILL, want 2", n)
	}
}

// TestKeep checks RURALZ_QUICKSTART_KEEP=1: after the first proxied
// request both processes keep running, the script prints the curl command
// and how to stop them, and SIGINT or SIGTERM stops both and exits 0; a
// Node that exits meanwhile ends the run as step 7 does.
func TestKeep(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		// killNode kills the Node instead of signaling the script.
		killNode bool
	}{
		{name: "SIGINT", sig: syscall.SIGINT},
		{name: "SIGTERM", sig: syscall.SIGTERM},
		{name: "Node exits", killNode: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCase(t, setup{})
			run := c.start(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+c.localRelease(stubServe, false), "RURALZ_QUICKSTART_KEEP=1")
			run.await("quickstart: Stop both with Ctrl-C, or with: kill -TERM ")
			for _, name := range []string{"ruralzd", "upstream"} {
				if pid, ok := c.pid(name); !ok || syscall.Kill(pid, 0) != nil || c.exists(name+".exited") {
					t.Errorf("the %s does not keep running", name)
				}
			}
			if tc.killNode {
				pid, _ := c.pid("ruralzd")
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				r := run.wait()
				expect(t, r, 1, []string{"quickstart: stopped the Upstream"}, []string{"quickstart: the Node (ruralzd) exited early with status 137", "quickstart: ruralzd log:"})
				return
			}
			run.signal(tc.sig)
			r := run.wait()
			expect(t, r, 0, []string{
				"quickstart: first proxied request after ",
				"quickstart: RURALZ_QUICKSTART_KEEP=1: the Node and the Upstream keep running. Try:",
				"quickstart:   curl http://127.0.0.1:8080/",
				"quickstart: Stop both with Ctrl-C, or with: kill -TERM " + strconv.Itoa(run.cmd.Process.Pid),
				"quickstart: stopped the Upstream and the Node",
			}, nil)
			c.drainedNode()
			c.stoppedUpstream()
		})
	}
}

// TestInterrupted checks that SIGHUP, SIGINT or SIGTERM before the first
// proxied request stops both processes, removes the temporary directory
// and exits 128 plus the signal number.
func TestInterrupted(t *testing.T) {
	t.Parallel()
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()
			c := newCase(t, setup{})
			run := c.start(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+c.localRelease(stubHang, false))
			run.awaitState("upstream.pid", "ruralzd.trapped")
			run.signal(sig)
			r := run.wait()
			expect(t, r, 128+int(sig), []string{"quickstart: stopped the Upstream and the Node"}, []string{"quickstart: interrupted"})
			c.drainedNode()
			c.stoppedUpstream()
		})
	}
}

// TestNoNetworkOutsideDownloads checks that a run from a local archive
// fetches nothing: every curl call is a loopback probe.
func TestNoNetworkOutsideDownloads(t *testing.T) {
	t.Parallel()
	c := newCase(t, setup{sc: scenario{CosignVersion: "v3.1.3"}, cosign: true})
	r := c.run(c.script(), "RURALZ_QUICKSTART_ARCHIVE="+c.localRelease(stubServe, true))
	expect(t, r, 0, nil, nil)
	for _, cl := range c.callsOf("curl") {
		if u := cl.Args[len(cl.Args)-1]; u != readyzURL && u != proxyURL {
			t.Errorf("a local-archive run fetched %s", u)
		}
	}
	if got := len(c.callsOf("cosign")); got != 1 {
		t.Errorf("cosign verified %d times, want 1", got)
	}
}

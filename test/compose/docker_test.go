// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build compose

package compose

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Inputs of the suite (see the package documentation).
const (
	envImage         = "RURALZ_TEST_IMAGE"
	envPreviousImage = "RURALZ_TEST_PREVIOUS_IMAGE"
)

// Deadlines, in real time: the suite tests a shipped image and waits on
// conditions under them, never on fixed sleeps.
const (
	// setupTimeout bounds the builds and pulls of TestMain.
	setupTimeout = 20 * time.Minute
	// commandTimeout bounds one docker or ruralz command.
	commandTimeout = 5 * time.Minute
	// upWait is docker compose up --wait-timeout, in seconds.
	upWait = 180
	// readyTimeout bounds the wait for a Node's /readyz, a started
	// container's boot included.
	readyTimeout = 60 * time.Second
	// requestTimeout bounds one HTTP exchange with a Node or a mock.
	requestTimeout = 10 * time.Second
	// signalTimeout bounds the wait for the Collector's files, above two
	// 15 s metric pushes (target).
	signalTimeout = 90 * time.Second
	// exitTimeout bounds the wait for a container to exit after a stop
	// or a handover: the 40 s stop grace period and a margin.
	exitTimeout = 90 * time.Second
	// downTimeout is docker compose down --timeout, in seconds: above the
	// stop grace period, so a draining Node exits by itself.
	downTimeout = 45
	// pollInterval spaces condition polls; it never decides an outcome.
	pollInterval = 250 * time.Millisecond
	// outputTail bounds the command output an error carries.
	outputTail = 8 << 10
)

// theSuite is what TestMain prepared; nil when RURALZ_TEST_IMAGE is unset,
// and every tagged test but TestComposeConfig skips.
var theSuite *suite

// suite is the images, the CLI and the Docker client every test shares.
type suite struct {
	root   string
	docker string
	// platform is the daemon's, such as linux/amd64.
	platform string
	// image is the image under test, oldImage the one the upgrade starts
	// from.
	image, oldImage string
	// mockImage and collectorImage are the suite's builds, removed at the
	// end.
	mockImage, collectorImage string
	// ruralz is the CLI the suite runs on the runner.
	ruralz string
}

// TestMain prepares the suite when RURALZ_TEST_IMAGE names an image: a
// suite asked for fails at once when Docker, the compose plugin or an
// image is missing.
func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) int {
	image := os.Getenv(envImage)
	if image == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	s, cleanup, err := newSuite(ctx, image, os.Getenv(envPreviousImage))
	cancel()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "compose: %v\n", err)
		return 1
	}
	defer cleanup()
	theSuite = s
	return m.Run()
}

// requireSuite returns the suite, or skips the test without one.
func requireSuite(t *testing.T) *suite {
	t.Helper()
	if theSuite == nil {
		t.Skipf("%s is not set: run make image-checks, or name an image with %s", envImage, envImage)
	}
	return theSuite
}

// newSuite checks Docker, builds the CLI (or takes it from
// RURALZ_TEST_BIN_DIR), builds the mock and Collector images and makes
// sure every image the projects run is present.
func newSuite(ctx context.Context, image, previous string) (_ *suite, cleanup func(), err error) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return nil, nil, fmt.Errorf("the docker CLI: %w", err)
	}
	s := &suite{docker: docker, image: image, oldImage: previous}
	if s.oldImage == "" {
		s.oldImage = image
	}
	out, err := s.dockerCmd(ctx, nil, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
	if err != nil {
		return nil, nil, fmt.Errorf("no Docker daemon: %w", err)
	}
	s.platform = strings.TrimSpace(string(out))
	if _, err := s.dockerCmd(ctx, nil, "compose", "version"); err != nil {
		return nil, nil, fmt.Errorf("no docker compose plugin: %w", err)
	}
	if s.root, err = binaries.RepoRoot("."); err != nil {
		return nil, nil, err
	}
	work, err := os.MkdirTemp("", "ruralz-compose-")
	if err != nil {
		return nil, nil, err
	}
	var images []string
	cleanup = func() {
		// The cleanup runs after the suite, past the end of ctx.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandTimeout)
		defer cancel()
		if len(images) > 0 {
			_, _ = s.dockerCmd(ctx, nil, append([]string{"image", "rm", "--force"}, images...)...)
		}
		_ = os.RemoveAll(work)
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	bins, err := binaries.Build(ctx, s.root, filepath.Join(work, "bin"), binaries.BuildOptions{Commands: []string{binaries.Ruralz}})
	if err != nil {
		return nil, nil, err
	}
	s.ruralz = bins.Ruralz
	suffix := randomHex(6)
	s.mockImage = "ruralz-compose-mock:" + suffix
	if err := s.buildMockImage(ctx, filepath.Join(work, "mock")); err != nil {
		return nil, nil, err
	}
	images = append(images, s.mockImage)
	s.collectorImage = "ruralz-compose-collector:" + suffix
	if err := s.buildImage(ctx, suitePath(s.root, "collector.Dockerfile"), s.collectorImage, filepath.Join(s.root, filepath.FromSlash(suiteDir))); err != nil {
		return nil, nil, err
	}
	images = append(images, s.collectorImage)
	for _, img := range []string{s.image, s.oldImage, redisserver.ImageValkey9} {
		if err := s.ensureImage(ctx, img); err != nil {
			return nil, nil, err
		}
	}
	for _, img := range []string{s.image, s.oldImage} {
		out, err := s.dockerCmd(ctx, nil, "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", img)
		if err != nil {
			return nil, nil, err
		}
		if got := strings.TrimSpace(string(out)); got != s.platform {
			return nil, nil, fmt.Errorf("image %s is %s, the daemon runs %s", img, got, s.platform)
		}
	}
	return s, cleanup, nil
}

// buildMockImage builds test/compose/mockimage for the daemon's platform
// with CGO_ENABLED=0 into dir and the mock image from it.
func (s *suite) buildMockImage(ctx context.Context, dir string) error {
	goos, goarch, ok := strings.Cut(s.platform, "/")
	if !ok || goos != "linux" {
		return fmt.Errorf("the daemon's platform %q is not linux/<arch>", s.platform)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	goCmd, err := goCommand()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, goCmd, "build", "-trimpath", "-o", filepath.Join(dir, "mock"), "./test/compose/mockimage") //nolint:gosec // G204: the go command with fixed arguments
	cmd.Dir = s.root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building the mock: %w\n%s", err, out)
	}
	return s.buildImage(ctx, suitePath(s.root, "mock.Dockerfile"), s.mockImage, dir)
}

// buildImage builds tag from dockerfile and the context dir for the
// daemon's platform and loads it into the daemon's images: --load, because
// a docker-container builder, the one docker/setup-buildx-action selects,
// keeps a result without it in its build cache only.
func (s *suite) buildImage(ctx context.Context, dockerfile, tag, dir string) error {
	_, err := s.dockerCmd(ctx, nil, "buildx", "build", "--load", "--progress", "plain", "--platform", s.platform, "--file", dockerfile, "--tag", tag, dir)
	return err
}

// goCommand returns $GOROOT/bin/go when GOROOT is set, else go from PATH.
func goCommand() (string, error) {
	if gr := os.Getenv("GOROOT"); gr != "" {
		p := filepath.Join(gr, "bin", "go")
		if _, err := os.Stat(p); err == nil { //nolint:gosec // G703: the go command under the caller's GOROOT
			return p, nil
		}
	}
	return exec.LookPath("go")
}

// ensureImage pulls img unless the daemon holds it.
func (s *suite) ensureImage(ctx context.Context, img string) error {
	if _, err := s.dockerCmd(ctx, nil, "image", "inspect", "--format", "{{.Id}}", img); err == nil {
		return nil
	}
	_, err := s.dockerCmd(ctx, nil, "pull", "--quiet", img)
	return err
}

// dockerCmd runs docker with args, env added to the environment, and
// returns its standard output; an error carries the tail of its standard
// error.
func (s *suite) dockerCmd(ctx context.Context, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.docker, args...) //nolint:gosec // G204: the docker CLI with the suite's arguments
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("docker %s: %w\n%s", strings.Join(args, " "), err, tail(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// tail returns the end of b, at most outputTail bytes.
func tail(b []byte) []byte {
	if len(b) > outputTail {
		return b[len(b)-outputTail:]
	}
	return b
}

// randomHex returns n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// project is one Compose project of one test: its name, file, directory
// and variables. Its cleanup writes the failure evidence and runs
// docker compose down.
type project struct {
	s    *suite
	t    *testing.T
	name string
	file string
	run  string
	env  []string
}

// newProject prepares a project of file for t, its directory the
// test's (RURALZ_COMPOSE_RUN) with vars added to the suite's.
func (s *suite) newProject(t *testing.T, scenario, file string, vars map[string]string) *project {
	t.Helper()
	run, err := os.MkdirTemp("", "ruralz-compose-"+scenario+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(run) })
	p := &project{
		s: s, t: t, name: "rzc-" + scenario + "-" + randomHex(4), file: suitePath(s.root, file), run: run,
		env: []string{varImage + "=" + s.image, varMockImage + "=" + s.mockImage, varRun + "=" + run},
	}
	for k, v := range vars {
		p.env = append(p.env, k+"="+v)
	}
	t.Cleanup(p.teardown)
	return p
}

// compose runs docker compose on the project.
func (p *project) compose(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"compose", "--ansi", "never", "--progress", "quiet", "--project-name", p.name, "--file", p.file, "--project-directory", p.run}, args...)
	return p.s.dockerCmd(ctx, p.env, full...)
}

// up starts services and waits until each runs.
func (p *project) up(ctx context.Context, args ...string) {
	p.t.Helper()
	full := append([]string{"up", "--detach", "--wait", "--wait-timeout", fmt.Sprint(upWait)}, args...)
	if _, err := p.compose(ctx, full...); err != nil {
		p.t.Fatalf("project %s: %v", p.name, err)
	}
}

// port returns the loopback address the runner reaches a service's
// container port on.
func (p *project) port(ctx context.Context, service string, port int) string {
	p.t.Helper()
	out, err := p.compose(ctx, "port", service, fmt.Sprint(port))
	if err != nil {
		p.t.Fatalf("project %s: %v", p.name, err)
	}
	_, hostPort, err := net.SplitHostPort(strings.TrimSpace(string(out)))
	if err != nil {
		p.t.Fatalf("project %s: port %s %d answered %q: %v", p.name, service, port, out, err)
	}
	return net.JoinHostPort("127.0.0.1", hostPort)
}

// containerState is what the suite reads of docker inspect.
type containerState struct {
	State struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		ExitCode  int    `json:"ExitCode"`
		OOMKilled bool   `json:"OOMKilled"`
	} `json:"State"`
	Config struct {
		User string `json:"User"`
	} `json:"Config"`
	HostConfig struct {
		ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
		CapDrop        []string `json:"CapDrop"`
		CapAdd         []string `json:"CapAdd"`
		SecurityOpt    []string `json:"SecurityOpt"`
		Privileged     bool     `json:"Privileged"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

// inspect returns the state of a service's container, stopped ones
// included.
func (p *project) inspect(ctx context.Context, service string) containerState {
	p.t.Helper()
	out, err := p.compose(ctx, "ps", "--all", "--quiet", service)
	if err != nil {
		p.t.Fatalf("project %s: %v", p.name, err)
	}
	ids := strings.Fields(string(out))
	if len(ids) != 1 {
		p.t.Fatalf("project %s: service %s has containers %v, want one", p.name, service, ids)
	}
	out, err = p.s.dockerCmd(ctx, nil, "inspect", "--format", "{{json .}}", ids[0])
	if err != nil {
		p.t.Fatalf("project %s: %v", p.name, err)
	}
	var st containerState
	if err := json.Unmarshal(out, &st); err != nil {
		p.t.Fatalf("project %s: inspect %s: %v", p.name, service, err)
	}
	return st
}

// logs returns a service's container logs.
func (p *project) logs(ctx context.Context, service string) string {
	p.t.Helper()
	out, err := p.compose(ctx, "logs", "--no-color", service)
	if err != nil {
		p.t.Fatalf("project %s: %v", p.name, err)
	}
	return string(out)
}

// checkHardening checks that a running ruralzd container got the
// hardening its service asks for, and nothing more.
func (p *project) checkHardening(ctx context.Context, service string) {
	p.t.Helper()
	st := p.inspect(ctx, service)
	h := st.HostConfig
	if !h.ReadonlyRootfs || len(h.CapDrop) != 1 || h.CapDrop[0] != "ALL" || len(h.CapAdd) != 0 || h.Privileged || !containsPrefix(h.SecurityOpt, "no-new-privileges") {
		p.t.Errorf("%s: host config %+v, want a read-only root, cap_drop [ALL], no added capability and no-new-privileges", service, h)
	}
	if st.Config.User != nodeUser {
		p.t.Errorf("%s: user %q, want %q", service, st.Config.User, nodeUser)
	}
	data := false
	for _, m := range st.Mounts {
		if m.Destination == nodeDataDir {
			data = m.Type == "volume" && m.RW
		}
	}
	if !data {
		p.t.Errorf("%s: mounts %+v, want a writable named volume at %s", service, st.Mounts, nodeDataDir)
	}
}

func containsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// stop stops a service's container with its stop grace period and checks
// that the Node drained and exited 0 before docker would have killed it.
func (p *project) stop(ctx context.Context, service string) {
	p.t.Helper()
	start := time.Now()
	if _, err := p.compose(ctx, "stop", service); err != nil {
		p.t.Fatalf("project %s: %v", p.name, err)
	}
	st := p.inspect(ctx, service)
	if st.State.Running || st.State.ExitCode != 0 || st.State.OOMKilled {
		p.t.Errorf("%s after docker compose stop (%v): state %+v, want exited with code 0 inside the %s grace period", service, time.Since(start).Round(time.Second), st.State, stopGracePeriod)
	}
}

// waitExit waits until a service's container exits and returns its state.
func (p *project) waitExit(ctx context.Context, service string, within time.Duration) containerState {
	p.t.Helper()
	deadline := time.Now().Add(within)
	for {
		st := p.inspect(ctx, service)
		if !st.State.Running && st.State.Status == "exited" {
			return st
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("%s still %s after %v", service, st.State.Status, within)
		}
		sleepCtx(ctx)
	}
}

// sleepCtx waits pollInterval or until ctx ends: the pause between two
// condition polls.
func sleepCtx(ctx context.Context) {
	t := time.NewTimer(pollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// teardown writes the evidence of a failed test, then removes the
// project's containers, networks and volumes.
func (p *project) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if p.t.Failed() {
		p.evidence(ctx)
	}
	if _, err := p.compose(ctx, "down", "--volumes", "--remove-orphans", "--timeout", fmt.Sprint(downTimeout)); err != nil {
		p.t.Logf("project %s: %v", p.name, err)
	}
}

// evidence writes the project's container list, every service's logs and
// the Collector's files under RURALZ_TEST_ARTIFACT_DIR/<test>/<project>.
func (p *project) evidence(ctx context.Context) {
	base := os.Getenv(topology.ArtifactDirEnv)
	if base == "" {
		return
	}
	dir := filepath.Join(base, safeName(p.t.Name()), p.name)
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // G703: under the artifact directory the test run names
		p.t.Logf("evidence: %v", err)
		return
	}
	write := func(name string, data []byte, err error) {
		if err != nil {
			data = append(data, []byte("\n"+err.Error())...)
		}
		if werr := os.WriteFile(filepath.Join(dir, name), data, 0o600); werr != nil { //nolint:gosec // G703: a fixed name under the artifact directory the test run names
			p.t.Logf("evidence: %v", werr)
		}
	}
	out, err := p.compose(ctx, "ps", "--all")
	write("ps.txt", out, err)
	out, err = p.compose(ctx, "logs", "--no-color", "--timestamps")
	write("logs.txt", out, err)
	out, err = p.compose(ctx, "config")
	write("compose.yaml", out, err)
	col := filepath.Join(p.run, "collector", "out")
	entries, _ := os.ReadDir(col)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(col, e.Name())) //nolint:gosec // G304: a file the test's Collector wrote
		write("collector-"+e.Name(), data, err)
	}
	p.t.Logf("evidence of project %s in %s", p.name, dir)
}

// safeName turns a test name into a directory name.
func safeName(name string) string {
	return regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(name, "_")
}

// runCLIBinary runs the suite's ruralz CLI and returns its exit code and
// outputs; a failure to start it fails the test.
func (s *suite) runCLIBinary(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.ruralz, args...) //nolint:gosec // G204: the CLI under test with the suite's arguments
	cmd.Env = cliEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, stdout.String(), stderr.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), stdout.String(), stderr.String()
	default:
		t.Fatalf("ruralz %s: %v", strings.Join(args, " "), err)
		return 0, "", ""
	}
}

// cliEnv is the CLI's environment: the runner's, without the RURALZ_*
// variables, so only the flags the suite passes choose an admin URL,
// token or Environment.
func cliEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "RURALZ_") {
			out = append(out, kv)
		}
	}
	return out
}

// writeShared writes data to path with mode 0644, creating its directory
// with mode 0755: the containers read it as user 65532, not the runner.
func writeShared(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: a container user other than the runner traverses it
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // G306: a container user other than the runner reads it
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: as above, whatever the umask
		t.Fatal(err)
	}
}

// mkShared creates dir with mode 0755 for a container to read or write.
func mkShared(t testing.TB, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: a container user other than the runner traverses it
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: as above, whatever the umask
		t.Fatal(err)
	}
}

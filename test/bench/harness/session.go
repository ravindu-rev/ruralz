// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// Environment variables of the harness: RURALZ_BENCH_RECORDS,
// RURALZ_BENCH_OHA and RURALZ_BENCH_VEGETA come from make bench-latency;
// the others from nightly.yml's inputs and the bench-records branch.
const (
	EnvRecords     = "RURALZ_BENCH_RECORDS"
	EnvOha         = "RURALZ_BENCH_OHA"
	EnvVegeta      = "RURALZ_BENCH_VEGETA"
	EnvProfile     = "RURALZ_BENCH_PROFILE"
	EnvState       = "RURALZ_BENCH_STATE"
	EnvRotationAll = "RURALZ_BENCH_ROTATION_ALL"
	EnvAccept      = "RURALZ_BENCH_ACCEPT"
	EnvBaselineBin = "RURALZ_BENCH_BASELINE_RURALZD"
	EnvBaselineRef = "RURALZ_BENCH_BASELINE_VERSION"
)

// Settings are the harness's inputs.
type Settings struct {
	// Records is the directory the job's records go to.
	Records string
	// Oha and Vegeta are the pinned load tools on the harness's host.
	Oha, Vegeta string
	// Profile is the profile inventory; empty runs the loopback profile.
	Profile string
	// State is the rotation state file read from the bench-records
	// branch; empty starts a fresh rotation.
	State string
	// RotationAll runs every slot without the 3-hour limit (release).
	RotationAll bool
	// Accept holds a maintainer's acceptance: scenario and commit.
	Accept *Acceptance
	// BaselineRuralzd and BaselineVersion name the previous release's
	// verified linux/amd64 ruralzd, the baseline of a release run; empty
	// judges the release on absolute budgets only (the first release).
	BaselineRuralzd, BaselineVersion string
}

// Acceptance is a maintainer's acceptance of a deliberate regression: the
// scenario's next baseline is Commit.
type Acceptance struct {
	Scenario, Commit string
}

// commitPattern matches a full or abbreviated commit name.
func commitPattern() *regexp.Regexp { return regexp.MustCompile(`^[0-9a-f]{7,64}$`) }

// ParseAcceptance parses "<scenario>=<commit>".
func ParseAcceptance(s string) (*Acceptance, error) {
	if s == "" {
		return nil, nil
	}
	sc, c, ok := strings.Cut(s, "=")
	if !ok || !fileName().MatchString(sc) || !commitPattern().MatchString(c) {
		return nil, fmt.Errorf("harness: %s %q is not <scenario>=<commit>", EnvAccept, s)
	}
	return &Acceptance{Scenario: sc, Commit: c}, nil
}

// SettingsFromEnv reads the settings from getenv.
func SettingsFromEnv(getenv func(string) string) (Settings, error) {
	s := Settings{
		Records: getenv(EnvRecords), Oha: getenv(EnvOha), Vegeta: getenv(EnvVegeta),
		Profile: getenv(EnvProfile), State: getenv(EnvState),
		BaselineRuralzd: getenv(EnvBaselineBin), BaselineVersion: getenv(EnvBaselineRef),
	}
	var errs []error
	switch v := getenv(EnvRotationAll); v {
	case "", "false":
	case "true":
		s.RotationAll = true
	default:
		errs = append(errs, fmt.Errorf("harness: %s %q is not true or false", EnvRotationAll, v))
	}
	a, err := ParseAcceptance(getenv(EnvAccept))
	errs = append(errs, err)
	s.Accept = a
	if s.Records == "" {
		errs = append(errs, fmt.Errorf("harness: %s names no records directory", EnvRecords))
	}
	if (s.BaselineRuralzd == "") != (s.BaselineVersion == "") {
		errs = append(errs, fmt.Errorf("harness: %s and %s go together", EnvBaselineBin, EnvBaselineRef))
	}
	return s, errors.Join(errs...)
}

// Build is one side's ruralzd: its binary, commit, version and the
// toolchain that built it.
type Build struct {
	Ruralzd   string
	Ruralz    string
	Commit    string
	Version   string
	Toolchain string
}

// gitOutput runs git in dir.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed git queries
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("harness: git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// goModToolchain returns the toolchain line of a go.mod ("go1.27.1"), or
// "go" plus its go line without one.
func goModToolchain(gomod []byte) string {
	var goLine string
	for line := range strings.SplitSeq(string(gomod), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "toolchain" {
			return f[1]
		}
		if len(f) == 2 && f[0] == "go" {
			goLine = "go" + f[1]
		}
	}
	return goLine
}

// BuildTree builds the release ruralzd and ruralz of the tree at root. A
// baseline tree builds with its own go.mod toolchain line (own true), so a
// Go upgrade shows in the comparison (PBB "Reproducibility"); the head
// builds with the job's toolchain, which setup-go took from the same line.
// out receives the binaries.
func BuildTree(ctx context.Context, root, out string, own bool) (Build, error) {
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod")) //nolint:gosec // G304: the tree's go.mod
	if err != nil {
		return Build{}, err
	}
	commit, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return Build{}, err
	}
	version, err := gitOutput(ctx, root, "describe", "--tags", "--match", "v[0-9]*", "--always")
	if err != nil {
		return Build{}, err
	}
	version = strings.TrimPrefix(version, "v")
	if !strings.Contains(version, ".") {
		version = "0.0.0-dev+" + version
	}
	o := binaries.BuildOptions{Commit: commit, Version: version}
	tc := goModToolchain(gomod)
	if own {
		o.Env = []string{"GOTOOLCHAIN=" + tc}
	} else if tc, err = goVersion(ctx, root); err != nil {
		return Build{}, err
	}
	set, err := binaries.Build(ctx, root, out, o)
	if err != nil {
		return Build{}, err
	}
	return Build{Ruralzd: set.Ruralzd, Ruralz: set.Ruralz, Commit: commit, Version: version, Toolchain: tc}, nil
}

// goVersion returns the toolchain the go command in dir runs.
func goVersion(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "GOVERSION")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("harness: go env GOVERSION: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Worktree checks out commit in a detached worktree of repo at dir; the
// returned function removes it.
func Worktree(ctx context.Context, repo, dir, commit string) (func() error, error) {
	if !commitPattern().MatchString(commit) {
		return nil, fmt.Errorf("harness: %q is not a commit", commit)
	}
	if _, err := gitOutput(ctx, repo, "worktree", "add", "--detach", "--quiet", dir, commit); err != nil {
		return nil, err
	}
	return func() error {
		_, err := gitOutput(context.WithoutCancel(ctx), repo, "worktree", "remove", "--force", dir)
		return err
	}, nil
}

// Revision is the Revision both sides load: the head's render of the
// scenario Bundle for the profile.
type Revision struct {
	// Rendered is the rendered Bundle on the gateway host.
	Rendered string
	// Digest is the Revision digest (sha256:<64 hex>) and Rev its short
	// name (rev-<12 hex>).
	Digest, Rev string
}

// Render renders the scenario's Bundle for the profile's Environment with
// the head's ruralz and builds its Revision offline, writing the rendered
// Bundle to the gateway host's fixed path.
func Render(ctx context.Context, ruralz, repoRoot string, s Scenario, p *Profile) (Revision, error) {
	bundle, envs := repoPath(repoRoot, s.Bundle), repoPath(repoRoot, s.Environments)
	for _, f := range []string{bundle, envs} {
		if _, err := os.Stat(f); err != nil {
			return Revision{}, fmt.Errorf("harness: scenario %s: %w", s.ID, err)
		}
	}
	rendered := filepath.Join(p.Hosts[RoleGateway].Dir, "config", strings.ToLower(s.ID)+".yaml")
	if err := os.MkdirAll(filepath.Dir(rendered), 0o750); err != nil {
		return Revision{}, err
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, ruralz, args...) //nolint:gosec // the head's ruralz
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("harness: ruralz %s: %w: %s%s", strings.Join(args, " "), err, errb.String(), out.String())
		}
		return out.Bytes(), nil
	}
	if _, err := run("bundle", "render", bundle, "--env", p.Name, "--environments", envs, "--output-file", rendered); err != nil {
		return Revision{}, err
	}
	out, err := run("bundle", "build", bundle, "--env", p.Name, "--environments", envs, "--offline", "--output", "json")
	if err != nil {
		return Revision{}, err
	}
	var doc struct {
		Digest   string `json:"digest"`
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(out, &doc); err != nil || doc.Digest == "" {
		return Revision{}, fmt.Errorf("harness: ruralz bundle build output %q: %v", out, err)
	}
	return Revision{Rendered: rendered, Digest: doc.Digest, Rev: doc.Revision}, nil
}

// repoPath resolves a repository-relative, slash-separated path; an
// absolute path stays as it is.
func repoPath(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, filepath.FromSlash(p))
}

// Session is one harness job: the profile, the tools on the lab hosts and
// the recorder.
type Session struct {
	Profile  *Profile
	Settings Settings
	RepoRoot string
	// Work is the harness's local working directory.
	Work string
	// Labhost is the local labhost binary.
	Labhost string
	// Tools maps oha and vegeta to their paths on the load host.
	Tools map[Tool]string
	// ToolVersions maps each load tool to its version.
	ToolVersions map[Tool]string
	// Facts are the gateway host's facts for the records.
	Facts lab.HostFacts
	Rec   *Recorder
	// OpenFiles is the open-file limit of the processes the session starts
	// on its own host, the Node's among them; 0 where it is unknown.
	OpenFiles uint64
}

// CheckOpenFiles returns an error when a run whose processes on the
// harness's host each need need open files exceeds OpenFiles, so a host
// whose limit is too low fails with its cause instead of with the dial
// errors and open breakers of a starved Node.
func (s *Session) CheckOpenFiles(run string, need int) error {
	if s.OpenFiles == 0 || need <= 0 || s.OpenFiles >= uint64(need) {
		return nil
	}
	return fmt.Errorf("harness: %s needs %d open files per process and this host allows %d: raise the hard limit (LimitNOFILE, ulimit -Hn)", run, need, s.OpenFiles)
}

// NewSession builds labhost, copies the load tools to the load host,
// reads the gateway host's facts and gives the processes it starts on its
// own host its open-file limit (InheritOpenFiles).
func NewSession(ctx context.Context, p *Profile, st Settings, repoRoot, work string) (*Session, error) {
	s := &Session{
		Profile: p, Settings: st, RepoRoot: repoRoot, Work: work,
		Labhost: filepath.Join(work, "labhost"), Tools: map[Tool]string{}, ToolVersions: map[Tool]string{},
		Rec: &Recorder{Dir: st.Records},
	}
	files, err := InheritOpenFiles()
	if err != nil {
		return nil, err
	}
	s.OpenFiles = files
	if err := BuildLabhost(ctx, repoRoot, s.Labhost); err != nil {
		return nil, err
	}
	commit, err := gitOutput(ctx, repoRoot, "rev-parse", "--short=12", "HEAD")
	if err != nil {
		return nil, err
	}
	s.ToolVersions[ToolLoadgen] = "ruralz " + commit
	for _, t := range []struct {
		tool Tool
		path string
		flag string
	}{{ToolOha, st.Oha, "--version"}, {ToolVegeta, st.Vegeta, "-version"}} {
		if t.path == "" {
			continue
		}
		dst := filepath.Join(p.Hosts[RoleLoad].Dir, "bin", string(t.tool))
		if err := p.Runner(RoleLoad).Put(ctx, t.path, dst, 0o700); err != nil {
			return nil, err
		}
		s.Tools[t.tool] = dst
		out, err := exec.CommandContext(ctx, t.path, t.flag).CombinedOutput() //nolint:gosec // the pinned tool
		if err != nil {
			return nil, fmt.Errorf("harness: %s %s: %w", t.tool, t.flag, err)
		}
		s.ToolVersions[t.tool] = firstLine(string(out))
	}
	s.Facts = lab.ReadHostFacts("/")
	return s, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

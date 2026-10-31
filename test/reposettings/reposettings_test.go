// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package reposettings tests scripts/repo-settings.sh, the repository
// settings script of docs/engineering/02-repository-layout-and-conventions.md
// "Repository settings", without the network and without touching any
// real repository: the script drives the host's real gh, which reaches a
// fake GitHub REST API through an HTTP proxy (GH_HOST=github.localhost,
// which gh calls over plain HTTP), so the cases check the script's gh
// calls and jq filters as GitHub would answer them. The fake records
// every call and refuses any other host. Cases need bash (Git for
// Windows' bash on Windows) and gh, and skip without them; the case
// without gh needs only bash.
package reposettings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runTimeout bounds one run of the script; a run takes a few seconds, so
// only a hung run reaches it.
const runTimeout = 3 * time.Minute

// The areas the script compares and writes, in its order.
func areas() []string {
	return []string{
		"Merging",
		"Security: secret scanning",
		"Security: private vulnerability reporting",
		"Security: Dependabot alerts",
		"Security: Dependabot security updates",
		"Actions: permissions",
		"Actions: workflow token",
		"Actions: fork pull requests",
		"Releases: immutable releases",
		`Ruleset "main and release-*"`,
		`Ruleset "v* tag creation"`,
		`Ruleset "v* tag updates and deletion"`,
		`Environment "release"`,
		`Environment "release" deployment branches`,
	}
}

// noAPISteps is what both modes print last: the steps no API covers.
const noAPISteps = `Steps no API covers, applied by hand after the first candidate's image push
(docs/engineering/04-release-versioning-and-compatibility.md, "Release runbook", step 8);
repo-settings neither reads nor writes them:
  1. Make the package ghcr.io/ravindu-rev/ruralzd public: in its settings,
     https://github.com/users/ravindu-rev/packages/container/ruralzd/settings,
     change its visibility to public.
  2. Link the package to the repository ravindu-rev/ruralz: on its page,
     https://github.com/users/ravindu-rev/packages/container/package/ruralzd,
     connect the repository ravindu-rev/ruralz.
`

// freshState is a new public repository with GitHub's defaults, a write
// token for workflows and every setting of the table still to apply.
func freshState() *ghState {
	return &ghState{
		OwnerType:        "User",
		AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: true,
		SquashMergeCommitTitle: "COMMIT_OR_PR_TITLE", SquashMergeCommitMessage: "COMMIT_MESSAGES",
		SecretScanning: "disabled", PushProtection: "disabled",
		ActionsEnabled: true, AllowedActions: "all",
		DefaultWorkflowPermissions: "write", CanApprovePullRequests: true,
		ApprovalPolicy: "first_time_contributors",
		NextRulesetID:  1, NextPolicyID: 1,
	}
}

// adminActor is the bypass actor of the repository role admin.
func adminActor() bypassActor {
	role := adminRole
	return bypassActor{ActorID: &role, ActorType: "RepositoryRole", BypassMode: "always"}
}

// The settings table's rulesets
// (docs/engineering/02-repository-layout-and-conventions.md, "Repository
// settings"), as GitHub stores them.
func branchesRuleset() ruleset {
	checks := []any{}
	for _, c := range []string{"pr-fast", "pr-full", "pr-title"} {
		checks = append(checks, map[string]any{"context": c, "integration_id": float64(actionsApp)})
	}
	return ruleset{
		Name: "main and release-*", Target: "branch", Enforcement: "active",
		Include: []string{"refs/heads/main", "refs/heads/release-*"}, Exclude: []string{}, BypassActors: []bypassActor{},
		Rules: []rule{
			{Type: "deletion"},
			{Type: "non_fast_forward"},
			{Type: "pull_request", Parameters: map[string]any{
				"allowed_merge_methods": []any{"squash"}, "dismiss_stale_reviews_on_push": false,
				"require_code_owner_review": false, "require_last_push_approval": false,
				"required_approving_review_count": float64(0), "required_review_thread_resolution": false,
			}},
			{Type: "required_status_checks", Parameters: map[string]any{
				"do_not_enforce_on_create": true, "strict_required_status_checks_policy": true, "required_status_checks": checks,
			}},
		},
	}
}

func tagCreationRuleset() ruleset {
	return ruleset{
		Name: "v* tag creation", Target: "tag", Enforcement: "active",
		Include: []string{"refs/tags/v*"}, Exclude: []string{},
		BypassActors: []bypassActor{adminActor()},
		Rules:        []rule{{Type: "creation"}},
	}
}

func tagChangesRuleset() ruleset {
	return ruleset{
		Name: "v* tag updates and deletion", Target: "tag", Enforcement: "active",
		Include: []string{"refs/tags/v*"}, Exclude: []string{}, BypassActors: []bypassActor{},
		Rules: []rule{{Type: "update"}, {Type: "deletion"}},
	}
}

// desiredState is the repository with every setting of the table.
func desiredState() *ghState {
	s := freshState()
	s.AllowMergeCommit, s.AllowRebaseMerge, s.DeleteBranchOnMerge = false, false, true
	s.SquashMergeCommitTitle, s.SquashMergeCommitMessage = "PR_TITLE", "COMMIT_MESSAGES"
	s.SecretScanning, s.PushProtection = "enabled", "enabled"
	s.SHAPinningRequired = true
	s.DefaultWorkflowPermissions, s.CanApprovePullRequests = "read", false
	s.ApprovalPolicy = "all_external_contributors"
	s.PrivateVulnerabilityReporting, s.DependabotAlerts, s.SecurityUpdates, s.ImmutableReleases = true, true, true, true
	s.Rulesets = []ruleset{branchesRuleset(), tagCreationRuleset(), tagChangesRuleset()}
	for i := range s.Rulesets {
		s.Rulesets[i].ID = i + 1
	}
	s.NextRulesetID = 4
	s.Environment = &environment{
		Reviewers:    []int{ownerID},
		BranchPolicy: &branchScheme{ProtectedBranches: false, CustomBranchPolicies: true},
	}
	s.BranchPolicies = []branchPolicy{{ID: 1, Name: "v*", Type: "tag"}}
	s.NextPolicyID = 2
	return s
}

// ruleOf returns the rule of type typ in the ruleset named name.
func ruleOf(t *testing.T, s *ghState, name, typ string) *rule {
	t.Helper()
	rs := rulesetNamed(t, s, name)
	i := slices.IndexFunc(rs.Rules, func(r rule) bool { return r.Type == typ })
	if i < 0 {
		t.Fatalf("ruleset %q has no %s rule", name, typ)
	}
	return &rs.Rules[i]
}

func rulesetNamed(t *testing.T, s *ghState, name string) *ruleset {
	t.Helper()
	i := slices.IndexFunc(s.Rulesets, func(rs ruleset) bool { return rs.Name == name })
	if i < 0 {
		t.Fatalf("no ruleset %q", name)
	}
	return &s.Rulesets[i]
}

// normalized returns the state as JSON with the server-assigned IDs and
// the orders GitHub does not keep left out, to compare two states.
func normalized(t *testing.T, s *ghState) string {
	t.Helper()
	c := cloneState(s)
	c.NextRulesetID, c.NextPolicyID = 0, 0
	for i := range c.Rulesets {
		rs := &c.Rulesets[i]
		rs.ID = 0
		sort.Strings(rs.Include)
		sort.Strings(rs.Exclude)
		sort.Slice(rs.Rules, func(a, b int) bool { return rs.Rules[a].Type < rs.Rules[b].Type })
	}
	sort.Slice(c.Rulesets, func(a, b int) bool { return c.Rulesets[a].Name < c.Rulesets[b].Name })
	for i := range c.BranchPolicies {
		c.BranchPolicies[i].ID = 0
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// repoRoot returns the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "scripts", "repo-settings.sh")); err != nil {
		t.Fatalf("no scripts/repo-settings.sh under %s: %v", root, err)
	}
	return root
}

// scriptTools are the commands repo-settings.sh calls besides gh and the
// bash builtins.
func scriptTools() []string { return []string{"awk", "cat", "mktemp", "mv", "rm"} }

// host is how a case runs the script: the bash to run it with and the
// PATH it gets, with or without gh.
type host struct {
	bash               string
	withGH, withoutGH  string
	windowsEnvironment []string
}

// findHost locates bash, the script's tools and, when needGH, gh, or
// skips the case.
func findHost(t *testing.T, needGH bool) host {
	t.Helper()
	gh, ghErr := exec.LookPath("gh")
	if needGH && ghErr != nil {
		t.Skip("gh not installed: the cases drive the real GitHub CLI against a fake API")
	}
	if runtime.GOOS == "windows" {
		return windowsHost(t, gh)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	// A directory of links to the tools alone, so a case can leave gh
	// out even where gh shares a directory with them.
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(scriptTools(), "bash") {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s not installed", name)
		}
		if err := os.Symlink(p, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	h := host{bash: bash, withoutGH: dir}
	if gh != "" {
		ghDir := filepath.Join(filepath.Dir(dir), "gh")
		if err := os.Mkdir(ghDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(gh, filepath.Join(ghDir, "gh")); err != nil {
			t.Fatal(err)
		}
		h.withGH = ghDir + string(os.PathListSeparator) + dir
	}
	return h
}

// windowsHost runs the script with Git for Windows' bash, whose usr\bin
// holds the script's tools; the bash on PATH may instead be WSL's.
func windowsHost(t *testing.T, gh string) host {
	t.Helper()
	msys := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "mktemp.exe"))
		return err == nil
	}
	var usrBin string
	if p, err := exec.LookPath("bash"); err == nil && msys(filepath.Dir(p)) {
		usrBin = filepath.Dir(p)
	} else if git, err := exec.LookPath("git"); err == nil {
		for dir := filepath.Dir(git); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if msys(filepath.Join(dir, "usr", "bin")) {
				usrBin = filepath.Join(dir, "usr", "bin")
				break
			}
		}
	}
	if usrBin == "" {
		t.Skip("Git for Windows' bash not found")
	}
	h := host{bash: filepath.Join(usrBin, "bash.exe"), withoutGH: usrBin}
	if gh != "" {
		h.withGH = filepath.Dir(gh) + string(os.PathListSeparator) + usrBin
	}
	// gh and Go programs need these on Windows.
	for _, k := range []string{"SYSTEMROOT", "WINDIR", "PATHEXT"} {
		if v, ok := os.LookupEnv(k); ok {
			h.windowsEnvironment = append(h.windowsEnvironment, k+"="+v)
		}
	}
	return h
}

// result is one run of the script.
type result struct {
	code           int
	stdout, stderr string
	calls          []call
	state          *ghState
}

// writes returns the run's writes as "METHOD URI".
func (r result) writes() []string {
	var out []string
	for _, c := range r.calls {
		if c.write() {
			out = append(out, c.Method+" "+c.URI)
		}
	}
	return out
}

// drift returns the run's "drift:" lines.
func (r result) drift() []string {
	var out []string
	for _, l := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(l, "drift: ") {
			out = append(out, l)
		}
	}
	return out
}

func (r result) String() string {
	return "exit " + strconv.Itoa(r.code) + "\n--- stdout\n" + r.stdout + "--- stderr\n" + r.stderr
}

// run runs the script with args against a fake serving s with faults f;
// withGH puts gh on its PATH.
func run(t *testing.T, s *ghState, f faults, withGH bool, args ...string) result {
	t.Helper()
	h := findHost(t, withGH)
	fake := newFake(s, f)
	srv := httptest.NewServer(fake)
	defer srv.Close()

	dir := t.TempDir()
	home, tmp, config := filepath.Join(dir, "home"), filepath.Join(dir, "tmp"), filepath.Join(dir, "gh-config")
	for _, d := range []string{home, tmp, config} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	path := h.withoutGH
	if withGH {
		path = h.withGH
	}
	env := append([]string{
		"PATH=" + path, "HOME=" + home, "TMPDIR=" + tmp, "TMP=" + tmp, "TEMP=" + tmp, "LC_ALL=C",
		// The real gh, logged in to the fake only: GH_HOST makes it call
		// http://api.github.localhost/, which the proxy variables send to
		// the fake, and any other host meets the fake too, which refuses it.
		"GH_HOST=github.localhost", "GH_TOKEN=fake-token-for-the-fake-api", "GH_CONFIG_DIR=" + config,
		"GH_NO_UPDATE_NOTIFIER=1", "GH_NO_EXTENSION_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1", "NO_COLOR=1",
		// gh's usage telemetry, which would reach for its own host.
		"GH_TELEMETRY=false", "DO_NOT_TRACK=1",
		"HTTP_PROXY=" + srv.URL, "HTTPS_PROXY=" + srv.URL, "http_proxy=" + srv.URL, "https_proxy=" + srv.URL,
	}, h.windowsEnvironment...)

	script := filepath.Join(repoRoot(t), "scripts", "repo-settings.sh")
	if runtime.GOOS == "windows" {
		script = filepath.ToSlash(script)
	}
	ctx, cancel := context.WithTimeout(t.Context(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bash, append([]string{script}, args...)...) //nolint:gosec // G204: bash runs the repository's repo-settings.sh.
	cmd.Env, cmd.Dir = env, dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := result{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		r.code = exit.ExitCode()
	default:
		t.Fatalf("run %v: %v", args, err)
	}
	var unexpected []string
	r.calls, unexpected, r.state = fake.snapshot()
	if len(unexpected) > 0 {
		t.Errorf("the script made requests the fake does not model: %q\n%v", unexpected, r)
	}
	return r
}

func check(t *testing.T, s *ghState) result {
	t.Helper()
	return run(t, s, faults{}, true, "--check")
}

func apply(t *testing.T, s *ghState, f faults) result {
	t.Helper()
	return run(t, s, f, true)
}

// wantCode fails the case unless the run exited with code.
func wantCode(t *testing.T, r result, code int) {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit %d, want %d\n%v", r.code, code, r)
	}
}

func wantLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func wantContains(t *testing.T, r result, stream, text string) {
	t.Helper()
	s := r.stdout
	if stream == "stderr" {
		s = r.stderr
	}
	if !strings.Contains(s, text) {
		t.Errorf("%s lacks %q\n%v", stream, text, r)
	}
}

// TestCheckPassesWhenSettingsMatch: --check on a repository with every
// setting of the table reads only, finds no drift and exits 0.
func TestCheckPassesWhenSettingsMatch(t *testing.T) {
	t.Parallel()
	r := check(t, desiredState())
	wantCode(t, r, 0)
	wantLines(t, "writes", r.writes(), nil)
	wantLines(t, "drift", r.drift(), nil)
	wantContains(t, r, "stdout", "repo-settings: no drift from the settings table\n")
	if r.stderr != "" {
		t.Errorf("stderr is not empty\n%v", r)
	}
}

// TestCheckIgnoresSettingsOutsideTheTable: a setting the table does not
// name, such as a rule parameter GitHub adds or the allowed actions
// policy, never reads as drift.
func TestCheckIgnoresSettingsOutsideTheTable(t *testing.T) {
	t.Parallel()
	s := desiredState()
	ruleOf(t, s, "main and release-*", "pull_request").Parameters["automatic_copilot_code_review_enabled"] = true
	s.AllowedActions = "selected"
	r := check(t, s)
	wantCode(t, r, 0)
	wantLines(t, "drift", r.drift(), nil)
}

// TestCheckReportsDrift: --check prints one "drift:" line per setting
// that differs from the table, writes nothing and exits 1.
func TestCheckReportsDrift(t *testing.T) {
	t.Parallel()
	const (
		branches   = `Ruleset "main and release-*"`
		creation   = `Ruleset "v* tag creation"`
		changes    = `Ruleset "v* tag updates and deletion"`
		env        = `Environment "release"`
		envBranch  = `Environment "release" deployment branches`
		checksWant = `[{"context":"pr-fast","integration_id":15368},{"context":"pr-full","integration_id":15368},{"context":"pr-title","integration_id":15368}]`
		adminByp   = `[{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}]`
	)
	cases := []struct {
		name   string
		mutate func(t *testing.T, s *ghState)
		drift  []string
	}{
		{"merge commits and rebase merging", func(_ *testing.T, s *ghState) {
			s.AllowMergeCommit, s.AllowRebaseMerge = true, true
		}, []string{
			"drift: Merging: allow_merge_commit is true, want false",
			"drift: Merging: allow_rebase_merge is true, want false",
		}},
		{"no squash merging", func(_ *testing.T, s *ghState) { s.AllowSquashMerge = false }, []string{
			"drift: Merging: allow_squash_merge is false, want true",
		}},
		{"squash commit title and message", func(_ *testing.T, s *ghState) {
			s.SquashMergeCommitTitle, s.SquashMergeCommitMessage = "COMMIT_OR_PR_TITLE", "PR_BODY"
		}, []string{
			`drift: Merging: squash_merge_commit_title is "COMMIT_OR_PR_TITLE", want "PR_TITLE"`,
			`drift: Merging: squash_merge_commit_message is "PR_BODY", want "COMMIT_MESSAGES"`,
		}},
		{"head branches kept", func(_ *testing.T, s *ghState) { s.DeleteBranchOnMerge = false }, []string{
			"drift: Merging: delete_branch_on_merge is false, want true",
		}},
		{"secret scanning off", func(_ *testing.T, s *ghState) { s.SecretScanning, s.PushProtection = "disabled", "disabled" }, []string{
			`drift: Security: secret scanning: secret_scanning is "disabled", want "enabled"`,
			`drift: Security: secret scanning: secret_scanning_push_protection is "disabled", want "enabled"`,
		}},
		{"private vulnerability reporting off", func(_ *testing.T, s *ghState) { s.PrivateVulnerabilityReporting = false }, []string{
			"drift: Security: private vulnerability reporting: enabled is false, want true",
		}},
		{"Dependabot alerts and security updates off", func(_ *testing.T, s *ghState) {
			s.DependabotAlerts, s.SecurityUpdates = false, false
		}, []string{
			"drift: Security: Dependabot alerts: enabled is false, want true",
			"drift: Security: Dependabot security updates: enabled is false, want true",
		}},
		{"security updates paused", func(_ *testing.T, s *ghState) { s.SecurityUpdatesPaused = true }, []string{
			"drift: Security: Dependabot security updates: paused is true, want false",
		}},
		{"Actions off", func(_ *testing.T, s *ghState) { s.ActionsEnabled = false }, []string{
			"drift: Actions: permissions: enabled is false, want true",
		}},
		{"actions not pinned to a SHA", func(_ *testing.T, s *ghState) { s.SHAPinningRequired = false }, []string{
			"drift: Actions: permissions: sha_pinning_required is false, want true",
		}},
		{"workflow token writes and approves", func(_ *testing.T, s *ghState) {
			s.DefaultWorkflowPermissions, s.CanApprovePullRequests = "write", true
		}, []string{
			`drift: Actions: workflow token: default_workflow_permissions is "write", want "read"`,
			"drift: Actions: workflow token: can_approve_pull_request_reviews is true, want false",
		}},
		{"first-time contributors only", func(_ *testing.T, s *ghState) { s.ApprovalPolicy = "first_time_contributors" }, []string{
			`drift: Actions: fork pull requests: approval_policy is "first_time_contributors", want "all_external_contributors"`,
		}},
		{"mutable releases", func(_ *testing.T, s *ghState) { s.ImmutableReleases = false }, []string{
			"drift: Releases: immutable releases: enabled is false, want true",
		}},
		{"branch ruleset missing", func(_ *testing.T, s *ghState) { s.Rulesets = s.Rulesets[1:] }, []string{
			"drift: " + branches + ": missing",
		}},
		{"branch ruleset renamed", func(t *testing.T, s *ghState) { rulesetNamed(t, s, "main and release-*").Name = "main" }, []string{
			"drift: " + branches + ": missing",
		}},
		{"branch ruleset disabled", func(t *testing.T, s *ghState) { rulesetNamed(t, s, "main and release-*").Enforcement = "disabled" }, []string{
			"drift: " + branches + `: enforcement is "disabled", want "active"`,
		}},
		{"branch ruleset on main only", func(t *testing.T, s *ghState) {
			rulesetNamed(t, s, "main and release-*").Include = []string{"refs/heads/main"}
		}, []string{
			"drift: " + branches + `: include is ["refs/heads/main"], want ["refs/heads/main","refs/heads/release-*"]`,
		}},
		{"branch ruleset excludes a release branch", func(t *testing.T, s *ghState) {
			rulesetNamed(t, s, "main and release-*").Exclude = []string{"refs/heads/release-0.1"}
		}, []string{
			"drift: " + branches + `: exclude is ["refs/heads/release-0.1"], want []`,
		}},
		{"admins bypass the branch ruleset", func(t *testing.T, s *ghState) {
			rulesetNamed(t, s, "main and release-*").BypassActors = []bypassActor{adminActor()}
		}, []string{
			"drift: " + branches + ": bypass_actors is " + adminByp + ", want []",
		}},
		{"force pushes allowed", func(t *testing.T, s *ghState) {
			rs := rulesetNamed(t, s, "main and release-*")
			rs.Rules = slices.DeleteFunc(rs.Rules, func(r rule) bool { return r.Type == "non_fast_forward" })
		}, []string{
			"drift: " + branches + `: rules is ["deletion","pull_request","required_status_checks"], want ["deletion","non_fast_forward","pull_request","required_status_checks"]`,
		}},
		{"an extra rule", func(t *testing.T, s *ghState) {
			rs := rulesetNamed(t, s, "main and release-*")
			rs.Rules = append(rs.Rules, rule{Type: "required_signatures"})
		}, []string{
			"drift: " + branches + `: rules is ["deletion","non_fast_forward","pull_request","required_signatures","required_status_checks"], want ["deletion","non_fast_forward","pull_request","required_status_checks"]`,
		}},
		{"an approval from a code owner", func(t *testing.T, s *ghState) {
			p := ruleOf(t, s, "main and release-*", "pull_request").Parameters
			p["required_approving_review_count"], p["require_code_owner_review"] = float64(1), true
		}, []string{
			"drift: " + branches + ": pull_request.require_code_owner_review is true, want false",
			"drift: " + branches + ": pull_request.required_approving_review_count is 1, want 0",
		}},
		{"merge commits through the ruleset", func(t *testing.T, s *ghState) {
			ruleOf(t, s, "main and release-*", "pull_request").Parameters["allowed_merge_methods"] = []any{"squash", "merge"}
		}, []string{
			"drift: " + branches + `: pull_request.allowed_merge_methods is ["merge","squash"], want ["squash"]`,
		}},
		{"resolved conversations required", func(t *testing.T, s *ghState) {
			ruleOf(t, s, "main and release-*", "pull_request").Parameters["required_review_thread_resolution"] = true
		}, []string{
			"drift: " + branches + ": pull_request.required_review_thread_resolution is true, want false",
		}},
		{"pr-title from any source", func(t *testing.T, s *ghState) {
			ruleOf(t, s, "main and release-*", "required_status_checks").Parameters["required_status_checks"] = []any{
				map[string]any{"context": "pr-fast", "integration_id": float64(actionsApp)},
				map[string]any{"context": "pr-full", "integration_id": float64(actionsApp)},
				map[string]any{"context": "pr-title"},
			}
		}, []string{
			"drift: " + branches + `: required_status_checks.required_status_checks is [{"context":"pr-title"},{"context":"pr-fast","integration_id":15368},{"context":"pr-full","integration_id":15368}], want ` + checksWant,
		}},
		{"pr-full not required", func(t *testing.T, s *ghState) {
			ruleOf(t, s, "main and release-*", "required_status_checks").Parameters["required_status_checks"] = []any{
				map[string]any{"context": "pr-fast", "integration_id": float64(actionsApp)},
				map[string]any{"context": "pr-title", "integration_id": float64(actionsApp)},
			}
		}, []string{
			"drift: " + branches + `: required_status_checks.required_status_checks is [{"context":"pr-fast","integration_id":15368},{"context":"pr-title","integration_id":15368}], want ` + checksWant,
		}},
		{"branches merge out of date", func(t *testing.T, s *ghState) {
			ruleOf(t, s, "main and release-*", "required_status_checks").Parameters["strict_required_status_checks_policy"] = false
		}, []string{
			"drift: " + branches + ": required_status_checks.strict_required_status_checks_policy is false, want true",
		}},
		{"checks enforced on branch creation", func(t *testing.T, s *ghState) {
			delete(ruleOf(t, s, "main and release-*", "required_status_checks").Parameters, "do_not_enforce_on_create")
		}, []string{
			"drift: " + branches + ": required_status_checks.do_not_enforce_on_create is absent, want true",
		}},
		{"anyone creates v* tags", func(t *testing.T, s *ghState) {
			rulesetNamed(t, s, "v* tag creation").BypassActors = []bypassActor{}
		}, []string{
			"drift: " + creation + ": bypass_actors is [], want " + adminByp,
		}},
		{"the tag creation ruleset on every tag", func(t *testing.T, s *ghState) {
			rulesetNamed(t, s, "v* tag creation").Include = []string{"~ALL"}
		}, []string{
			"drift: " + creation + `: include is ["~ALL"], want ["refs/tags/v*"]`,
		}},
		{"v* tags deleted", func(t *testing.T, s *ghState) {
			rs := rulesetNamed(t, s, "v* tag updates and deletion")
			rs.Rules = rs.Rules[:1]
		}, []string{
			"drift: " + changes + `: rules is ["update"], want ["deletion","update"]`,
		}},
		{"v* tags updated", func(t *testing.T, s *ghState) {
			rs := rulesetNamed(t, s, "v* tag updates and deletion")
			rs.Rules = rs.Rules[1:]
		}, []string{
			"drift: " + changes + `: rules is ["deletion"], want ["deletion","update"]`,
		}},
		{"admins move v* tags", func(t *testing.T, s *ghState) {
			rulesetNamed(t, s, "v* tag updates and deletion").BypassActors = []bypassActor{adminActor()}
		}, []string{
			"drift: " + changes + ": bypass_actors is " + adminByp + ", want []",
		}},
		{"the tag rulesets on branches", func(t *testing.T, s *ghState) {
			rulesetNamed(t, s, "v* tag creation").Target = "branch"
			rulesetNamed(t, s, "v* tag updates and deletion").Target = "branch"
		}, []string{
			"drift: " + creation + `: target is "branch", want "tag"`,
			"drift: " + changes + `: target is "branch", want "tag"`,
		}},
		{"no release Environment", func(_ *testing.T, s *ghState) { s.Environment, s.BranchPolicies = nil, nil }, []string{
			"drift: " + env + ": missing",
			"drift: " + envBranch + `: branch_policies is [], want [{"name":"v*","type":"tag"}]`,
		}},
		{"another reviewer", func(_ *testing.T, s *ghState) { s.Environment.Reviewers = []int{otherID} }, []string{
			"drift: " + env + `: reviewers is ["User:another-maintainer"], want ["User:ravindu-rev"]`,
		}},
		{"a second reviewer", func(_ *testing.T, s *ghState) { s.Environment.Reviewers = []int{otherID, ownerID} }, []string{
			"drift: " + env + `: reviewers is ["User:another-maintainer","User:ravindu-rev"], want ["User:ravindu-rev"]`,
		}},
		{"no required reviewer", func(_ *testing.T, s *ghState) { s.Environment.Reviewers = nil }, []string{
			"drift: " + env + `: protection_rules is ["branch_policy"], want ["branch_policy","required_reviewers"]`,
			"drift: " + env + `: reviewers is [], want ["User:ravindu-rev"]`,
			"drift: " + env + ": prevent_self_review is null, want false",
		}},
		{"self-review prevented", func(_ *testing.T, s *ghState) { s.Environment.PreventSelfReview = true }, []string{
			"drift: " + env + ": prevent_self_review is true, want false",
		}},
		{"a wait timer", func(_ *testing.T, s *ghState) { s.Environment.WaitTimer = 30 }, []string{
			"drift: " + env + `: protection_rules is ["branch_policy","required_reviewers","wait_timer"], want ["branch_policy","required_reviewers"]`,
		}},
		{"every branch deploys", func(_ *testing.T, s *ghState) { s.Environment.BranchPolicy, s.BranchPolicies = nil, nil }, []string{
			"drift: " + env + `: deployment_branch_policy is null, want {"custom_branch_policies":true,"protected_branches":false}`,
			"drift: " + env + `: protection_rules is ["required_reviewers"], want ["branch_policy","required_reviewers"]`,
			"drift: " + envBranch + `: branch_policies is [], want [{"name":"v*","type":"tag"}]`,
		}},
		{"protected branches deploy", func(_ *testing.T, s *ghState) {
			s.Environment.BranchPolicy, s.BranchPolicies = &branchScheme{ProtectedBranches: true}, nil
		}, []string{
			"drift: " + env + `: deployment_branch_policy is {"custom_branch_policies":false,"protected_branches":true}, want {"custom_branch_policies":true,"protected_branches":false}`,
			"drift: " + envBranch + `: branch_policies is [], want [{"name":"v*","type":"tag"}]`,
		}},
		{"main deploys too", func(_ *testing.T, s *ghState) {
			s.BranchPolicies = append(s.BranchPolicies, branchPolicy{ID: 7, Name: "main", Type: "branch"})
		}, []string{
			"drift: " + envBranch + `: branch_policies is [{"name":"main","type":"branch"},{"name":"v*","type":"tag"}], want [{"name":"v*","type":"tag"}]`,
		}},
		{"a branch named v*", func(_ *testing.T, s *ghState) { s.BranchPolicies[0].Type = "branch" }, []string{
			"drift: " + envBranch + `: branch_policies is [{"name":"v*","type":"branch"}], want [{"name":"v*","type":"tag"}]`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := desiredState()
			tc.mutate(t, s)
			before := normalized(t, s)
			r := check(t, s)
			wantCode(t, r, 1)
			wantLines(t, "drift", r.drift(), tc.drift)
			wantLines(t, "writes", r.writes(), nil)
			if got := normalized(t, r.state); got != before {
				t.Errorf("--check changed the settings")
			}
			wantContains(t, r, "stdout", "repo-settings: "+strconv.Itoa(len(tc.drift))+" setting(s) drift from the settings table")
		})
	}
}

// TestApplyConfiguresAFreshRepository: applying the table to a new
// repository writes each area once, in order, after which the settings
// equal the table's and --check passes.
func TestApplyConfiguresAFreshRepository(t *testing.T) {
	t.Parallel()
	r := apply(t, freshState(), faults{})
	wantCode(t, r, 0)
	wantLines(t, "writes", r.writes(), []string{
		"PATCH " + repoPath,
		"PATCH " + repoPath,
		"PUT " + repoPath + "/private-vulnerability-reporting",
		"PUT " + repoPath + "/vulnerability-alerts",
		"PUT " + repoPath + "/automated-security-fixes",
		"PUT " + repoPath + "/actions/permissions",
		"PUT " + repoPath + "/actions/permissions/workflow",
		"PUT " + repoPath + "/actions/permissions/fork-pr-contributor-approval",
		"PUT " + repoPath + "/immutable-releases",
		"POST " + repoPath + "/rulesets",
		"POST " + repoPath + "/rulesets",
		"POST " + repoPath + "/rulesets",
		"PUT " + repoPath + "/environments/release",
		"POST " + repoPath + "/environments/release/deployment-branch-policies",
	})
	var updated []string
	for _, a := range areas() {
		updated = append(updated, "updated: "+a)
	}
	wantLines(t, "updated areas", linesWithPrefix(r.stdout, "updated: "), updated)
	wantContains(t, r, "stdout", "repo-settings: 14 area(s) updated, 0 unchanged\n")
	if got, want := normalized(t, r.state), normalized(t, desiredState()); got != want {
		t.Errorf("settings after apply:\n%s\nwant\n%s", got, want)
	}
	again := check(t, r.state)
	wantCode(t, again, 0)
}

// TestApplyTagUpdateRuleWithoutParameters: GitHub keeps
// update_allows_fetch_and_merge only on branch rulesets, so the update
// rule of the v* tag updates and deletion ruleset reads back without
// parameters; apply writes it without any and finds it matching.
func TestApplyTagUpdateRuleWithoutParameters(t *testing.T) {
	t.Parallel()
	r := apply(t, freshState(), faults{})
	wantCode(t, r, 0)
	if strings.Contains(r.stderr, "still differs") {
		t.Errorf("apply found a setting differing after its write:\n%s", r)
	}
	if p := ruleOf(t, r.state, "v* tag updates and deletion", "update").Parameters; len(p) != 0 {
		t.Errorf("the v* tag update rule reads back with parameters %v, want none", p)
	}
}

// TestApplyWritesNothingWhenSettingsMatch: applying the table to a
// repository that already matches it only reads.
func TestApplyWritesNothingWhenSettingsMatch(t *testing.T) {
	t.Parallel()
	r := apply(t, desiredState(), faults{})
	wantCode(t, r, 0)
	wantLines(t, "writes", r.writes(), nil)
	var unchanged []string
	for _, a := range areas() {
		unchanged = append(unchanged, "unchanged: "+a)
	}
	wantLines(t, "unchanged areas", linesWithPrefix(r.stdout, "unchanged: "), unchanged)
	wantContains(t, r, "stdout", "repo-settings: 0 area(s) updated, 14 unchanged\n")
}

// TestApplyUpdatesOnlyDriftedAreas: apply writes just the areas that
// differ, replaces a drifted ruleset in place, keeps the allowed actions
// policy the table does not name, and removes a deployment branch policy
// the table does not list.
func TestApplyUpdatesOnlyDriftedAreas(t *testing.T) {
	t.Parallel()
	s := desiredState()
	s.AllowedActions, s.SHAPinningRequired = "selected", false
	branches := rulesetNamed(t, s, "main and release-*")
	branches.BypassActors = []bypassActor{adminActor()}
	branches.Rules = append(branches.Rules, rule{Type: "required_signatures"})
	ruleOf(t, s, "main and release-*", "pull_request").Parameters["required_approving_review_count"] = float64(1)
	s.Environment.WaitTimer, s.Environment.PreventSelfReview = 30, true
	s.BranchPolicies = append(s.BranchPolicies, branchPolicy{ID: 7, Name: "main", Type: "branch"})
	s.NextPolicyID = 8

	r := apply(t, s, faults{})
	wantCode(t, r, 0)
	wantLines(t, "drift before the writes", r.drift(), []string{
		"drift: Actions: permissions: sha_pinning_required is false, want true",
		`drift: Ruleset "main and release-*": bypass_actors is [{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}], want []`,
		`drift: Ruleset "main and release-*": rules is ["deletion","non_fast_forward","pull_request","required_signatures","required_status_checks"], want ["deletion","non_fast_forward","pull_request","required_status_checks"]`,
		`drift: Ruleset "main and release-*": pull_request.required_approving_review_count is 1, want 0`,
		`drift: Environment "release": protection_rules is ["branch_policy","required_reviewers","wait_timer"], want ["branch_policy","required_reviewers"]`,
		`drift: Environment "release": prevent_self_review is true, want false`,
		`drift: Environment "release" deployment branches: branch_policies is [{"name":"main","type":"branch"},{"name":"v*","type":"tag"}], want [{"name":"v*","type":"tag"}]`,
	})
	wantLines(t, "writes", r.writes(), []string{
		"PUT " + repoPath + "/actions/permissions",
		"PUT " + repoPath + "/rulesets/1",
		"PUT " + repoPath + "/environments/release",
		"DELETE " + repoPath + "/environments/release/deployment-branch-policies/7",
	})
	wantState := desiredState()
	wantState.AllowedActions = "selected"
	if got, want := normalized(t, r.state), normalized(t, wantState); got != want {
		t.Errorf("settings after apply:\n%s\nwant\n%s", got, want)
	}
	wantLines(t, "updated areas", linesWithPrefix(r.stdout, "updated: "), []string{
		"updated: Actions: permissions",
		`updated: Ruleset "main and release-*"`,
		`updated: Environment "release"`,
		`updated: Environment "release" deployment branches`,
	})
}

// TestApplyCreatesTheTagPolicy: with the release Environment's v* policy
// replaced by another, apply deletes that one and creates the v* tag
// policy.
func TestApplyCreatesTheTagPolicy(t *testing.T) {
	t.Parallel()
	s := desiredState()
	s.BranchPolicies = []branchPolicy{{ID: 1, Name: "v*", Type: "branch"}}
	r := apply(t, s, faults{})
	wantCode(t, r, 0)
	wantLines(t, "writes", r.writes(), []string{
		"DELETE " + repoPath + "/environments/release/deployment-branch-policies/1",
		"POST " + repoPath + "/environments/release/deployment-branch-policies",
	})
	if got, want := normalized(t, r.state), normalized(t, desiredState()); got != want {
		t.Errorf("settings after apply:\n%s\nwant\n%s", got, want)
	}
}

// TestNoAPISteps: both modes end by printing the package steps no API
// covers, whose settings the script neither reads nor writes.
func TestNoAPISteps(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"--check"}} {
		r := run(t, desiredState(), faults{}, true, args...)
		wantCode(t, r, 0)
		if !strings.HasSuffix(r.stdout, noAPISteps) {
			t.Errorf("repo-settings %v does not end with the steps no API covers\n%v", args, r)
		}
		for _, c := range r.calls {
			if strings.Contains(c.URI, "packages") {
				t.Errorf("repo-settings %v called %s %s", args, c.Method, c.URI)
			}
		}
	}
}

// TestErrors: usage errors, a missing gh, failed or refused calls,
// settings the token may not read and writes that do not take effect all
// exit 2, and every area the error does not touch is still compared or
// written.
func TestErrors(t *testing.T) {
	t.Parallel()
	server := func(status int, message string) faults {
		return faults{fail: map[string]failure{"GET " + repoPath + "/actions/permissions": {status, message}}}
	}
	cases := []struct {
		name   string
		args   []string
		noGH   bool
		state  func() *ghState
		faults faults
		// stderr lines the run prints; drift and writes, when set, are
		// its drift lines and writes.
		stderr []string
		drift  []string
		writes []string
		noCall bool
	}{
		{
			name: "unknown argument", args: []string{"--apply"}, state: desiredState, noCall: true,
			stderr: []string{"usage: repo-settings [--check]"},
		},
		{
			name: "two arguments", args: []string{"--check", "--check"}, state: desiredState, noCall: true,
			stderr: []string{"usage: repo-settings [--check]"},
		},
		{
			name: "gh missing", args: []string{"--check"}, noGH: true, state: desiredState, noCall: true,
			stderr: []string{"repo-settings: gh not found: install the GitHub CLI, https://cli.github.com"},
		},
		{
			name: "repository not found", args: []string{"--check"}, state: desiredState,
			faults: faults{fail: map[string]failure{"GET " + repoPath: {http.StatusNotFound, "Not Found"}}},
			stderr: []string{
				"repo-settings: GET repos/ravindu-rev/ruralz: Not Found (HTTP 404)",
				"repo-settings: cannot read the repository ravindu-rev/ruralz",
			},
		},
		{
			name: "owner is an organization", args: []string{"--check"},
			state:  func() *ghState { s := desiredState(); s.OwnerType = "Organization"; return s },
			stderr: []string{"repo-settings: the release Environment's reviewer is the owner, a user; ravindu-rev is an account of type Organization"},
		},
		{
			name: "a read fails", args: []string{"--check"}, state: desiredState, faults: server(http.StatusInternalServerError, "Server Error"),
			stderr: []string{
				"repo-settings: GET repos/ravindu-rev/ruralz/actions/permissions: Server Error (HTTP 500)",
				"repo-settings: 1 error(s)",
			}, drift: []string{},
		},
		{
			name: "a read is refused", args: []string{"--check"}, state: desiredState,
			faults: server(http.StatusForbidden, "Resource not accessible by personal access token"),
			stderr: []string{
				"repo-settings: GET repos/ravindu-rev/ruralz/actions/permissions: Resource not accessible by personal access token (HTTP 403)",
			},
		},
		{
			name: "drift and a failed read", args: []string{"--check"},
			state: func() *ghState { s := desiredState(); s.AllowMergeCommit = true; return s },
			faults: faults{fail: map[string]failure{
				"GET " + repoPath + "/immutable-releases": {http.StatusServiceUnavailable, "Service Unavailable"},
			}},
			stderr: []string{"repo-settings: GET repos/ravindu-rev/ruralz/immutable-releases: Service Unavailable (HTTP 503)"},
			drift:  []string{"drift: Merging: allow_merge_commit is true, want false"},
		},
		{
			name: "settings hidden from apply", state: func() *ghState { s := desiredState(); s.AllowMergeCommit = true; return s },
			faults: faults{hideMerge: true},
			stderr: []string{"repo-settings: Merging: cannot read allow_squash_merge: the response leaves it out, so the token may not read it"},
			writes: []string{},
		},
		{
			name: "a write is refused", state: freshState,
			faults: faults{fail: map[string]failure{
				"PUT " + repoPath + "/immutable-releases": {http.StatusForbidden, "Resource not accessible by personal access token"},
			}},
			stderr: []string{
				"repo-settings: PUT repos/ravindu-rev/ruralz/immutable-releases: Resource not accessible by personal access token (HTTP 403)",
				"repo-settings: Releases: immutable releases: not updated",
				"repo-settings: 1 error(s)",
			},
		},
		{
			name: "a write does not take effect", state: func() *ghState {
				s := desiredState()
				s.DefaultWorkflowPermissions = "write"
				return s
			}, faults: faults{ignore: map[string]bool{"PUT " + repoPath + "/actions/permissions/workflow": true}},
			stderr: []string{
				"repo-settings: Actions: workflow token: still differs after its write:",
				`drift: Actions: workflow token: default_workflow_permissions is "write", want "read"`,
			},
			writes: []string{"PUT " + repoPath + "/actions/permissions/workflow"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := run(t, tc.state(), tc.faults, !tc.noGH, tc.args...)
			wantCode(t, r, 2)
			for _, l := range tc.stderr {
				wantContains(t, r, "stderr", l+"\n")
			}
			if tc.noCall && len(r.calls) > 0 {
				t.Errorf("the run made calls: %v", r.calls)
			}
			if tc.drift != nil {
				wantLines(t, "drift", r.drift(), nonNil(tc.drift))
			}
			if tc.writes != nil {
				wantLines(t, "writes", r.writes(), nonNil(tc.writes))
			}
		})
	}
}

// TestCheckSkipsHiddenSettings: --check, run with the weekly workflow's
// read-only token, notes each setting GitHub leaves out of the response
// to that token (docs/engineering/02-repository-layout-and-conventions.md,
// "Repository settings") and checks every other one: no error, exit 0
// when the rest matches, exit 1 on drift elsewhere.
func TestCheckSkipsHiddenSettings(t *testing.T) {
	t.Parallel()
	hidden := faults{hideMerge: true, hideSecurity: true, hideBypass: true}
	notices := []string{
		"notice: Merging: allow_squash_merge not checked: GitHub leaves it out of the response to this token",
		"notice: Merging: delete_branch_on_merge not checked: GitHub leaves it out of the response to this token",
		"notice: Security: secret scanning: secret_scanning not checked: GitHub leaves it out of the response to this token",
		"notice: Security: secret scanning: secret_scanning_push_protection not checked: GitHub leaves it out of the response to this token",
		`notice: Ruleset "main and release-*": bypass_actors not checked: GitHub leaves it out of the response to this token`,
		`notice: Ruleset "v* tag creation": bypass_actors not checked: GitHub leaves it out of the response to this token`,
		`notice: Ruleset "v* tag updates and deletion": bypass_actors not checked: GitHub leaves it out of the response to this token`,
	}
	r := run(t, desiredState(), hidden, true, "--check")
	wantCode(t, r, 0)
	for _, l := range notices {
		wantContains(t, r, "stdout", l+"\n")
	}
	if strings.Contains(r.stderr, "error(s)") {
		t.Errorf("--check with hidden settings reported errors:\n%s", r.stderr)
	}
	wantLines(t, "drift", r.drift(), nil)

	drifted := desiredState()
	drifted.DefaultWorkflowPermissions = "write"
	r = run(t, drifted, hidden, true, "--check")
	wantCode(t, r, 1)
	wantLines(t, "drift", r.drift(), []string{`drift: Actions: workflow token: default_workflow_permissions is "write", want "read"`})
}

// TestApplyContinuesPastAFailedArea: a refused write leaves its area as
// it was and every other area applied.
func TestApplyContinuesPastAFailedArea(t *testing.T) {
	t.Parallel()
	r := apply(t, freshState(), faults{fail: map[string]failure{
		"POST " + repoPath + "/rulesets": {http.StatusUnprocessableEntity, "Validation Failed"},
	}})
	wantCode(t, r, 2)
	wantState := desiredState()
	wantState.Rulesets, wantState.NextRulesetID = nil, 1
	if got, want := normalized(t, r.state), normalized(t, wantState); got != want {
		t.Errorf("settings after apply:\n%s\nwant\n%s", got, want)
	}
	wantContains(t, r, "stderr", "repo-settings: 3 error(s)\n")
}

func nonNil(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func linesWithPrefix(text, prefix string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

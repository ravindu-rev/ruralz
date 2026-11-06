// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package benchhost

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// workflow returns a workflow file of the repository.
func workflow(t *testing.T, name string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name)) //nolint:gosec // G304: a workflow of the repository
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// jobs splits a workflow's jobs section into its jobs' blocks by id.
func jobs(t *testing.T, wf string) map[string]string {
	t.Helper()
	_, rest, ok := strings.Cut(wf, "\njobs:\n")
	if !ok {
		t.Fatal("no jobs section")
	}
	out := map[string]string{}
	id := ""
	header := regexp.MustCompile(`^  ([a-z0-9-]+):$`)
	for line := range strings.SplitSeq(rest, "\n") {
		if m := header.FindStringSubmatch(line); m != nil {
			id = m[1]
			continue
		}
		if id != "" {
			out[id] += line + "\n"
		}
	}
	return out
}

// rh1Runner is the runs-on expression of a job that runs on rh-1.
const rh1Runner = `runs-on: ${{ fromJSON('["self-hosted", "rh-1"]') }}`

// rh1Only is the job condition of a job that runs only once
// RH1_PROVISIONED is true, alone or after the job's other conditions.
func rh1Only() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^    if: (.* && )?vars\.RH1_PROVISIONED == 'true'$`)
}

// TestRH1Jobs: the jobs on rh-1 are nightly's Latency job, the release
// soak job and chaos-scale.yml's job (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Reference hardware"); each runs alone in the rh-1 concurrency group,
// which never cancels a running job, for at most 180 minutes, first
// refuses an unprotected ref or a pull-request event, and runs only once
// RH1_PROVISIONED is true, from M6: until then GitHub skips it.
func TestRH1Jobs(t *testing.T) {
	want := map[string]string{"nightly.yml": "latency", "release.yml": "soak", "chaos-scale.yml": "chaos"}
	for file, id := range want {
		t.Run(file, func(t *testing.T) {
			all := jobs(t, workflow(t, file))
			var onRH1 []string
			for j, block := range all {
				if strings.Contains(block, `"rh-1"]`) {
					onRH1 = append(onRH1, j)
				}
			}
			if !slices.Equal(onRH1, []string{id}) {
				t.Fatalf("jobs on rh-1: %q, want %q", onRH1, id)
			}
			block := all[id]
			for _, s := range []string{
				rh1Runner,
				"    concurrency:\n      group: rh-1\n      cancel-in-progress: false\n",
				"    timeout-minutes: 180\n",
				"PROTECTED: ${{ github.ref_protected }}",
				`if [[ "$EVENT" == pull_request* || "$PROTECTED" != true ]]; then`,
			} {
				if !strings.Contains(block, s) {
					t.Errorf("job %s lacks %q", id, s)
				}
			}
			head, steps, _ := strings.Cut(block, "    steps:\n")
			if !rh1Only().MatchString(head) {
				t.Errorf("job %s does not run only once RH1_PROVISIONED is true:\n%s", id, head)
			}
			if strings.Contains(block, "RH-1 not provisioned") {
				t.Errorf("job %s fails until RH1_PROVISIONED is true instead of being skipped", id)
			}
			if !strings.HasPrefix(steps, "      - name: Protected ref\n") {
				t.Errorf("job %s does not check its ref first:\n%s", id, steps)
			}
		})
	}
}

// TestReleaseBaselineWithRH1: release.yml downloads the previous release
// for the Latency job's baseline only once RH1_PROVISIONED is true, and
// passes the baseline artifact only when that job made it.
func TestReleaseBaselineWithRH1(t *testing.T) {
	all := jobs(t, workflow(t, "release.yml"))
	if head, _, _ := strings.Cut(all["baseline"], "    steps:\n"); !rh1Only().MatchString(head) {
		t.Errorf("the baseline job does not run only once RH1_PROVISIONED is true:\n%s", head)
	}
	if want := "baseline-artifact: ${{ needs.baseline.result == 'success' && 'bench-baseline' || '' }}"; !strings.Contains(all["nightly"], want) {
		t.Errorf("the nightly call lacks %q", want)
	}
}

// TestChaosScaleWorkflow: chaos-scale.yml runs on demand and when
// release.yml calls it and runs `make chaos-scale` on the RH-1 inventory;
// its one job is the chaos job, since the run that started it, a
// maintainer's dispatch or the release run, reports a failure.
func TestChaosScaleWorkflow(t *testing.T) {
	wf := workflow(t, "chaos-scale.yml")
	for _, s := range []string{
		"\n  workflow_dispatch:\n", "\n  workflow_call:\n",
		"BENCH_PROFILE: ${{ github.workspace }}/deploy/benchhost/rh-1.json", "run: make chaos-scale",
	} {
		if !strings.Contains(wf, s) {
			t.Errorf("chaos-scale.yml lacks %q", s)
		}
	}
	if got := slices.Sorted(maps.Keys(jobs(t, wf))); !slices.Equal(got, []string{"chaos"}) {
		t.Errorf("chaos-scale.yml jobs = %q, want only chaos", got)
	}
}

// TestSchedules: release-verify.yml, the weekly re-verification of the
// newest release of each supported line, is the only workflow on a
// schedule. nightly.yml and chaos-scale.yml run on demand and in the
// release run, and image-checks.yml and t5-systemd.yml also on the pushes
// to main that change their inputs
// (docs/engineering/02-repository-layout-and-conventions.md, "CI
// stages").
func TestSchedules(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("workflows: %v, %v", files, err)
	}
	var scheduled []string
	for _, f := range files {
		if strings.Contains(workflow(t, filepath.Base(f)), "\n  schedule:\n") {
			scheduled = append(scheduled, filepath.Base(f))
		}
	}
	if !slices.Equal(scheduled, []string{"release-verify.yml"}) {
		t.Errorf("scheduled workflows = %q, want only release-verify.yml", scheduled)
	}
	for name, triggers := range map[string][]string{
		"nightly.yml":      {"workflow_dispatch", "workflow_call"},
		"chaos-scale.yml":  {"workflow_dispatch", "workflow_call"},
		"image-checks.yml": {"push", "workflow_dispatch", "workflow_call"},
		"t5-systemd.yml":   {"push", "workflow_dispatch", "workflow_call"},
	} {
		wf := workflow(t, name)
		for _, tr := range triggers {
			if !strings.Contains(wf, "\n  "+tr+":\n") {
				t.Errorf("%s lacks the %s trigger", name, tr)
			}
		}
	}
}

// TestNightlyRunnerOffline: once RH1_PROVISIONED is true, nightly's
// runner status job lists the runners with a read-only token on a
// GitHub-hosted runner and opens or updates the "RH-1 runner offline"
// nightly-failure issue when no online runner carries the rh-1 label; a
// Latency job canceled before a runner took it opens the same issue.
func TestNightlyRunnerOffline(t *testing.T) {
	all := jobs(t, workflow(t, "nightly.yml"))
	status := all["runner-status"]
	for _, s := range []string{
		"name: 11. RH-1 runner status", "if: vars.RH1_PROVISIONED == 'true'", "runs-on: ubuntu-latest",
		`select(.status == "online" and any(.labels[]; .name == "rh-1"))`, `title="RH-1 runner offline"`, "--label nightly-failure",
	} {
		if !strings.Contains(status, s) {
			t.Errorf("the runner status job lacks %q", s)
		}
	}
	issues := all["issues"]
	for _, s := range []string{
		"runner-status", "needs.latency.result == 'cancelled'", "LATENCY: ${{ needs.latency.result }}",
		`(.runner_name // "") == ""`, `upsert nightly-failure "RH-1 runner offline"`,
	} {
		if !strings.Contains(issues, s) {
			t.Errorf("the nightly issues job lacks %q", s)
		}
	}
}

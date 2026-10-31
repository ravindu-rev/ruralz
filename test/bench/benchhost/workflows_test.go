// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package benchhost

import (
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

// rh1Runner is the runs-on expression of a job that runs on rh-1 once
// RH1_PROVISIONED is true.
const rh1Runner = `runs-on: ${{ vars.RH1_PROVISIONED == 'true' && fromJSON('["self-hosted", "rh-1"]') || 'ubuntu-latest' }}`

// TestRH1Jobs: the jobs on rh-1 are nightly's Latency job, the release
// soak job and chaos-scale.yml's job (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Reference hardware"); each runs alone in the rh-1 concurrency group,
// which never cancels a running job, for at most 180 minutes, first
// refuses an unprotected ref or a pull-request event, and fails with an
// "RH-1 not provisioned" annotation until RH1_PROVISIONED is true.
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
				"        if: vars.RH1_PROVISIONED != 'true'\n",
				"title=RH-1 not provisioned::",
				"PROTECTED: ${{ github.ref_protected }}",
				`if [[ "$EVENT" == pull_request* || "$PROTECTED" != true ]]; then`,
			} {
				if !strings.Contains(block, s) {
					t.Errorf("job %s lacks %q", id, s)
				}
			}
			_, steps, _ := strings.Cut(block, "    steps:\n")
			if !strings.HasPrefix(steps, "      - name: Protected ref\n") {
				t.Errorf("job %s does not check its ref first:\n%s", id, steps)
			}
		})
	}
}

// TestChaosScaleWorkflow: chaos-scale.yml runs every Sunday at 07:17 UTC,
// on demand and when release.yml calls it, runs `make chaos-scale` on the
// RH-1 inventory, and opens or updates a nightly-failure issue only for a
// failed scheduled run, from a GitHub-hosted runner.
func TestChaosScaleWorkflow(t *testing.T) {
	wf := workflow(t, "chaos-scale.yml")
	for _, s := range []string{
		"  schedule:\n    - cron: \"17 7 * * 0\"\n", "\n  workflow_dispatch:\n", "\n  workflow_call:\n",
		"BENCH_PROFILE: ${{ github.workspace }}/deploy/benchhost/rh-1.json", "run: make chaos-scale",
	} {
		if !strings.Contains(wf, s) {
			t.Errorf("chaos-scale.yml lacks %q", s)
		}
	}
	issues := jobs(t, wf)["issues"]
	for _, s := range []string{
		"needs: chaos", "runs-on: ubuntu-latest", "issues: write", "--label nightly-failure",
		"if: failure() && github.workflow == 'chaos-scale' && github.event_name == 'schedule'",
	} {
		if !strings.Contains(issues, s) {
			t.Errorf("chaos-scale.yml's issues job lacks %q", s)
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

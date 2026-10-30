// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
)

// CrossCheckTolerance is the largest difference between vegeta's S1 p99
// and oha's pooled S1 p99 before the night's S1 comparison is
// inconclusive: 10% (target).
const CrossCheckTolerance = 0.10

// Driver runs an item with its own driver (O1, O2, C1, C2, R1, G1, the
// constants runs, PB-10 and the GOMEMLIMIT run), registered by the suites
// that define them.
type Driver func(ctx context.Context, n *Night, it Item) (Outcome, error)

// Night is one Latency job.
type Night struct {
	S       *Session
	State   State
	Plan    Plan
	Head    Build
	Drivers map[string]Driver
	// Catalog is the scenarios; default Catalog().
	Catalog []Scenario
	// Lengths override the benchmark lengths (loopback runs).
	Warmup, Duration time.Duration
	Pairs            int

	checker   *HostChecker
	reserve   time.Duration
	baselines map[string]*Build
	// release is the previous release's ruralzd, a release run's
	// baseline, read once.
	release *Build
	workers map[string]string
	headBin string
	// s1 is the night's pooled S1 head external p99, for the cross-check.
	s1       time.Duration
	outcomes map[string]Outcome
	failures []string
}

// NightResult is a night's outcome.
type NightResult struct {
	Outcomes map[string]Outcome
	// Failures are the night's failures, one line each; any opens the
	// nightly-failure issue.
	Failures []string
	// Next is the state for the next night.
	Next State
}

// Run runs every item of the plan in order. A host check failure stops
// the job at once with ErrHostCheck.
func (n *Night) Run(ctx context.Context) (*NightResult, error) {
	if n.Catalog == nil {
		n.Catalog = Catalog()
	}
	n.checker = NewHostChecker(n.S)
	n.reserve = ReserveMinutes * time.Minute
	n.baselines, n.workers, n.outcomes = map[string]*Build{}, map[string]string{}, map[string]Outcome{}
	for _, it := range n.Plan.Items {
		o, err := n.item(ctx, it)
		if errors.Is(err, ErrHostCheck) {
			return nil, err
		}
		if err != nil {
			n.failures = append(n.failures, it.ID+": "+err.Error())
			o = Failed
		}
		if _, set := n.outcomes[it.ID]; !set || o != Passed {
			n.outcomes[it.ID] = o
		}
	}
	next, more := Advance(n.State, n.Plan, n.outcomes, n.Head.Commit)
	return &NightResult{Outcomes: n.outcomes, Failures: append(n.failures, more...), Next: next}, nil
}

// Check runs the pre-run host checks before a driver's scenario, with the
// night's checker, which keeps each host's previous facts; a failure wraps
// ErrHostCheck, which stops the job.
func (n *Night) Check(ctx context.Context, sc Scenario) error { return n.checker.Check(ctx, sc) }

// Rerun spends the night's reserve on one re-run of a driver's invalid
// single run, as for a scenario's; false when the reserve is spent.
func (n *Night) Rerun() bool { return n.rerun() }

// rerun spends the reserve on one re-run of an invalid single run.
func (n *Night) rerun() bool {
	cost := n.Warmup + n.Duration
	if cost == 0 {
		cost = Warmup + Measured
	}
	if n.reserve < cost && !(n.Plan.Slot == 0) {
		return false
	}
	n.reserve -= cost
	return true
}

// baseline returns the baseline build of scenario key: the previous
// release's ruralzd in a release run, else the commit of the scenario's
// last valid passing run built with its own toolchain; nil without one.
func (n *Night) baseline(ctx context.Context, key string) (*Build, error) {
	st := n.S.Settings
	if n.Plan.Slot == 0 {
		if st.BaselineRuralzd == "" {
			return nil, nil
		}
		if n.release == nil {
			b, err := ReleaseBuild(st.BaselineRuralzd, st.BaselineVersion)
			if err != nil {
				return nil, err
			}
			n.release = b
		}
		return n.release, nil
	}
	commit, ok := n.State.Baselines[key]
	if !ok {
		return nil, nil
	}
	if b, ok := n.baselines[commit]; ok {
		return b, nil
	}
	dir := filepath.Join(n.S.Work, "baseline-"+commit)
	remove, err := Worktree(ctx, n.S.RepoRoot, dir, commit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = remove() }()
	b, err := BuildTree(ctx, dir, filepath.Join(n.S.Work, "bin-"+commit), true)
	if err != nil {
		return nil, err
	}
	if err := BuildHarnessTest(ctx, dir, filepath.Join(n.S.Work, "worker-"+commit)); err == nil {
		n.workers[commit] = filepath.Join(n.S.Work, "worker-"+commit)
	}
	n.baselines[commit] = &b
	return &b, nil
}

// ReleaseBuild describes the previous release's verified ruralzd, the
// baseline of a release run, from the binary's Go build information: the
// commit it was built from and its toolchain, so the baseline runs and the
// comparison record the previous release, not the head. The binary must
// be the release version: its module version is the release tag.
func ReleaseBuild(bin, version string) (*Build, error) {
	info, err := buildinfo.ReadFile(bin)
	if err != nil {
		return nil, fmt.Errorf("harness: the baseline ruralzd %s: %w", bin, err)
	}
	return releaseBuildOf(bin, version, info)
}

// releaseBuildOf is ReleaseBuild over read build information.
func releaseBuildOf(bin, version string, info *buildinfo.BuildInfo) (*Build, error) {
	rev := ""
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			rev = s.Value
		}
	}
	if !commitPattern().MatchString(rev) {
		return nil, fmt.Errorf("harness: the baseline ruralzd %s records no commit (vcs.revision %q): it is no release build", bin, rev)
	}
	if info.Main.Version != "v"+version {
		return nil, fmt.Errorf("harness: the baseline ruralzd %s is module version %q, not the release v%s", bin, info.Main.Version, version)
	}
	return &Build{Ruralzd: bin, Commit: rev, Version: version, Toolchain: info.GoVersion}, nil
}

// item runs one item.
func (n *Night) item(ctx context.Context, it Item) (Outcome, error) {
	switch it.Kind {
	case KindComponents:
		return n.components(ctx)
	case KindDriver:
		d, ok := n.Drivers[it.ID]
		if !ok {
			return Failed, fmt.Errorf("no driver for %s yet", it.ID)
		}
		return d(ctx, n, it)
	}
	sc, ok := Find(n.Catalog, it.Scenario)
	if !ok {
		return Failed, fmt.Errorf("scenario %s is not in the catalog", it.Scenario)
	}
	if err := n.checker.Check(ctx, sc); err != nil {
		return Failed, err
	}
	rev, err := Render(ctx, n.Head.Ruralz, n.S.RepoRoot, sc, n.S.Profile)
	if err != nil {
		return Failed, err
	}
	switch it.Kind {
	case KindAB:
		base, err := n.baseline(ctx, sc.ID)
		if err != nil {
			return Failed, err
		}
		res, err := n.S.Compare(ctx, CompareSpec{
			Scenario: sc, Head: n.Head, Baseline: base, Revision: rev,
			Warmup: n.Warmup, Duration: n.Duration, Pairs: n.Pairs, Rerun: n.rerun,
		})
		if err != nil {
			return Failed, err
		}
		if sc.ID == "S1" {
			n.s1 = res.Summary.Summary.Head.External.P99
		}
		return verdictOutcome(res), nil
	case KindLadder:
		rec, err := n.S.Ladder(ctx, LadderSpec{Scenario: sc, Build: n.Head, Revision: rev})
		if err != nil {
			return Failed, err
		}
		return budgetsOutcome(rec), nil
	case KindSingle:
		var runs []Scenario
		switch it.ID {
		case "S5-zipf":
			runs = []Scenario{sc.Zipf()}
		case "S2-pb15-pb16":
			a, b := sc.AtRate(0.6*sc.Saturation, pb15()), sc.AtRate(0.8*sc.Saturation, pb16())
			a.ID, b.ID = sc.ID+"-pb15", sc.ID+"-pb16"
			runs = []Scenario{a, b}
		default:
			return Failed, fmt.Errorf("no single run named %s", it.ID)
		}
		out := Passed
		for _, r := range runs {
			o, err := n.single(ctx, r, rev)
			if err != nil {
				return Failed, err
			}
			out = worse(out, o)
		}
		return out, nil
	case KindCrossCheck:
		v := sc.VegetaCrossCheck()
		res, err := n.S.Run(ctx, RunSpec{Scenario: v, Build: n.Head, Revision: rev, Name: "run-1", Warmup: n.Warmup, Duration: n.Duration})
		if err != nil {
			return Failed, err
		}
		if n.s1 == 0 {
			return Failed, errors.New("no S1 comparison ran tonight to cross-check")
		}
		diff := math.Abs(float64(res.Record.Latency.External.P99-n.s1)) / float64(n.s1)
		if diff > CrossCheckTolerance {
			n.outcomes["S1"] = Inconclusive
			return Failed, fmt.Errorf("vegeta's S1 p99 %s differs from oha's pooled %s by %.1f%%, above %.0f%%: S1 is inconclusive",
				res.Record.Latency.External.P99, n.s1, diff*100, CrossCheckTolerance*100)
		}
		return Passed, nil
	default:
		return Failed, fmt.Errorf("unknown item kind %q", it.Kind)
	}
}

// single runs one run judged on absolute criteria, re-running an invalid
// run once from the reserve.
func (n *Night) single(ctx context.Context, sc Scenario, rev Revision) (Outcome, error) {
	spec := RunSpec{Scenario: sc, Build: n.Head, Revision: rev, Name: "run-1", Warmup: n.Warmup, Duration: n.Duration}
	out, err := n.S.Run(ctx, spec)
	if err == nil && !out.Valid && n.rerun() {
		spec.Name = "run-1-rerun"
		out, err = n.S.Run(ctx, spec)
	}
	if err != nil {
		return Failed, err
	}
	if !out.Valid {
		return Inconclusive, nil
	}
	return budgetsOutcome(out.Record), nil
}

// components runs every component benchmark against its baseline.
func (n *Night) components(ctx context.Context) (Outcome, error) {
	if n.headBin == "" {
		n.headBin = filepath.Join(n.S.Work, "worker-head")
		if err := BuildHarnessTest(ctx, n.S.RepoRoot, n.headBin); err != nil {
			return Failed, err
		}
	}
	out := Passed
	for _, c := range Components() {
		key := "component-" + c.Name
		base, err := n.baseline(ctx, key)
		if err != nil {
			return Failed, err
		}
		cs := ComponentSpec{Component: c, Head: n.Head, HeadBin: n.headBin, Pairs: n.Pairs}
		if base != nil {
			if bin, ok := n.workers[n.State.Baselines[key]]; ok {
				cs.Baseline, cs.BaselineBin = base, bin
			}
		}
		res, err := n.S.CompareComponent(ctx, cs)
		if err != nil {
			return Failed, err
		}
		o := verdictOutcome(res)
		if o == Passed && n.Plan.Slot != 0 {
			n.State.Baselines[key] = n.Head.Commit
		}
		out = worse(out, o)
	}
	return out, nil
}

func verdictOutcome(r *CompareResult) Outcome {
	switch {
	case r.Requeue:
		return Inconclusive
	case r.Pass:
		return Passed
	default:
		return Failed
	}
}

func budgetsOutcome(rec *benchrecord.Record) Outcome {
	if AnyFailed(rec.Budgets) {
		return Failed
	}
	return Passed
}

// worse returns the worse outcome: Failed over Inconclusive over Passed.
func worse(a, b Outcome) Outcome {
	rank := func(o Outcome) int {
		switch o {
		case Failed:
			return 2
		case Inconclusive:
			return 1
		default:
			return 0
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

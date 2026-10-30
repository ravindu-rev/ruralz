// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// Bundle directories under harness.ScenarioRoot that the catalog's
// lower-case scenario IDs do not name.
const (
	DirS3             = "s3"
	DirO2             = "o2"
	DirS2TelemetryOff = "s2-telemetry-off"
	DirS2TraceCaps    = "s2-trace-caps"
	DirConstants      = "constants"
	DirO1Functional   = "o1-functional"
	DirO2Functional   = "o2-functional"
)

// Path returns the repository-relative path of a Bundle directory.
func Path(dir string) string { return harness.ScenarioRoot + "/" + dir }

// EnvironmentsFile is the host profiles' environments file.
func EnvironmentsFile() string { return harness.ScenarioRoot + "/environments.yaml" }

// Profiles returns the host profiles, the Environments of
// environments.yaml.
func Profiles() []string {
	return []string{harness.ProfileLoopback, harness.ProfileRH1, harness.ProfileRH3}
}

// FunctionalDirs returns the Bundle directories only the functional
// variants load, which hold a loopback overlay only.
func FunctionalDirs() []string { return []string{DirO1Functional, DirO2Functional} }

// Item IDs of the Latency job's driver items (harness.SlotItems).
const (
	ItemO1         = "O1"
	ItemO2         = "O2"
	ItemC1         = "C1"
	ItemC2         = "C2"
	ItemR1         = "R1"
	ItemG1         = "G1"
	ItemConstants  = "constants"
	ItemPB10       = "PB-10"
	ItemGOMEMLIMIT = "S2-gomemlimit"
)

// DriverItems returns every driver item ID, in slot order.
func DriverItems() []string {
	return []string{ItemO1, ItemC2, ItemO2, ItemG1, ItemC1, ItemPB10, ItemGOMEMLIMIT, ItemR1, ItemConstants}
}

// Env is what a driver runs with.
type Env struct {
	S    *harness.Session
	Head harness.Build
	// Check runs the pre-run host checks before a scenario; nil runs none.
	Check func(context.Context, harness.Scenario) error
	// Rerun spends the reserve on one re-run of an invalid run; nil never
	// re-runs.
	Rerun func() bool
	// Warmup and Duration override the Scale's run lengths when set (the
	// Night's loopback lengths).
	Warmup, Duration time.Duration
}

// check runs the host checks for sc.
func (e Env) check(ctx context.Context, sc harness.Scenario) error {
	if e.Check == nil {
		return nil
	}
	return e.Check(ctx, sc)
}

// rerun reports whether an invalid run may run again.
func (e Env) rerun() bool { return e.Rerun != nil && e.Rerun() }

// FileReserve is the open files a run's process holds beyond its
// connections: listeners, the admin API, logs, the State Store and the
// lab services' own sockets.
const FileReserve = 1000

// needFiles fails a run before it starts when one of its processes on the
// harness's host holds conns connections, plus FileReserve, beyond the
// host's open-file limit: O1's Node and mock hold 20,000 upstream
// connections, C1's Node its idle ones (PBB "Reference hardware":
// LimitNOFILE for every benchmark process).
func (e Env) needFiles(run string, conns int) error {
	return e.S.CheckOpenFiles(run, conns+FileReserve)
}

// Result is a driver's outcome: its verdict and the criteria results its
// records hold.
type Result struct {
	Outcome harness.Outcome
	// Budgets are the criteria results of every run, in run order.
	Budgets []benchrecord.BudgetResult
	// Records are the records written, as <scenario>/<file>.
	Records []string
	// Invalid says why a run was invalid after its re-run; such a run
	// makes the item inconclusive.
	Invalid []string
}

// add appends a run's results.
func (r *Result) add(scenario, path string, budgets []benchrecord.BudgetResult) {
	r.Budgets = append(r.Budgets, budgets...)
	if path != "" {
		r.Records = append(r.Records, scenario+"/"+path)
	}
}

// finish sets the outcome: a failed criterion fails the item, an invalid
// run makes it inconclusive, otherwise it passed.
func (r *Result) finish() *Result {
	switch {
	case harness.AnyFailed(r.Budgets):
		r.Outcome = harness.Failed
	case len(r.Invalid) > 0:
		r.Outcome = harness.Inconclusive
	default:
		r.Outcome = harness.Passed
	}
	return r
}

// Find returns the result of criterion id.
func (r *Result) Find(id string) (benchrecord.BudgetResult, bool) {
	for _, b := range r.Budgets {
		if b.ID == id {
			return b, true
		}
	}
	return benchrecord.BudgetResult{}, false
}

// Failures returns each failed criterion, one line each.
func (r *Result) Failures() []string {
	var out []string
	for _, b := range r.Budgets {
		if b.Verdict == benchrecord.BudgetFail {
			out = append(out, b.ID+": "+b.Result+", want "+b.Value)
		}
	}
	return out
}

// judge returns a pass or fail result.
func judge(id, value string, pass bool, result string) benchrecord.BudgetResult {
	v := benchrecord.BudgetFail
	if pass {
		v = benchrecord.BudgetPass
	}
	if result == "" {
		result = "none"
	}
	return benchrecord.BudgetResult{ID: id, Value: value, Result: result, Verdict: v}
}

// skipped returns a skipped result saying why.
func skipped(id, value, why string) benchrecord.BudgetResult {
	return benchrecord.BudgetResult{ID: id, Value: value, Result: why, Verdict: benchrecord.BudgetSkipped}
}

// onlyRH1 is the reason a functional run records for a criterion that
// holds only on the reference hardware.
const onlyRH1 = "judged on RH-1 only: it does not hold on every host"

// hardware returns r on a Full scale, and r skipped on a Functional one:
// latencies, CPU and memory sizes hold only on the reference hardware.
func (s Scale) hardware(r benchrecord.BudgetResult) benchrecord.BudgetResult {
	if s.Functional {
		return skipped(r.ID, r.Value, onlyRH1)
	}
	return r
}

// withSLO sets r's SLO.
func withSLO(r benchrecord.BudgetResult, slo string) benchrecord.BudgetResult {
	r.SLO = slo
	return r
}

// Drivers returns the Latency job's drivers at scale s, keyed by item ID,
// for harness.Night.Drivers.
func Drivers(s Scale) map[string]harness.Driver {
	wrap := func(run func(context.Context, Env, Scale) (*Result, error)) harness.Driver {
		return func(ctx context.Context, n *harness.Night, _ harness.Item) (harness.Outcome, error) {
			r, err := run(ctx, Env{S: n.S, Head: n.Head, Check: n.Check, Rerun: n.Rerun, Warmup: n.Warmup, Duration: n.Duration}, s)
			if err != nil {
				return harness.Failed, err
			}
			if r.Outcome == harness.Failed {
				return harness.Failed, errors.New(strings.Join(r.Failures(), "; "))
			}
			return r.Outcome, nil
		}
	}
	return map[string]harness.Driver{
		ItemO1:         wrap(RunO1),
		ItemO2:         wrap(RunO2),
		ItemC1:         wrap(RunC1),
		ItemC2:         wrap(RunC2),
		ItemR1:         wrap(RunR1),
		ItemG1:         wrap(RunG1),
		ItemConstants:  wrap(RunConstants),
		ItemPB10:       wrap(RunPB10),
		ItemGOMEMLIMIT: wrap(RunGOMEMLIMIT),
	}
}

// lengths returns a single run's warm-up and measured phase and whether
// the warm-up settles: the Env's override, else the Scale's, else the
// benchmark lengths (60 s settled and 180 s).
func (e Env) lengths(s Scale) (warm, dur time.Duration, noSettle bool) {
	warm, dur = s.Warmup, s.Duration
	if e.Warmup != 0 {
		warm = e.Warmup
	}
	if e.Duration != 0 {
		dur = e.Duration
	}
	return warm, dur, warm != 0
}

// scenario returns a driver scenario over a Bundle directory.
func scenario(id, dir string, w harness.Workload, saturation float64) harness.Scenario {
	return harness.Scenario{
		ID: id, Bundle: Path(dir), Environments: EnvironmentsFile(), Workload: w, Saturation: saturation,
	}
}

// catalog returns a scenario of the harness's catalog.
func catalog(id string) (harness.Scenario, error) {
	sc, ok := harness.Find(harness.Catalog(), id)
	if !ok {
		return harness.Scenario{}, fmt.Errorf("scenarios: %s is not in the harness catalog", id)
	}
	return sc, nil
}

// run renders sc's Revision, runs spec on it and re-runs an invalid run
// once from the reserve; the record's criteria come from spec.Criteria.
func (e Env) run(ctx context.Context, sc harness.Scenario, spec harness.RunSpec, res *Result) (*harness.RunOutcome, error) {
	if err := e.check(ctx, sc); err != nil {
		return nil, err
	}
	rev, err := harness.Render(ctx, e.Head.Ruralz, e.S.RepoRoot, sc, e.S.Profile)
	if err != nil {
		return nil, err
	}
	spec.Scenario, spec.Build, spec.Revision = sc, e.Head, rev
	if spec.Name == "" {
		spec.Name = "run-1"
	}
	out, err := e.S.Run(ctx, spec)
	if err == nil && !out.Valid && e.rerun() {
		spec.Name += "-rerun"
		out, err = e.S.Run(ctx, spec)
	}
	if err != nil {
		return nil, err
	}
	res.add(sc.ID, out.Path, criteriaOf(out.Record))
	if !out.Valid {
		res.Invalid = append(res.Invalid, sc.ID+": "+strings.Join(out.Record.Validity.Reasons, "; "))
	}
	return out, nil
}

// criteriaOf returns a record's budget results.
func criteriaOf(rec *benchrecord.Record) []benchrecord.BudgetResult {
	if rec == nil {
		return nil
	}
	return slices.Clone(rec.Budgets)
}

// sampleEvery is the Node sampling interval of a driver run whose criteria
// read gauges over the measured window: fine enough that a plateau shows
// in several samples.
const sampleEvery = time.Second

// readRecord reads tonight's record name of scenario from the session's
// records, the way a later item reads an earlier one's (PB-10, O2).
func readRecord(s *harness.Session, scenario, name string) (*benchrecord.Record, error) {
	path := filepath.Join(s.Rec.Dir, scenario, name)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("scenarios: no %s record of %s tonight: %w", name, scenario, err)
	}
	return s.Rec.ReadRecord(scenario, name)
}

// codesOf returns the load's count of RZ code c.
func codesOf(r *lab.LoadResult, c string) int64 {
	if r == nil {
		return 0
	}
	return r.Codes[c]
}

// RHSecretRoot is the fixed secret root of the RH-1 and RH-3 profiles: their
// overlays name the run's TLS files under it, and their inventories in
// deploy/benchhost set it as the Node's RURALZ_SECRET_ROOT.
const RHSecretRoot = "/var/lib/ruralz-bench/secrets"

// s2Intro starts the sentence that introduces the S2 Bundle block of
// docs/architecture/12-performance-budgets-and-benchmarking.md.
const s2Intro = "The S2 Bundle ("

// S2Block returns the S2 Bundle block of
// docs/architecture/12-performance-budgets-and-benchmarking.md "Scenarios",
// which test/bench/scenarios/s2/ruralz.yaml equals byte for byte: the body
// of the first yaml block after the sentence that introduces it.
func S2Block(doc []byte) (string, error) {
	s := string(doc)
	i := strings.Index(s, "\n"+s2Intro)
	if i < 0 && strings.HasPrefix(s, s2Intro) {
		i = 0
	}
	if i < 0 {
		return "", errors.New("scenarios: the document has no sentence introducing the S2 Bundle")
	}
	rest := s[i:]
	const fence = "\n```yaml\n"
	open := strings.Index(rest, fence)
	if open < 0 {
		return "", errors.New("scenarios: no yaml block follows the S2 Bundle's sentence")
	}
	body := rest[open+len(fence):]
	end := strings.Index(body, "\n```")
	if end < 0 {
		return "", errors.New("scenarios: the S2 Bundle's yaml block does not end")
	}
	return body[:end+1], nil
}

// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
)

// Loopback builds the release ruralzd and ruralz of the tree at root and
// returns an Env on the loopback profile whose session writes its records
// into records: the pr-full smoke run's and the nightly Functional job's,
// which drive the release build in real time on one host (PBB "Suite
// layers"; docs/engineering/03-testing-and-quality-strategy.md "Hermetic
// rule scope").
func Loopback(ctx context.Context, root, work, records string) (Env, error) {
	head, err := harness.BuildTree(ctx, root, filepath.Join(work, "bin"), false)
	if err != nil {
		return Env{}, err
	}
	s, err := harness.NewSession(ctx, harness.Loopback(work), harness.Settings{Records: records}, root, filepath.Join(work, "session"))
	if err != nil {
		return Env{}, err
	}
	return Env{S: s, Head: head}, nil
}

// AtScale returns catalog scenario sc offered at s's reduced scale with
// loadgen, which drives the smoke run and the functional variants, and
// without the catalog's budgets, which hold only on RH-1.
func AtScale(sc harness.Scenario, rate float64) harness.Scenario {
	sc.Workload.Tool, sc.Workload.Rate = harness.ToolLoadgen, rate
	sc.Budgets = nil
	return sc
}

// RunScenario runs catalog scenario sc once at s's lengths and returns its
// result: a run that served every offered request passes.
func RunScenario(ctx context.Context, e Env, s Scale, sc harness.Scenario) (*Result, error) {
	res := &Result{}
	warm, dur, noSettle := e.lengths(s)
	err := e.run(ctx, sc, harness.RunSpec{
		Warmup: warm, Duration: dur, NoSettle: noSettle,
		Criteria: func(o *harness.RunOutcome) []benchrecord.BudgetResult {
			return []benchrecord.BudgetResult{served(sc.ID+"-served", o)}
		},
	}, res)
	if err != nil {
		return nil, err
	}
	return res.finish(), nil
}

// served judges a run that answered every offered request with a 2xx
// response.
func served(id string, o *harness.RunOutcome) benchrecord.BudgetResult {
	return judge(id, "every offered request served", o.Load != nil && o.Load.Offered > 0 && failedRequests(o.Load) == 0,
		fmt.Sprintf("%d of %d failed", failedRequests(o.Load), offered(o.Load)))
}

// CheckRecords reads every record under dir and returns one line per
// problem: a record that does not read back valid, or a macro record of a
// run that served no request.
func CheckRecords(dir string) ([]string, int, error) {
	var problems []string
	n := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		n++
		// A record that does not read back is a problem, not a walk error.
		switch rec, rerr := benchrecord.ReadFile(path); {
		case rerr != nil:
			problems = append(problems, path+": "+rerr.Error())
		case rec.Identity.Kind == benchrecord.KindMacro && !(rec.Load.AchievedRate > 0):
			problems = append(problems, path+": the run served nothing")
		}
		return nil
	})
	slices.Sort(problems)
	return problems, n, err
}
